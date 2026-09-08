package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	appName            = "Codex Remote Win"
	appVersion         = "0.11.0-beta.6"
	appIconResourceID  = 1
	defaultHost        = "0.0.0.0"
	defaultPort        = "8787"
	defaultCDPPort     = "9222"
	maxBodyBytes       = 32 * 1024 * 1024
	maxTailBytes       = 5 * 1024 * 1024
	maxHistory         = 100
	maxTextLength      = 8000
	maxAttachments     = 6
	maxAttachmentBytes = 12 * 1024 * 1024
	sessionTTL         = 12 * time.Hour
)

var threadIDPattern = regexp.MustCompile(`(?i)([a-f0-9]{8}-[a-f0-9-]{27,})\.jsonl$`)

//go:embed assets/codex-remote-icon-192.png
var appIcon192PNG []byte

//go:embed assets/codex-remote-icon-512.png
var appIcon512PNG []byte

//go:embed web/v11.js
var webV11 string

//go:embed web/theme.css
var webTheme string

func projectPage() string {
	page := indexHTMLProjects
	if i := strings.LastIndex(page, "boot();"); i >= 0 {
		page = page[:i] + page[i+len("boot();"):]
	}
	page = strings.Replace(page, "v0.10</span>", "v0.11</span>", 1)
	themeHead := `<script>(function(){var t="dark";try{var s=localStorage.getItem("crw.theme");t=s==="light"||s==="dark"?s:(matchMedia("(prefers-color-scheme: light)").matches?"light":"dark")}catch(e){}document.documentElement.dataset.theme=t;var m=document.querySelector('meta[name="theme-color"]');if(m)m.content=t==="light"?"#f5f7fa":"#111315"})()</script><link rel="stylesheet" href="/theme.css">`
	page = strings.Replace(page, "</head>", themeHead+"</head>", 1)
	return strings.Replace(page, "</body>", `<script src="/v11.js"></script></body>`, 1)
}

type session struct {
	TokenHash string    `json:"tokenHash"`
	Device    string    `json:"device"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	IP        string    `json:"ip"`
}

type threadRow struct {
	ID          string       `json:"id"`
	Title       string       `json:"title"`
	Preview     string       `json:"preview"`
	Cwd         string       `json:"cwd"`
	SessionFile string       `json:"sessionFile"`
	UpdatedAt   string       `json:"updatedAt"`
	Active      bool         `json:"active"`
	Status      string       `json:"status"`
	Pinned      bool         `json:"pinned"`
	Archived    bool         `json:"archived"`
	ProjectKey  string       `json:"projectKey"`
	ProjectName string       `json:"projectName"`
	Context     contextUsage `json:"context"`
	mtime       time.Time
}

type messageRow struct {
	Seq         int            `json:"seq"`
	Role        string         `json:"role"`
	Text        string         `json:"text"`
	Timestamp   string         `json:"timestamp"`
	Attachments []uploadRecord `json:"attachments,omitempty"`
}

type attachmentInput struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	DataURL string `json:"dataUrl"`
}

type savedAttachment struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Kind string `json:"kind"`
	Size int    `json:"size"`
	Path string `json:"-"`
}

type serverState struct {
	mu       sync.Mutex
	sendMu   sync.Mutex
	sessions map[string]session
	trusted  map[string]session
	pairCode string
	host     string
	port     string
	home     string
	dataDir  string
	audit    *log.Logger
	recentMu sync.Mutex
	recent   map[string]sendReceipt
	cacheMu  sync.Mutex
	threads  map[string]threadCacheEntry
	desktop  desktopCaller
}

type threadCacheEntry struct {
	Row        threadRow
	FileSize   int64
	FileMTime  int64
	StateMTime int64
	IndexMTime int64
}

type sendReceipt struct {
	RequestID   string            `json:"requestId"`
	Queued      bool              `json:"queued"`
	SentAt      string            `json:"sentAt"`
	Attachments []savedAttachment `json:"attachments"`
	createdAt   time.Time
}

type statusStep struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Detail    string `json:"detail,omitempty"`
	Timestamp string `json:"timestamp"`
}

type appLocalState struct {
	PinnedThreadIDs   []string                     `json:"pinnedThreadIds"`
	ArchivedThreadIDs []string                     `json:"archivedThreadIds"`
	TitleOverrides    map[string]threadTitleRecord `json:"titleOverrides"`
}

type threadTitleRecord struct {
	Name      string `json:"name"`
	RenamedAt string `json:"renamedAt"`
}

type contextUsage struct {
	Available       bool    `json:"available"`
	UsedTokens      int     `json:"usedTokens"`
	WindowTokens    int     `json:"windowTokens"`
	RemainingTokens int     `json:"remainingTokens"`
	Percent         float64 `json:"percent"`
	UpdatedAt       string  `json:"updatedAt"`
}

func main() {
	appID := syscall.StringToUTF16Ptr("CodexRemoteWin.Desktop")
	procSetCurrentProcessAppID.Call(uintptr(unsafe.Pointer(appID)))
	home, _ := os.UserHomeDir()
	host := env("CODEX_REMOTE_HOST", defaultHost)
	port := env("CODEX_REMOTE_PORT", defaultPort)
	pairCode := env("CODEX_REMOTE_PAIR_CODE", mustPairCode())

	dataDir := filepath.Join(exeDir(), "data")
	_ = os.MkdirAll(dataDir, 0755)
	auditFile, err := os.OpenFile(filepath.Join(dataDir, "audit.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Println("Cannot open audit log:", err)
		return
	}
	defer auditFile.Close()

	state := &serverState{
		sessions: map[string]session{},
		trusted:  map[string]session{},
		pairCode: pairCode,
		host:     host,
		port:     port,
		home:     home,
		dataDir:  dataDir,
		audit:    log.New(auditFile, "", 0),
		recent:   map[string]sendReceipt{},
		threads:  map[string]threadCacheEntry{},
	}
	state.loadTrusted()
	state.desktop = &nativeDesktop{contextID: state.desktopContextID()}

	mux := http.NewServeMux()
	mux.HandleFunc("/", state.handle)

	addr := net.JoinHostPort(host, port)
	localURL := "http://" + net.JoinHostPort(displayHost(host), port)

	fmt.Println()
	fmt.Println(appName + " is running.")
	fmt.Println("Local URL: " + localURL)
	for _, url := range lanURLs(port) {
		fmt.Println("LAN URL:   " + url)
	}
	fmt.Println("Pairing code: " + pairCode)
	fmt.Println()
	fmt.Println("Keep this window open. For OpenWrt FRP, forward the Windows LAN URL above.")
	fmt.Println("Put HTTPS plus another auth layer in front of the public FRP endpoint.")
	fmt.Println()

	server := &http.Server{Addr: addr, Handler: mux}
	serverErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			state.auditEvent("server_stopped", map[string]any{"error": err.Error()})
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		showMessage(appName, "Could not start server on "+addr+".\n\n"+err.Error()+"\n\nExit the old Codex Remote Win tray process first, then run this version again.")
		return
	case <-time.After(450 * time.Millisecond):
	}

	_ = openBrowser(localURL)

	if err := runTray(state, localURL); err != nil {
		state.auditEvent("tray_failed", map[string]any{"error": err.Error()})
		select {}
	}
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func displayHost(host string) string {
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "127.0.0.1"
	}
	return host
}

func lanURLs(port string) []string {
	var urls []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return urls
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.To4() == nil {
				continue
			}
			urls = append(urls, "http://"+net.JoinHostPort(ip.String(), port))
		}
	}
	sort.Strings(urls)
	return urls
}

func mustPairCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		return fmt.Sprintf("%06d", time.Now().UnixNano()%900000+100000)
	}
	return fmt.Sprintf("%06d", n.Int64()+100000)
}

func randomToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (s *serverState) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	path := r.URL.Path
	if path == "/" || path == "/index.html" {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		w.Header().Set("cache-control", "no-store")
		_, _ = io.WriteString(w, projectPage())
		return
	}
	if path == "/manifest.webmanifest" {
		w.Header().Set("content-type", "application/manifest+json; charset=utf-8")
		w.Header().Set("cache-control", "no-store")
		_, _ = io.WriteString(w, manifestJSON)
		return
	}
	if path == "/v11.js" {
		w.Header().Set("content-type", "application/javascript; charset=utf-8")
		w.Header().Set("cache-control", "no-store")
		_, _ = io.WriteString(w, webV11)
		return
	}
	if path == "/theme.css" {
		w.Header().Set("content-type", "text/css; charset=utf-8")
		w.Header().Set("cache-control", "no-store")
		_, _ = io.WriteString(w, webTheme)
		return
	}
	if path == "/icon-192.png" || path == "/icon-512.png" || path == "/icon-blue-192.png" || path == "/icon-blue-512.png" {
		w.Header().Set("content-type", "image/png")
		w.Header().Set("cache-control", "public, max-age=604800")
		if path == "/icon-192.png" || path == "/icon-blue-192.png" {
			_, _ = w.Write(appIcon192PNG)
		} else {
			_, _ = w.Write(appIcon512PNG)
		}
		return
	}
	if strings.HasPrefix(path, "/api/") {
		s.handleAPI(w, r)
		return
	}
	http.NotFound(w, r)
}

func (s *serverState) handleAPI(w http.ResponseWriter, r *http.Request) {
	if s.handleV11API(w, r) {
		return
	}
	switch r.URL.Path {
	case "/api/health":
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"service":   appName,
			"version":   appVersion,
			"host":      hostname(),
			"bind":      s.host,
			"port":      s.port,
			"lanUrls":   lanURLs(s.port),
			"transport": "desktop-app-tools",
			"now":       time.Now().Format(time.RFC3339),
		})
	case "/api/pair":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, errJSON("METHOD_NOT_ALLOWED", "Use POST."))
			return
		}
		s.handlePair(w, r)
	default:
		sess, ok := s.requireSession(w, r)
		if !ok {
			return
		}
		switch r.URL.Path {
		case "/api/logout":
			s.mu.Lock()
			delete(s.sessions, sess.TokenHash)
			delete(s.trusted, sess.TokenHash)
			s.saveTrustedLocked()
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		case "/api/threads":
			includeArchived := r.URL.Query().Get("includeArchived") == "1"
			rows := s.listThreads(includeArchived)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "threads": rows, "projects": projectSummary(rows)})
		case "/api/diagnostics":
			writeJSON(w, http.StatusOK, s.diagnostics())
		case "/api/history":
			id := r.URL.Query().Get("thread")
			if !validThreadID(id) {
				writeJSON(w, http.StatusBadRequest, errJSON("BAD_THREAD_ID", "Invalid thread id."))
				return
			}
			limit := queryInt(r, "limit", 80, 20, 160)
			before := queryInt(r, "before", 0, 0, 1<<30)
			messages, available, hasMore, nextBefore := s.historyPage(id, before, limit)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "available": available, "threadId": id, "messages": messages, "hasMore": hasMore, "nextBefore": nextBefore})
		case "/api/status":
			id := r.URL.Query().Get("thread")
			if !validThreadID(id) {
				writeJSON(w, http.StatusBadRequest, errJSON("BAD_THREAD_ID", "Invalid thread id."))
				return
			}
			row, items, ok := s.threadStatusData(id)
			if !ok {
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "available": false, "threadId": id, "active": false, "status": "missing"})
				return
			}
			steps, model, reasoning := statusDetailsFromItems(items)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "available": true, "thread": row, "active": row.Active, "status": row.Status, "preview": row.Preview, "steps": steps, "model": model, "reasoning": reasoning})
		case "/api/new-thread":
			if r.Method != http.MethodPost {
				writeJSON(w, http.StatusMethodNotAllowed, errJSON("METHOD_NOT_ALLOWED", "Use POST."))
				return
			}
			if err := openBrowser("codex://threads/new"); err != nil {
				writeJSON(w, http.StatusInternalServerError, errJSON("NEW_THREAD_FAILED", err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		case "/api/thread-action":
			if r.Method != http.MethodPost {
				writeJSON(w, http.StatusMethodNotAllowed, errJSON("METHOD_NOT_ALLOWED", "Use POST."))
				return
			}
			s.handleThreadAction(w, r, sess)
		case "/api/stop":
			if r.Method != http.MethodPost {
				writeJSON(w, http.StatusMethodNotAllowed, errJSON("METHOD_NOT_ALLOWED", "Use POST."))
				return
			}
			s.handleStop(w, r, sess)
		case "/api/send":
			if r.Method != http.MethodPost {
				writeJSON(w, http.StatusMethodNotAllowed, errJSON("METHOD_NOT_ALLOWED", "Use POST."))
				return
			}
			s.handleSend(w, r, sess)
		default:
			writeJSON(w, http.StatusNotFound, errJSON("NOT_FOUND", "API route not found."))
		}
	}
}

func (s *serverState) handlePair(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code   string `json:"code"`
		Device string `json:"deviceName"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errJSON("BAD_JSON", "Invalid JSON."))
		return
	}
	if strings.TrimSpace(body.Code) != s.pairCode {
		s.auditEvent("pair_failed", map[string]any{"ip": clientIP(r)})
		writeJSON(w, http.StatusUnauthorized, errJSON("BAD_PAIR_CODE", "Pairing code is incorrect."))
		return
	}
	token := randomToken()
	sess := session{
		TokenHash: hash(token),
		Device:    clip(body.Device, 80),
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(sessionTTL),
		IP:        clientIP(r),
	}
	if sess.Device == "" {
		sess.Device = "Phone"
	}
	s.mu.Lock()
	s.sessions[sess.TokenHash] = sess
	s.trusted[sess.TokenHash] = sess
	s.saveTrustedLocked()
	s.mu.Unlock()
	s.auditEvent("paired", map[string]any{"device": sess.Device, "ip": sess.IP})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": token, "expiresAt": sess.ExpiresAt.Format(time.RFC3339)})
}

func (s *serverState) handleSend(w http.ResponseWriter, r *http.Request, sess session) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	var body struct {
		Text        string            `json:"text"`
		ThreadID    string            `json:"threadId"`
		RequestID   string            `json:"clientRequestId"`
		Attachments []attachmentInput `json:"attachments"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errJSON("BAD_JSON", "Invalid JSON."))
		return
	}
	text := strings.TrimSpace(body.Text)
	attachments, err := s.saveAttachments(body.Attachments)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errJSON("BAD_ATTACHMENT", err.Error()))
		return
	}
	if text == "" && len(attachments) == 0 {
		writeJSON(w, http.StatusBadRequest, errJSON("EMPTY_TEXT", "Message or attachment is required."))
		return
	}
	if len([]rune(text)) > maxTextLength {
		writeJSON(w, http.StatusRequestEntityTooLarge, errJSON("TEXT_TOO_LONG", "Message is too long."))
		return
	}
	if body.ThreadID != "" && !validThreadID(body.ThreadID) {
		writeJSON(w, http.StatusBadRequest, errJSON("BAD_THREAD_ID", "Invalid thread id."))
		return
	}
	body.RequestID = strings.TrimSpace(body.RequestID)
	if body.RequestID == "" {
		body.RequestID = randomToken()
	}
	if len(body.RequestID) > 100 {
		writeJSON(w, http.StatusBadRequest, errJSON("BAD_REQUEST_ID", "Request id is too long."))
		return
	}
	if receipt, ok := s.recentReceipt(body.RequestID); ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "duplicate": true, "requestId": receipt.RequestID, "queued": receipt.Queued, "sentAt": receipt.SentAt, "attachments": receipt.Attachments})
		return
	}
	sessionFile := ""
	queuedHint := false
	if body.ThreadID != "" {
		sessionFile = s.fileForThread(body.ThreadID)
		activeCount := 0
		for _, row := range s.listThreads(false) {
			if row.Active {
				activeCount++
			}
			if row.ID == body.ThreadID && row.Active {
				queuedHint = true
			}
		}
		if activeCount >= 2 {
			queuedHint = true
		}
	}
	queued, err := pasteIntoCodex(text, body.ThreadID, sessionFile, attachments, queuedHint)
	if err != nil {
		s.auditEvent("send_failed", map[string]any{"device": sess.Device, "threadId": body.ThreadID, "error": err.Error()})
		writeJSON(w, http.StatusInternalServerError, errJSON("SEND_FAILED", err.Error()))
		return
	}
	s.auditEvent("send", map[string]any{"device": sess.Device, "threadId": body.ThreadID, "chars": len([]rune(text)), "attachments": len(attachments), "queued": queued})
	receipt := sendReceipt{RequestID: body.RequestID, Queued: queued, SentAt: time.Now().Format(time.RFC3339), Attachments: attachments, createdAt: time.Now()}
	s.rememberReceipt(receipt)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "requestId": receipt.RequestID, "queued": queued, "sentAt": receipt.SentAt, "attachments": attachments})
}

func (s *serverState) recentReceipt(id string) (sendReceipt, bool) {
	s.recentMu.Lock()
	defer s.recentMu.Unlock()
	now := time.Now()
	for key, receipt := range s.recent {
		if now.Sub(receipt.createdAt) > 30*time.Minute {
			delete(s.recent, key)
		}
	}
	receipt, ok := s.recent[id]
	return receipt, ok
}

func (s *serverState) rememberReceipt(receipt sendReceipt) {
	s.recentMu.Lock()
	s.recent[receipt.RequestID] = receipt
	s.recentMu.Unlock()
}

func (s *serverState) handleThreadAction(w http.ResponseWriter, r *http.Request, sess session) {
	var body struct {
		ThreadID string `json:"threadId"`
		Action   string `json:"action"`
		Name     string `json:"name"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errJSON("BAD_JSON", "Invalid JSON."))
		return
	}
	if !validThreadID(body.ThreadID) {
		writeJSON(w, http.StatusBadRequest, errJSON("BAD_THREAD_ID", "Invalid thread id."))
		return
	}
	action := strings.TrimSpace(strings.ToLower(body.Action))
	state := s.loadLocalState()
	switch action {
	case "pin":
		state.PinnedThreadIDs = setThreadMembership(state.PinnedThreadIDs, body.ThreadID, true)
	case "unpin":
		state.PinnedThreadIDs = setThreadMembership(state.PinnedThreadIDs, body.ThreadID, false)
	case "archive":
		state.ArchivedThreadIDs = setThreadMembership(state.ArchivedThreadIDs, body.ThreadID, true)
		state.PinnedThreadIDs = setThreadMembership(state.PinnedThreadIDs, body.ThreadID, false)
	case "restore":
		state.ArchivedThreadIDs = setThreadMembership(state.ArchivedThreadIDs, body.ThreadID, false)
	case "rename":
		name := strings.TrimSpace(strings.Join(strings.Fields(body.Name), " "))
		if name == "" {
			writeJSON(w, http.StatusBadRequest, errJSON("EMPTY_NAME", "Thread name is empty."))
			return
		}
		if len([]rune(name)) > 120 {
			writeJSON(w, http.StatusBadRequest, errJSON("NAME_TOO_LONG", "Thread name is too long."))
			return
		}
		if state.TitleOverrides == nil {
			state.TitleOverrides = map[string]threadTitleRecord{}
		}
		state.TitleOverrides[body.ThreadID] = threadTitleRecord{Name: name, RenamedAt: time.Now().Format(time.RFC3339)}
	default:
		writeJSON(w, http.StatusBadRequest, errJSON("BAD_ACTION", "Unsupported thread action."))
		return
	}
	if err := s.saveLocalState(state); err != nil {
		writeJSON(w, http.StatusInternalServerError, errJSON("STATE_SAVE_FAILED", err.Error()))
		return
	}
	s.auditEvent("thread_action", map[string]any{"device": sess.Device, "threadId": body.ThreadID, "action": action})
	next := body.ThreadID
	if action == "archive" {
		next = ""
		if rows := s.listThreads(false); len(rows) > 0 {
			next = rows[0].ID
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": action, "threadId": body.ThreadID, "nextThreadId": next})
}

func (s *serverState) handleStop(w http.ResponseWriter, r *http.Request, sess session) {
	var body struct {
		ThreadID string `json:"threadId"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errJSON("BAD_JSON", "Invalid JSON."))
		return
	}
	if body.ThreadID != "" && !validThreadID(body.ThreadID) {
		writeJSON(w, http.StatusBadRequest, errJSON("BAD_THREAD_ID", "Invalid thread id."))
		return
	}
	if err := stopCodex(body.ThreadID); err != nil {
		s.auditEvent("stop_failed", map[string]any{"device": sess.Device, "threadId": body.ThreadID, "error": err.Error()})
		writeJSON(w, http.StatusInternalServerError, errJSON("STOP_FAILED", err.Error()))
		return
	}
	s.auditEvent("stop", map[string]any{"device": sess.Device, "threadId": body.ThreadID})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *serverState) diagnostics() map[string]any {
	hwnd := findCodexWindow()
	title := ""
	if hwnd != 0 {
		title = windowText(hwnd)
	}
	return map[string]any{
		"ok":          true,
		"service":     appName,
		"version":     appVersion,
		"bind":        s.host,
		"port":        s.port,
		"lanUrls":     lanURLs(s.port),
		"cdp":         detectCDP(),
		"codexWindow": map[string]any{"found": hwnd != 0, "title": title},
		"sessionsDir": s.sessionsDir(),
		"dataDir":     s.dataDir,
	}
}

func (s *serverState) requireSession(w http.ResponseWriter, r *http.Request) (session, bool) {
	auth := r.Header.Get("Authorization")
	token := ""
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		token = strings.TrimSpace(auth[7:])
	}
	if token == "" {
		writeJSON(w, http.StatusUnauthorized, errJSON("UNAUTHORIZED", "Missing session token."))
		return session{}, false
	}
	tokenHash := hash(token)
	s.mu.Lock()
	sess, ok := s.sessions[tokenHash]
	if ok && time.Now().After(sess.ExpiresAt) {
		delete(s.sessions, tokenHash)
		ok = false
	}
	if !ok {
		if trusted, found := s.trusted[tokenHash]; found {
			trusted.ExpiresAt = time.Now().Add(sessionTTL)
			s.sessions[tokenHash] = trusted
			sess = trusted
			ok = true
		}
	}
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errJSON("SESSION_EXPIRED", "Pair this device again."))
		return session{}, false
	}
	return sess, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	data, _ := json.Marshal(body)
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func errJSON(code, message string) map[string]any {
	return map[string]any{"ok": false, "code": code, "message": message}
}

func readJSON(r *http.Request, out any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxBodyBytes {
		return errors.New("request too large")
	}
	return json.Unmarshal(body, out)
}

func queryInt(r *http.Request, key string, fallback, min, max int) int {
	value, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil || value < min {
		return fallback
	}
	if value > max {
		return max
	}
	return value
}

func (s *serverState) saveAttachments(inputs []attachmentInput) ([]savedAttachment, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if len(inputs) > maxAttachments {
		return nil, fmt.Errorf("最多一次发送 %d 个附件", maxAttachments)
	}
	dir := filepath.Join(s.dataDir, "uploads")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建上传目录失败: %w", err)
	}
	cleanupUploads(dir)
	out := make([]savedAttachment, 0, len(inputs))
	for i, item := range inputs {
		att, err := saveAttachment(dir, item, i)
		if err != nil {
			return nil, err
		}
		out = append(out, att)
	}
	return out, nil
}

func saveAttachment(dir string, item attachmentInput, index int) (savedAttachment, error) {
	mime := strings.ToLower(strings.TrimSpace(item.Type))
	dataURL := strings.TrimSpace(item.DataURL)
	if dataURL == "" {
		return savedAttachment{}, fmt.Errorf("附件数据为空")
	}
	if strings.HasPrefix(dataURL, "data:") {
		header, encoded, ok := strings.Cut(dataURL, ",")
		if !ok || !strings.Contains(strings.ToLower(header), ";base64") {
			return savedAttachment{}, fmt.Errorf("附件数据格式不正确")
		}
		if mime == "" {
			mime = strings.TrimPrefix(strings.Split(strings.TrimPrefix(header, "data:"), ";")[0], " ")
		}
		dataURL = encoded
	}
	buf, err := base64.StdEncoding.DecodeString(dataURL)
	if err != nil {
		return savedAttachment{}, fmt.Errorf("附件解码失败")
	}
	if len(buf) == 0 || len(buf) > maxAttachmentBytes {
		return savedAttachment{}, fmt.Errorf("单个附件不能超过 %dMB", maxAttachmentBytes/1024/1024)
	}
	name := safeFileName(item.Name)
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		ext = extensionForMime(mime)
		name += ext
	}
	kind := attachmentKind(mime, ext)
	if kind == "" {
		return savedAttachment{}, fmt.Errorf("暂不支持这种附件类型: %s", ext)
	}
	if kind == "image" && !allowedImageExt(ext) {
		ext = extensionForMime(mime)
		name = strings.TrimSuffix(name, filepath.Ext(name)) + ext
	}
	if name == "" || name == ext {
		name = fmt.Sprintf("attachment-%d%s", index+1, ext)
	}
	fileName := fmt.Sprintf("%d-%d-%s", time.Now().UnixNano(), index, name)
	filePath := filepath.Join(dir, fileName)
	if err := os.WriteFile(filePath, buf, 0644); err != nil {
		return savedAttachment{}, fmt.Errorf("保存附件失败: %w", err)
	}
	return savedAttachment{Name: name, Type: mime, Kind: kind, Size: len(buf), Path: filePath}, nil
}

func safeFileName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "." || name == string(filepath.Separator) {
		name = ""
	}
	var b strings.Builder
	for _, r := range name {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			b.WriteRune('_')
			continue
		}
		b.WriteRune(r)
	}
	clean := strings.Trim(b.String(), " .")
	if clean == "" {
		return "image"
	}
	return clip(clean, 96)
}

func extensionForMime(mime string) string {
	switch strings.ToLower(mime) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "application/pdf":
		return ".pdf"
	case "text/markdown":
		return ".md"
	case "text/csv":
		return ".csv"
	case "application/json":
		return ".json"
	case "text/plain":
		return ".txt"
	case "application/zip", "application/x-zip-compressed":
		return ".zip"
	default:
		if strings.HasPrefix(strings.ToLower(mime), "image/") {
			return ".png"
		}
		return ".bin"
	}
}

func allowedImageExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp":
		return true
	default:
		return false
	}
}

func attachmentKind(mime, ext string) string {
	mime = strings.ToLower(strings.TrimSpace(mime))
	ext = strings.ToLower(strings.TrimSpace(ext))
	if strings.HasPrefix(mime, "image/") || allowedImageExt(ext) {
		return "image"
	}
	switch ext {
	case ".txt", ".md", ".markdown", ".csv", ".json", ".yaml", ".yml", ".toml", ".xml", ".html", ".css", ".js", ".ts", ".py", ".go", ".rs", ".java", ".c", ".cpp", ".h", ".hpp", ".cs", ".php", ".rb", ".sh", ".ps1", ".log":
		return "file"
	case ".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx", ".rtf", ".zip":
		return "file"
	default:
		switch mime {
		case "application/pdf", "text/plain", "text/markdown", "text/csv", "application/json", "application/zip", "application/x-zip-compressed", "application/msword", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "application/vnd.ms-excel", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "application/vnd.ms-powerpoint", "application/vnd.openxmlformats-officedocument.presentationml.presentation":
			return "file"
		}
	}
	return ""
}

func cleanupUploads(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
		}
	}
}

func hostname() string {
	name, _ := os.Hostname()
	return name
}

func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		return strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *serverState) auditEvent(event string, details map[string]any) {
	details["at"] = time.Now().Format(time.RFC3339)
	details["event"] = event
	data, _ := json.Marshal(details)
	s.audit.Println(string(data))
}

func (s *serverState) authPath() string {
	return filepath.Join(s.dataDir, "auth.json")
}

func (s *serverState) loadTrusted() {
	data, err := os.ReadFile(s.authPath())
	if err != nil {
		return
	}
	var rows []session
	if json.Unmarshal(data, &rows) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range rows {
		if row.TokenHash != "" {
			s.trusted[row.TokenHash] = row
		}
	}
}

func (s *serverState) saveTrustedLocked() {
	rows := make([]session, 0, len(s.trusted))
	for _, row := range s.trusted {
		rows = append(rows, row)
	}
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(s.authPath(), data, 0600)
}

func (s *serverState) localStatePath() string {
	return filepath.Join(s.dataDir, "state.json")
}

func (s *serverState) loadLocalState() appLocalState {
	state := appLocalState{TitleOverrides: map[string]threadTitleRecord{}}
	data, err := os.ReadFile(s.localStatePath())
	if err != nil {
		return state
	}
	if json.Unmarshal(data, &state) != nil {
		return appLocalState{TitleOverrides: map[string]threadTitleRecord{}}
	}
	state.PinnedThreadIDs = filterThreadIDs(state.PinnedThreadIDs)
	state.ArchivedThreadIDs = filterThreadIDs(state.ArchivedThreadIDs)
	if state.TitleOverrides == nil {
		state.TitleOverrides = map[string]threadTitleRecord{}
	}
	for id, record := range state.TitleOverrides {
		if !validThreadID(id) || strings.TrimSpace(record.Name) == "" {
			delete(state.TitleOverrides, id)
		}
	}
	return state
}

func (s *serverState) saveLocalState(state appLocalState) error {
	state.PinnedThreadIDs = filterThreadIDs(state.PinnedThreadIDs)
	state.ArchivedThreadIDs = filterThreadIDs(state.ArchivedThreadIDs)
	if state.TitleOverrides == nil {
		state.TitleOverrides = map[string]threadTitleRecord{}
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.localStatePath(), data, 0600)
}

func filterThreadIDs(ids []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		if validThreadID(id) && !seen[id] {
			out = append(out, id)
			seen[id] = true
		}
	}
	return out
}

func setThreadMembership(ids []string, id string, enabled bool) []string {
	ids = filterThreadIDs(ids)
	var out []string
	for _, existing := range ids {
		if existing != id {
			out = append(out, existing)
		}
	}
	if enabled && validThreadID(id) {
		out = append([]string{id}, out...)
	}
	return out
}

func threadIDSet(ids []string) map[string]bool {
	set := map[string]bool{}
	for _, id := range ids {
		if validThreadID(id) {
			set[id] = true
		}
	}
	return set
}

func validThreadID(id string) bool {
	return regexp.MustCompile(`(?i)^[a-f0-9]{8}-[a-f0-9-]{27,}$`).MatchString(id)
}

func (s *serverState) sessionsDir() string {
	return filepath.Join(s.home, ".codex", "sessions")
}

func (s *serverState) indexPath() string {
	return filepath.Join(s.home, ".codex", "session_index.jsonl")
}

func (s *serverState) sessionFiles() []string {
	var files []string
	_ = filepath.WalkDir(s.sessionsDir(), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(strings.ToLower(path), ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	return files
}

func (s *serverState) listThreads(includeArchived bool) []threadRow {
	names := s.indexNames()
	local := s.loadLocalState()
	stateMTime := fileMTimeUnix(s.localStatePath())
	indexMTime := fileMTimeUnix(s.indexPath())
	var rows []threadRow
	for _, file := range s.sessionFiles() {
		if !isUserThreadFile(file) {
			continue
		}
		row, ok := s.cachedThreadRow(file, names, local, stateMTime, indexMTime)
		if !ok || (row.Archived && !includeArchived) {
			continue
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Archived != rows[j].Archived {
			return !rows[i].Archived
		}
		if rows[i].Pinned != rows[j].Pinned {
			return rows[i].Pinned
		}
		return rows[i].mtime.After(rows[j].mtime)
	})
	if len(rows) > 160 {
		rows = rows[:160]
	}
	return rows
}

func isUserThreadFile(file string) bool {
	f, err := os.Open(file)
	if err != nil {
		return true
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, 64*1024)
	line, err := reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return true
	}
	var item map[string]any
	if json.Unmarshal(line, &item) != nil || item["type"] != "session_meta" {
		return true
	}
	payload := asMap(item["payload"])
	source := asMap(payload["source"])
	_, isSubagent := source["subagent"]
	return !isSubagent
}

func fileMTimeUnix(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.ModTime().UnixNano()
	}
	return 0
}

func (s *serverState) cachedThreadRow(file string, names map[string]string, local appLocalState, stateMTime, indexMTime int64) (threadRow, bool) {
	id := threadIDFromFile(file)
	if id == "" {
		return threadRow{}, false
	}
	info, err := os.Stat(file)
	if err != nil {
		return threadRow{}, false
	}
	s.cacheMu.Lock()
	entry, found := s.threads[file]
	if found && entry.FileSize == info.Size() && entry.FileMTime == info.ModTime().UnixNano() && entry.StateMTime == stateMTime && entry.IndexMTime == indexMTime {
		s.cacheMu.Unlock()
		return entry.Row, true
	}
	s.cacheMu.Unlock()
	items := parseJSONLTail(file, maxTailBytes)
	row := threadRowFromItems(id, file, info, items, names, local)
	s.cacheMu.Lock()
	s.threads[file] = threadCacheEntry{Row: row, FileSize: info.Size(), FileMTime: info.ModTime().UnixNano(), StateMTime: stateMTime, IndexMTime: indexMTime}
	s.cacheMu.Unlock()
	return row, true
}

func threadRowFromItems(id, file string, info os.FileInfo, items []map[string]any, names map[string]string, local appLocalState) threadRow {
	pinned := threadIDSet(local.PinnedThreadIDs)
	archived := threadIDSet(local.ArchivedThreadIDs)
	title := names[id]
	if override := strings.TrimSpace(local.TitleOverrides[id].Name); override != "" {
		title = override
	}
	if title == "" {
		title = firstUser(items)
	}
	if title == "" {
		title = "(untitled)"
	}
	active := isSessionActive(file)
	status := "idle"
	if active {
		status = "running"
	}
	cwd := cwdFromItems(items)
	projectKey, projectName := projectFromCwd(cwd)
	return threadRow{ID: id, Title: title, Preview: lastAssistant(items), Cwd: cwd, SessionFile: filepath.Base(file), UpdatedAt: info.ModTime().Format(time.RFC3339), Active: active, Status: status, Pinned: pinned[id], Archived: archived[id], ProjectKey: projectKey, ProjectName: projectName, Context: contextUsageFromItems(items), mtime: info.ModTime()}
}

func projectFromCwd(cwd string) (string, string) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return "__unassigned__", "未归类"
	}
	cleanPath := filepath.Clean(cwd)
	name := filepath.Base(cleanPath)
	if name == "." || name == string(filepath.Separator) || name == "" {
		name = cleanPath
	}
	return strings.ToLower(cleanPath), name
}

func projectSummary(rows []threadRow) []map[string]any {
	type countRow struct {
		key, name, path         string
		count, active, archived int
		updated                 string
	}
	byKey := map[string]*countRow{}
	for _, row := range rows {
		item := byKey[row.ProjectKey]
		if item == nil {
			item = &countRow{key: row.ProjectKey, name: row.ProjectName, path: row.Cwd}
			byKey[row.ProjectKey] = item
		}
		item.count++
		if row.Active {
			item.active++
		}
		if row.Archived {
			item.archived++
		}
		if row.UpdatedAt > item.updated {
			item.updated = row.UpdatedAt
		}
	}
	items := make([]*countRow, 0, len(byKey))
	for _, item := range byKey {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].updated > items[j].updated })
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, map[string]any{"key": item.key, "name": item.name, "path": item.path, "count": item.count, "active": item.active, "archived": item.archived})
	}
	return out
}

func (s *serverState) indexNames() map[string]string {
	names := map[string]string{}
	file, err := os.Open(s.indexPath())
	if err != nil {
		return names
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 4*1024*1024)
	for scanner.Scan() {
		var row map[string]any
		if json.Unmarshal(scanner.Bytes(), &row) == nil {
			id, _ := row["id"].(string)
			name, _ := row["thread_name"].(string)
			if id != "" && name != "" {
				names[id] = name
			}
		}
	}
	return names
}

func (s *serverState) fileForThread(id string) string {
	var best string
	var bestTime time.Time
	for _, file := range s.sessionFiles() {
		if !strings.Contains(filepath.Base(file), id) {
			continue
		}
		info, err := os.Stat(file)
		if err == nil && (best == "" || info.ModTime().After(bestTime)) {
			best = file
			bestTime = info.ModTime()
		}
	}
	return best
}

func (s *serverState) historyPage(id string, before, limit int) ([]messageRow, bool, bool, int) {
	file := s.fileForThread(id)
	if file == "" {
		return nil, false, false, 0
	}
	items := parseJSONLTail(file, 24*1024*1024)
	var messages []messageRow
	for _, item := range items {
		payload := asMap(item["payload"])
		if item["type"] == "event_msg" && payload["type"] == "user_message" {
			if text := clean(fmt.Sprint(payload["message"]), 12000); text != "" {
				messages = append(messages, messageRow{Seq: len(messages) + 1, Role: "user", Text: text, Timestamp: asString(item["timestamp"])})
			}
		}
		if item["type"] == "response_item" && payload["type"] == "message" && payload["role"] == "assistant" {
			phase := asString(payload["phase"])
			if phase != "" && phase != "final" {
				continue
			}
			if text := clean(contentText(payload["content"]), 12000); text != "" {
				messages = append(messages, messageRow{Seq: len(messages) + 1, Role: "assistant", Text: text, Timestamp: asString(item["timestamp"])})
			}
		}
		if item["type"] == "event_msg" && payload["type"] == "task_complete" {
			if text := clean(asString(payload["last_agent_message"]), 12000); text != "" {
				if len(messages) > 0 && messages[len(messages)-1].Role == "assistant" && messages[len(messages)-1].Text == text {
					continue
				}
				messages = append(messages, messageRow{Seq: len(messages) + 1, Role: "assistant", Text: text, Timestamp: asString(item["timestamp"])})
			}
		}
	}
	if limit <= 0 {
		limit = maxHistory
	}
	end := len(messages)
	if before > 0 && before-1 < end {
		end = before - 1
	}
	if end < 0 {
		end = 0
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	page := messages[start:end]
	nextBefore := 0
	if start > 0 && len(page) > 0 {
		nextBefore = page[0].Seq
	}
	return page, true, start > 0, nextBefore
}

func (s *serverState) history(id string) ([]messageRow, bool) {
	messages, available, _, _ := s.historyPage(id, 0, maxHistory)
	return messages, available
}

func (s *serverState) threadStatusData(id string) (threadRow, []map[string]any, bool) {
	file := s.fileForThread(id)
	if file == "" {
		return threadRow{}, nil, false
	}
	info, err := os.Stat(file)
	if err != nil {
		return threadRow{}, nil, false
	}
	items := parseJSONLTail(file, 6*1024*1024)
	row := threadRowFromItems(id, file, info, items, s.indexNames(), s.loadLocalState())
	return row, items, true
}

func threadIDFromFile(file string) string {
	match := threadIDPattern.FindStringSubmatch(filepath.Base(file))
	if len(match) > 1 {
		return match[1]
	}
	return ""
}

func parseJSONLTail(file string, maxBytes int64) []map[string]any {
	info, err := os.Stat(file)
	if err != nil {
		return nil
	}
	start := info.Size() - maxBytes
	if start < 0 {
		start = 0
	}
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	if start > 0 {
		_, _ = f.Seek(start, io.SeekStart)
		_, _ = bufio.NewReader(f).ReadString('\n')
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024), 8*1024*1024)
	var rows []map[string]any
	for scanner.Scan() {
		var row map[string]any
		if json.Unmarshal(scanner.Bytes(), &row) == nil {
			rows = append(rows, row)
		}
	}
	return rows
}

func statusDetailsFromItems(items []map[string]any) ([]statusStep, string, string) {
	steps := make([]statusStep, 0, 32)
	model := ""
	reasoning := ""
	add := func(index int, kind, label, detail, timestamp string) {
		detail = clean(detail, 520)
		if len(steps) > 0 {
			last := steps[len(steps)-1]
			if last.Kind == kind && last.Label == label && last.Detail == detail {
				return
			}
		}
		steps = append(steps, statusStep{ID: fmt.Sprintf("%d-%s", index, timestamp), Kind: kind, Label: label, Detail: detail, Timestamp: timestamp})
	}
	for index, item := range items {
		payload := asMap(item["payload"])
		timestamp := asString(item["timestamp"])
		for _, key := range []string{"model", "model_name"} {
			if value := asString(payload[key]); value != "" {
				model = value
			}
		}
		for _, key := range []string{"reasoning_effort", "reasoning"} {
			if value := asString(payload[key]); value != "" {
				reasoning = value
			}
		}
		if item["type"] == "event_msg" {
			kind := asString(payload["type"])
			switch kind {
			case "task_started":
				add(index, "start", "开始处理", "", timestamp)
			case "agent_reasoning":
				add(index, "reasoning", "正在思考", asString(payload["text"]), timestamp)
			case "task_complete":
				add(index, "complete", "处理完成", "", timestamp)
			default:
				lower := strings.ToLower(kind)
				if strings.Contains(lower, "error") || strings.Contains(lower, "fail") || strings.Contains(lower, "abort") {
					detail := asString(payload["message"])
					if detail == "" {
						detail = asString(payload["error"])
					}
					add(index, "error", "执行出现错误", detail, timestamp)
				}
			}
		}
		if item["type"] != "response_item" {
			continue
		}
		payloadType := asString(payload["type"])
		if payloadType != "function_call" && payloadType != "custom_tool_call" {
			continue
		}
		name := asString(payload["name"])
		detail := asString(payload["arguments"])
		if detail == "" {
			detail = asString(payload["input"])
		}
		add(index, "tool", toolLabel(name, detail), summarizeToolDetail(detail), timestamp)
	}
	if len(steps) > 30 {
		steps = steps[len(steps)-30:]
	}
	return steps, model, reasoning
}

func toolLabel(name, detail string) string {
	lower := strings.ToLower(name + " " + detail)
	switch {
	case strings.Contains(lower, "update_plan"):
		return "更新执行计划"
	case strings.Contains(lower, "apply_patch"):
		return "修改项目文件"
	case strings.Contains(lower, "shell_command") || strings.Contains(lower, "exec_command"):
		return "运行命令"
	case strings.Contains(lower, "web__run") || strings.Contains(lower, "search_query"):
		return "查找资料"
	case strings.Contains(lower, "view_image"):
		return "查看图片"
	case strings.Contains(lower, "wait"):
		return "等待命令完成"
	case name != "":
		return "调用 " + clean(name, 48)
	default:
		return "执行工具"
	}
}

func summarizeToolDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return ""
	}
	var value map[string]any
	if json.Unmarshal([]byte(detail), &value) == nil {
		for _, key := range []string{"command", "path", "question", "objective"} {
			if text := asString(value[key]); text != "" {
				return clean(text, 260)
			}
		}
	}
	return clean(detail, 260)
}

func asMap(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func asSlice(value any) []any {
	if items, ok := value.([]any); ok {
		return items
	}
	return nil
}

func asString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func contentText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			switch x := item.(type) {
			case string:
				parts = append(parts, x)
			case map[string]any:
				if text := asString(x["text"]); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func firstUser(items []map[string]any) string {
	for _, item := range items {
		payload := asMap(item["payload"])
		if item["type"] == "event_msg" && payload["type"] == "user_message" {
			return clean(fmt.Sprint(payload["message"]), 120)
		}
	}
	return ""
}

func lastAssistant(items []map[string]any) string {
	for i := len(items) - 1; i >= 0; i-- {
		payload := asMap(items[i]["payload"])
		if items[i]["type"] == "response_item" && payload["type"] == "message" && payload["role"] == "assistant" {
			return clean(contentText(payload["content"]), 260)
		}
		if items[i]["type"] == "event_msg" && payload["type"] == "task_complete" {
			if text := clean(asString(payload["last_agent_message"]), 260); text != "" {
				return text
			}
		}
	}
	return ""
}

func cwdFromItems(items []map[string]any) string {
	for _, item := range items {
		payload := asMap(item["payload"])
		for _, key := range []string{"cwd", "current_working_directory"} {
			if text := asString(payload[key]); text != "" {
				return text
			}
		}
	}
	return ""
}

func isSessionActive(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return false
	}
	const chunkSize int64 = 256 * 1024
	const overlap int64 = 64
	startMarker := []byte(`"type":"task_started"`)
	endMarker := []byte(`"type":"task_complete"`)
	end := info.Size()
	for end > 0 {
		start := end - chunkSize
		if start < 0 {
			start = 0
		}
		buf := make([]byte, end-start)
		n, readErr := file.ReadAt(buf, start)
		buf = buf[:n]
		lastStart := bytes.LastIndex(buf, startMarker)
		lastEnd := bytes.LastIndex(buf, endMarker)
		if lastStart >= 0 || lastEnd >= 0 {
			return lastStart > lastEnd
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return false
		}
		if start == 0 {
			break
		}
		end = start + overlap
	}
	return false
}

func contextUsageFromItems(items []map[string]any) contextUsage {
	windowTokens := 0
	usedTokens := 0
	updatedAt := ""
	for _, item := range items {
		payload := asMap(item["payload"])
		if item["type"] == "event_msg" && payload["type"] == "task_started" {
			if v := intFromAny(payload["model_context_window"]); v > 0 {
				windowTokens = v
			}
		}
		if item["type"] != "event_msg" || payload["type"] != "token_count" {
			continue
		}
		info := asMap(payload["info"])
		if v := intFromAny(info["model_context_window"]); v > 0 {
			windowTokens = v
		}
		usage := asMap(info["current_token_usage"])
		if len(usage) == 0 {
			usage = asMap(info["last_token_usage"])
		}
		if len(usage) == 0 {
			continue
		}
		input := intFromAny(usage["input_tokens"])
		output := intFromAny(usage["output_tokens"])
		total := intFromAny(usage["total_tokens"])
		if total <= 0 {
			total = input + output
		}
		if total > 0 {
			usedTokens = total
			updatedAt = asString(item["timestamp"])
		}
	}
	if windowTokens <= 0 || usedTokens <= 0 {
		return contextUsage{Available: false, WindowTokens: windowTokens, RemainingTokens: windowTokens, UpdatedAt: updatedAt}
	}
	percent := float64(usedTokens) / float64(windowTokens) * 100
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	return contextUsage{
		Available:       true,
		UsedTokens:      usedTokens,
		WindowTokens:    windowTokens,
		RemainingTokens: maxInt(0, windowTokens-usedTokens),
		Percent:         percent,
		UpdatedAt:       updatedAt,
	}
}

func intFromAny(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	default:
		return 0
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clean(value string, max int) string {
	text := strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
	if len([]rune(text)) > max {
		runes := []rune(text)
		text = string(runes[:max])
	}
	return text
}

func clip(value string, max int) string {
	value = strings.TrimSpace(value)
	if len([]rune(value)) > max {
		return string([]rune(value)[:max])
	}
	return value
}

func pasteIntoCodex(text, threadID, sessionFile string, attachments []savedAttachment, queuedHint bool) (bool, error) {
	if len(attachments) == 0 && strings.TrimSpace(text) != "" {
		if err := cdpSendText(text, threadID); err == nil {
			return false, nil
		}
	}
	beforeSize := fileSize(sessionFile)
	hwnd := ensureCodexWindow(threadID, 8*time.Second)
	if hwnd == 0 {
		return false, errors.New("Codex window not found. Open Codex Desktop first")
	}
	if !focusWindow(hwnd) {
		return false, errors.New("failed to focus Codex window")
	}
	time.Sleep(220 * time.Millisecond)
	if !focusCodexComposer(hwnd) {
		return false, errors.New("failed to focus Codex message input")
	}
	time.Sleep(80 * time.Millisecond)
	if len(attachments) > 0 {
		for _, item := range attachments {
			if item.Kind == "image" {
				if err := setClipboardImageDIB(item.Path); err != nil {
					if err := setClipboardFiles([]string{item.Path}); err != nil {
						return false, fmt.Errorf("failed to set image clipboard: %w", err)
					}
				}
			} else {
				if err := setClipboardFiles([]string{item.Path}); err != nil {
					return false, fmt.Errorf("failed to set file clipboard: %w", err)
				}
			}
			sendCtrlV()
			time.Sleep(650 * time.Millisecond)
		}
	}
	if strings.TrimSpace(text) != "" {
		if err := setClipboardText(text); err != nil {
			return false, fmt.Errorf("failed to set clipboard: %w", err)
		}
		sendCtrlV()
		time.Sleep(180 * time.Millisecond)
	}
	keyTap(vkReturn)
	if strings.TrimSpace(text) != "" && sessionFile != "" {
		verifyTimeout := 7 * time.Second
		if queuedHint {
			verifyTimeout = 1200 * time.Millisecond
		}
		if waitForUserMessage(sessionFile, text, beforeSize, verifyTimeout) {
			return false, nil
		}
		if queuedHint {
			return true, nil
		}
		if err := retryCodexSubmit(hwnd, text); err == nil && waitForUserMessage(sessionFile, text, beforeSize, 8*time.Second) {
			return false, nil
		}
		return false, errors.New("Codex did not accept the message; keep Codex Desktop unlocked and open on a thread")
	}
	return queuedHint, nil
}

func fileSize(path string) int64 {
	if path == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func waitForUserMessage(path, text string, offset int64, timeout time.Duration) bool {
	target := strings.TrimSpace(text)
	if path == "" || target == "" || offset < 0 {
		return false
	}
	scanFrom := offset
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		file, err := os.Open(path)
		if err == nil {
			if info, statErr := file.Stat(); statErr != nil || info.Size() < scanFrom {
				_ = file.Close()
				return false
			}
			atBoundary := true
			if scanFrom > 0 {
				_, _ = file.Seek(scanFrom-1, io.SeekStart)
				var previous [1]byte
				_, _ = file.Read(previous[:])
				atBoundary = previous[0] == '\n'
			}
			_, _ = file.Seek(scanFrom, io.SeekStart)
			reader := bufio.NewReaderSize(io.LimitReader(file, 8*1024*1024), 64*1024)
			if !atBoundary {
				_, _ = reader.ReadBytes('\n')
			}
			for {
				line, readErr := reader.ReadBytes('\n')
				if readErr == nil && messageLineContainsText(line, target) {
					_ = file.Close()
					return true
				}
				if readErr != nil {
					break
				}
			}
			_ = file.Close()
		}
		delay := time.Until(deadline)
		if delay > 180*time.Millisecond {
			delay = 180 * time.Millisecond
		}
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	return false
}

func messageLineContainsText(line []byte, target string) bool {
	var item map[string]any
	if json.Unmarshal(line, &item) != nil {
		return false
	}
	payload := asMap(item["payload"])
	if item["type"] == "event_msg" && payload["type"] == "user_message" {
		return strings.TrimSpace(asString(payload["message"])) == target
	}
	if item["type"] != "response_item" || payload["type"] != "message" || payload["role"] != "user" {
		return false
	}
	for _, part := range asSlice(payload["content"]) {
		content := asMap(part)
		if content["type"] == "input_text" && strings.TrimSpace(asString(content["text"])) == target {
			return true
		}
	}
	return false
}

func retryCodexSubmit(hwnd uintptr, text string) error {
	if !focusWindow(hwnd) {
		return errors.New("failed to refocus Codex window")
	}
	time.Sleep(180 * time.Millisecond)
	if !focusCodexComposer(hwnd) {
		return errors.New("failed to refocus Codex message input")
	}
	time.Sleep(100 * time.Millisecond)
	sendCtrlA()
	if err := setClipboardText(text); err != nil {
		return err
	}
	sendCtrlV()
	time.Sleep(240 * time.Millisecond)
	keyTap(vkReturn)
	return nil
}

func stopCodex(threadID string) error {
	if err := cdpStop(threadID); err == nil {
		return nil
	}
	hwnd := ensureCodexWindow(threadID, 6*time.Second)
	if hwnd == 0 {
		return errors.New("Codex window not found. Open Codex Desktop first")
	}
	if !focusWindow(hwnd) {
		return errors.New("failed to focus Codex window")
	}
	time.Sleep(160 * time.Millisecond)
	keyTap(vkEscape)
	return nil
}

func ensureCodexWindow(threadID string, timeout time.Duration) uintptr {
	if hwnd := findCodexWindow(); hwnd != 0 {
		if threadID != "" {
			_ = openBrowser("codex://threads/" + threadID)
			time.Sleep(500 * time.Millisecond)
			if next := findCodexWindow(); next != 0 {
				return next
			}
		}
		return hwnd
	}
	if threadID != "" {
		_ = openBrowser("codex://threads/" + threadID)
	} else {
		_ = openBrowser("codex://threads/new")
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if hwnd := findCodexWindow(); hwnd != 0 {
			return hwnd
		}
		time.Sleep(250 * time.Millisecond)
	}
	return 0
}

type cdpStatus struct {
	Available bool   `json:"available"`
	Port      string `json:"port"`
	Target    string `json:"target"`
	Error     string `json:"error,omitempty"`
}

type cdpTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func detectCDP() cdpStatus {
	target, port, err := findCDPTarget()
	if err != nil {
		return cdpStatus{Available: false, Port: cdpPorts()[0], Error: err.Error()}
	}
	return cdpStatus{Available: true, Port: port, Target: target.Title}
}

func cdpSendText(text, threadID string) error {
	if threadID != "" {
		_ = openBrowser("codex://threads/" + threadID)
		time.Sleep(450 * time.Millisecond)
	}
	target, _, err := findCDPTarget()
	if err != nil {
		return err
	}
	conn, err := newCDPConn(target.WebSocketDebuggerURL)
	if err != nil {
		return err
	}
	defer conn.Close()
	expr := cdpSendExpression(text)
	if _, err := conn.Call("Runtime.evaluate", map[string]any{
		"expression":                  expr,
		"awaitPromise":                true,
		"returnByValue":               true,
		"userGesture":                 true,
		"allowUnsafeEvalBlockedByCSP": true,
	}); err != nil {
		return err
	}
	return nil
}

func cdpStop(threadID string) error {
	if threadID != "" {
		_ = openBrowser("codex://threads/" + threadID)
		time.Sleep(350 * time.Millisecond)
	}
	target, _, err := findCDPTarget()
	if err != nil {
		return err
	}
	conn, err := newCDPConn(target.WebSocketDebuggerURL)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.Call("Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "windowsVirtualKeyCode": 27, "nativeVirtualKeyCode": 27, "key": "Escape", "code": "Escape"}); err != nil {
		return err
	}
	_, err = conn.Call("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "windowsVirtualKeyCode": 27, "nativeVirtualKeyCode": 27, "key": "Escape", "code": "Escape"})
	return err
}

func cdpPorts() []string {
	var ports []string
	if p := strings.TrimSpace(os.Getenv("CODEX_REMOTE_CDP_PORT")); p != "" {
		ports = append(ports, p)
	}
	ports = append(ports, defaultCDPPort, "9223", "9230", "9333", "18777")
	seen := map[string]bool{}
	var out []string
	for _, p := range ports {
		if p != "" && !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	return out
}

func findCDPTarget() (cdpTarget, string, error) {
	client := http.Client{Timeout: 850 * time.Millisecond}
	var lastErr error
	for _, port := range cdpPorts() {
		reqURL := "http://127.0.0.1:" + port + "/json/list"
		resp, err := client.Get(reqURL)
		if err != nil {
			lastErr = err
			continue
		}
		var targets []cdpTarget
		err = json.NewDecoder(resp.Body).Decode(&targets)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if target, ok := chooseCDPTarget(targets); ok {
			return target, port, nil
		}
		lastErr = errors.New("CDP target not found")
	}
	if lastErr == nil {
		lastErr = errors.New("CDP is not available")
	}
	return cdpTarget{}, cdpPorts()[0], lastErr
}

func chooseCDPTarget(targets []cdpTarget) (cdpTarget, bool) {
	var fallback cdpTarget
	for _, target := range targets {
		if target.WebSocketDebuggerURL == "" {
			continue
		}
		if fallback.WebSocketDebuggerURL == "" {
			fallback = target
		}
		hay := strings.ToLower(target.Title + " " + target.URL)
		if target.Type == "page" && (strings.Contains(hay, "codex") || strings.Contains(hay, "openai") || strings.HasPrefix(strings.ToLower(target.URL), "app://")) {
			return target, true
		}
	}
	if fallback.WebSocketDebuggerURL != "" {
		return fallback, true
	}
	return cdpTarget{}, false
}

func cdpSendExpression(text string) string {
	data, _ := json.Marshal(text)
	return `(async()=>{` +
		`const text=` + string(data) + `;` +
		`const seen=new Set();` +
		`function walk(root,out=[]){if(!root||seen.has(root))return out;seen.add(root);for(const el of root.querySelectorAll('textarea,input,[contenteditable="true"],[role="textbox"],button')){out.push(el);if(el.shadowRoot)walk(el.shadowRoot,out)}return out}` +
		`function visible(el){const r=el.getBoundingClientRect();const s=getComputedStyle(el);return r.width>1&&r.height>1&&s.visibility!=='hidden'&&s.display!=='none'}` +
		`const nodes=walk(document);` +
		`const input=nodes.find(el=>visible(el)&&(el.tagName==='TEXTAREA'||el.isContentEditable||el.getAttribute('role')==='textbox'||(el.tagName==='INPUT'&&/text|search|url|email|password|^$/i.test(el.type||''))));` +
		`if(!input)throw new Error('Codex input box not found');` +
		`input.focus();` +
		`if(input.isContentEditable){input.textContent=text;input.dispatchEvent(new InputEvent('input',{bubbles:true,inputType:'insertText',data:text}));}` +
		`else{const proto=input.tagName==='TEXTAREA'?HTMLTextAreaElement.prototype:HTMLInputElement.prototype;const desc=Object.getOwnPropertyDescriptor(proto,'value');desc.set.call(input,text);input.dispatchEvent(new InputEvent('input',{bubbles:true,inputType:'insertText',data:text}));input.dispatchEvent(new Event('change',{bubbles:true}));}` +
		`await new Promise(r=>setTimeout(r,80));` +
		`const buttons=nodes.filter(el=>el.tagName==='BUTTON'&&visible(el));` +
		`const send=buttons.find(b=>!/stop|cancel|停止|取消/i.test((b.innerText||b.title||b.ariaLabel||''))&&/(send|发送|submit|↑|↵)/i.test((b.innerText||b.title||b.ariaLabel||'')))||buttons[buttons.length-1];` +
		`if(send){send.click();return true;}` +
		`input.dispatchEvent(new KeyboardEvent('keydown',{bubbles:true,cancelable:true,key:'Enter',code:'Enter'}));` +
		`input.dispatchEvent(new KeyboardEvent('keyup',{bubbles:true,cancelable:true,key:'Enter',code:'Enter'}));` +
		`return true;` +
		`})()`
}

type cdpConn struct {
	conn   net.Conn
	reader *bufio.Reader
	nextID int
}

func newCDPConn(rawURL string) (*cdpConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "ws" {
		return nil, fmt.Errorf("unsupported CDP websocket scheme: %s", u.Scheme)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 1200*time.Millisecond)
	if err != nil {
		return nil, err
	}
	keyBytes := make([]byte, 16)
	_, _ = rand.Read(keyBytes)
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, u.Host, key)
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if !strings.Contains(status, "101") {
		_ = conn.Close()
		return nil, fmt.Errorf("CDP websocket handshake failed: %s", strings.TrimSpace(status))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	return &cdpConn{conn: conn, reader: reader}, nil
}

func (c *cdpConn) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *cdpConn) Call(method string, params map[string]any) (map[string]any, error) {
	c.nextID++
	id := c.nextID
	payload, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	_ = c.conn.SetDeadline(time.Now().Add(4 * time.Second))
	if err := c.writeFrame(payload); err != nil {
		return nil, err
	}
	for {
		frame, opcode, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		if opcode == 8 {
			return nil, errors.New("CDP websocket closed")
		}
		if opcode != 1 {
			continue
		}
		var msg map[string]any
		if json.Unmarshal(frame, &msg) != nil {
			continue
		}
		if intFromAny(msg["id"]) != id {
			continue
		}
		if errObj := asMap(msg["error"]); len(errObj) > 0 {
			return nil, fmt.Errorf("CDP %s failed: %s", method, asString(errObj["message"]))
		}
		return asMap(msg["result"]), nil
	}
}

func (c *cdpConn) writeFrame(payload []byte) error {
	header := []byte{0x81}
	length := len(payload)
	switch {
	case length < 126:
		header = append(header, byte(0x80|length))
	case length <= 65535:
		header = append(header, 0x80|126, byte(length>>8), byte(length))
	default:
		header = append(header, 0x80|127)
		var lenBytes [8]byte
		binary.BigEndian.PutUint64(lenBytes[:], uint64(length))
		header = append(header, lenBytes[:]...)
	}
	mask := make([]byte, 4)
	_, _ = rand.Read(mask)
	header = append(header, mask...)
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(masked)
	return err
}

func (c *cdpConn) readFrame() ([]byte, byte, error) {
	b1, err := c.reader.ReadByte()
	if err != nil {
		return nil, 0, err
	}
	b2, err := c.reader.ReadByte()
	if err != nil {
		return nil, 0, err
	}
	opcode := b1 & 0x0f
	masked := b2&0x80 != 0
	length := uint64(b2 & 0x7f)
	if length == 126 {
		var buf [2]byte
		if _, err := io.ReadFull(c.reader, buf[:]); err != nil {
			return nil, 0, err
		}
		length = uint64(binary.BigEndian.Uint16(buf[:]))
	} else if length == 127 {
		var buf [8]byte
		if _, err := io.ReadFull(c.reader, buf[:]); err != nil {
			return nil, 0, err
		}
		length = binary.BigEndian.Uint64(buf[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.reader, mask[:]); err != nil {
			return nil, 0, err
		}
	}
	if length > 16*1024*1024 {
		return nil, 0, errors.New("CDP frame too large")
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return nil, 0, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return payload, opcode, nil
}

func findCodexWindow() uintptr {
	var found uintptr
	callback := syscall.NewCallback(func(hwnd uintptr, lParam uintptr) uintptr {
		if found != 0 {
			return 0
		}
		visible, _, _ := procIsWindowVisible.Call(hwnd)
		if visible == 0 {
			return 1
		}
		title := strings.ToLower(windowText(hwnd))
		if isCodexWindowProcess(hwnd) || strings.Contains(title, "codex") || strings.Contains(title, "openai") {
			found = hwnd
			return 0
		}
		return 1
	})
	procEnumWindows.Call(callback, 0)
	return found
}

func isCodexWindowProcess(hwnd uintptr) bool {
	var pid uint32
	procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return false
	}
	handle, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if handle == 0 {
		return false
	}
	defer procCloseHandle.Call(handle)
	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	ret, _, _ := procQueryFullProcessImageNameW.Call(handle, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if ret == 0 || size == 0 {
		return false
	}
	path := strings.ToLower(syscall.UTF16ToString(buf[:size]))
	return strings.Contains(path, `\openai.codex_`) && strings.HasSuffix(path, `\chatgpt.exe`)
}

func windowText(hwnd uintptr) string {
	n, _, _ := procGetWindowTextLengthW.Call(hwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, int(n)+1)
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf)
}

func focusWindow(hwnd uintptr) bool {
	iconic, _, _ := procIsIconic.Call(hwnd)
	if iconic != 0 {
		procShowWindow.Call(hwnd, swRestore)
	} else {
		procShowWindow.Call(hwnd, swShow)
	}
	procBringWindowToTop.Call(hwnd)
	keyDown(vkMenu)
	ret, _, _ := procSetForegroundWindow2.Call(hwnd)
	keyUp(vkMenu)
	if ret != 0 {
		return true
	}
	time.Sleep(80 * time.Millisecond)
	foreground, _, _ := procGetForegroundWindow.Call()
	return foreground == hwnd
}

type winRect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

func focusCodexComposer(hwnd uintptr) bool {
	var rect winRect
	ret, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rect)))
	if ret == 0 || rect.Right <= rect.Left || rect.Bottom <= rect.Top {
		return false
	}
	var cursor point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))
	x := (rect.Left + rect.Right) / 2
	y := rect.Bottom - 64
	if y <= rect.Top {
		return false
	}
	procSetCursorPos.Call(uintptr(x), uintptr(y))
	procMouseEvent.Call(mouseeventfLeftDown, 0, 0, 0, 0)
	time.Sleep(24 * time.Millisecond)
	procMouseEvent.Call(mouseeventfLeftUp, 0, 0, 0, 0)
	time.Sleep(45 * time.Millisecond)
	procSetCursorPos.Call(uintptr(cursor.x), uintptr(cursor.y))
	return true
}

func setClipboardText(text string) error {
	if err := openClipboardWithRetry(); err != nil {
		return err
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	utf16 := syscall.StringToUTF16(text)
	size := uintptr(len(utf16) * 2)
	hmem, _, _ := procGlobalAlloc.Call(gmemMoveable, size)
	if hmem == 0 {
		return errors.New("GlobalAlloc failed")
	}
	ptr, _, _ := procGlobalLock.Call(hmem)
	if ptr == 0 {
		return errors.New("GlobalLock failed")
	}
	dst := unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), len(utf16))
	copy(dst, utf16)
	procGlobalUnlock.Call(hmem)
	if ret, _, _ := procSetClipboardData.Call(cfUnicodeText, hmem); ret == 0 {
		return errors.New("SetClipboardData failed")
	}
	return nil
}

func setClipboardFiles(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if err := openClipboardWithRetry(); err != nil {
		return err
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	const dropFilesSize = 20
	var names []uint16
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = path
		}
		names = append(names, syscall.StringToUTF16(abs)...)
	}
	names = append(names, 0)
	size := uintptr(dropFilesSize + len(names)*2)
	hmem, _, _ := procGlobalAlloc.Call(gmemMoveable|gmemZeroInit, size)
	if hmem == 0 {
		return errors.New("GlobalAlloc failed")
	}
	ptr, _, _ := procGlobalLock.Call(hmem)
	if ptr == 0 {
		return errors.New("GlobalLock failed")
	}
	mem := unsafe.Slice((*byte)(unsafe.Pointer(ptr)), int(size))
	binary.LittleEndian.PutUint32(mem[0:4], uint32(dropFilesSize))
	binary.LittleEndian.PutUint32(mem[16:20], 1)
	dst := unsafe.Slice((*uint16)(unsafe.Pointer(uintptr(ptr)+dropFilesSize)), len(names))
	copy(dst, names)
	procGlobalUnlock.Call(hmem)
	if ret, _, _ := procSetClipboardData.Call(cfHDrop, hmem); ret == 0 {
		return errors.New("SetClipboardData failed")
	}
	return nil
}

func setClipboardImageDIB(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	img, _, err := image.Decode(file)
	_ = file.Close()
	if err != nil {
		return err
	}
	dib, err := imageToDIB(img)
	if err != nil {
		return err
	}
	if err := openClipboardWithRetry(); err != nil {
		return err
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	hmem, _, _ := procGlobalAlloc.Call(gmemMoveable|gmemZeroInit, uintptr(len(dib)))
	if hmem == 0 {
		return errors.New("GlobalAlloc failed")
	}
	ptr, _, _ := procGlobalLock.Call(hmem)
	if ptr == 0 {
		return errors.New("GlobalLock failed")
	}
	dst := unsafe.Slice((*byte)(unsafe.Pointer(ptr)), len(dib))
	copy(dst, dib)
	procGlobalUnlock.Call(hmem)
	if ret, _, _ := procSetClipboardData.Call(cfDIB, hmem); ret == 0 {
		return errors.New("SetClipboardData failed")
	}
	return nil
}

func imageToDIB(src image.Image) ([]byte, error) {
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return nil, errors.New("invalid image size")
	}
	if w*h > 80_000_000 {
		return nil, errors.New("image too large")
	}
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(rgba, rgba.Bounds(), src, bounds.Min, draw.Src)
	headerSize := 40
	pixelSize := w * h * 4
	dib := make([]byte, headerSize+pixelSize)
	binary.LittleEndian.PutUint32(dib[0:4], uint32(headerSize))
	binary.LittleEndian.PutUint32(dib[4:8], uint32(int32(w)))
	binary.LittleEndian.PutUint32(dib[8:12], uint32(int32(-h)))
	binary.LittleEndian.PutUint16(dib[12:14], 1)
	binary.LittleEndian.PutUint16(dib[14:16], 32)
	binary.LittleEndian.PutUint32(dib[16:20], 0)
	binary.LittleEndian.PutUint32(dib[20:24], uint32(pixelSize))
	offset := headerSize
	for y := 0; y < h; y++ {
		row := rgba.Pix[y*rgba.Stride:]
		for x := 0; x < w; x++ {
			r := row[x*4+0]
			g := row[x*4+1]
			b := row[x*4+2]
			a := row[x*4+3]
			dib[offset+0] = b
			dib[offset+1] = g
			dib[offset+2] = r
			dib[offset+3] = a
			offset += 4
		}
	}
	return dib, nil
}

func openClipboardWithRetry() error {
	var opened uintptr
	for i := 0; i < 16; i++ {
		opened, _, _ = procOpenClipboard.Call(0)
		if opened != 0 {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("clipboard is busy")
}

func sendCtrlV() {
	keyDown(vkControl)
	keyTap(vkV)
	keyUp(vkControl)
}

func sendCtrlA() {
	keyDown(vkControl)
	keyTap(vkA)
	keyUp(vkControl)
}

func sendCtrlVEnter() {
	sendCtrlV()
	time.Sleep(180 * time.Millisecond)
	keyTap(vkReturn)
}

func keyTap(vk uintptr) {
	keyDown(vk)
	time.Sleep(20 * time.Millisecond)
	keyUp(vk)
}

func keyDown(vk uintptr) {
	procKeybdEvent.Call(vk, 0, 0, 0)
}

func keyUp(vk uintptr) {
	procKeybdEvent.Call(vk, 0, keyeventKeyUp, 0)
}

func hiddenCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000,
	}
	return cmd
}

func psQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func openBrowser(url string) error {
	return hiddenCommand("rundll32.exe", "url.dll,FileProtocolHandler", url).Start()
}

const (
	wmCommand                      = 0x0111
	wmDestroy                      = 0x0002
	wmRButtonUp                    = 0x0205
	wmLButtonDbl                   = 0x0203
	wmApp                          = 0x8000
	trayMessage                    = wmApp + 1
	nimAdd                         = 0x00000000
	nimModify                      = 0x00000001
	nimDelete                      = 0x00000002
	nifMessage                     = 0x00000001
	nifIcon                        = 0x00000002
	nifTip                         = 0x00000004
	nifInfo                        = 0x00000010
	niifInfo                       = 0x00000001
	mfString                       = 0x00000000
	mfSeparator                    = 0x00000800
	tpmRightButton                 = 0x0002
	idiApplication                 = 32512
	swHide                         = 0
	swShow                         = 5
	swRestore                      = 9
	cfDIB                          = 8
	cfUnicodeText                  = 13
	cfHDrop                        = 15
	gmemMoveable                   = 0x0002
	gmemZeroInit                   = 0x0040
	keyeventKeyUp                  = 0x0002
	mouseeventfLeftDown            = 0x0002
	mouseeventfLeftUp              = 0x0004
	processQueryLimitedInformation = 0x1000
	vkControl                      = 0x11
	vkA                            = 0x41
	vkV                            = 0x56
	vkReturn                       = 0x0D
	vkEscape                       = 0x1B
	vkMenu                         = 0x12
)

const (
	menuOpen = 1001 + iota
	menuPair
	menuStartup
	menuExit
)

var (
	user32                         = syscall.NewLazyDLL("user32.dll")
	shell32                        = syscall.NewLazyDLL("shell32.dll")
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procRegisterClassExW           = user32.NewProc("RegisterClassExW")
	procCreateWindowExW            = user32.NewProc("CreateWindowExW")
	procDefWindowProcW             = user32.NewProc("DefWindowProcW")
	procGetMessageW                = user32.NewProc("GetMessageW")
	procTranslateMessage           = user32.NewProc("TranslateMessage")
	procDispatchMessageW           = user32.NewProc("DispatchMessageW")
	procPostQuitMessage            = user32.NewProc("PostQuitMessage")
	procLoadIconW                  = user32.NewProc("LoadIconW")
	procLoadImageW                 = user32.NewProc("LoadImageW")
	procMessageBoxW                = user32.NewProc("MessageBoxW")
	procEnumWindows                = user32.NewProc("EnumWindows")
	procIsWindowVisible            = user32.NewProc("IsWindowVisible")
	procIsIconic                   = user32.NewProc("IsIconic")
	procGetWindowTextLengthW       = user32.NewProc("GetWindowTextLengthW")
	procGetWindowTextW             = user32.NewProc("GetWindowTextW")
	procGetWindowThreadProcessID   = user32.NewProc("GetWindowThreadProcessId")
	procShowWindow                 = user32.NewProc("ShowWindow")
	procGetWindowRect              = user32.NewProc("GetWindowRect")
	procGetForegroundWindow        = user32.NewProc("GetForegroundWindow")
	procSetForegroundWindow2       = user32.NewProc("SetForegroundWindow")
	procBringWindowToTop           = user32.NewProc("BringWindowToTop")
	procKeybdEvent                 = user32.NewProc("keybd_event")
	procSetCursorPos               = user32.NewProc("SetCursorPos")
	procMouseEvent                 = user32.NewProc("mouse_event")
	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procEmptyClipboard             = user32.NewProc("EmptyClipboard")
	procSetClipboardData           = user32.NewProc("SetClipboardData")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procCreatePopupMenu            = user32.NewProc("CreatePopupMenu")
	procAppendMenuW                = user32.NewProc("AppendMenuW")
	procTrackPopupMenu             = user32.NewProc("TrackPopupMenu")
	procDestroyMenu                = user32.NewProc("DestroyMenu")
	procSetForegroundWindow        = user32.NewProc("SetForegroundWindow")
	procGetCursorPos               = user32.NewProc("GetCursorPos")
	procShellNotifyIconW           = shell32.NewProc("Shell_NotifyIconW")
	procSetCurrentProcessAppID     = shell32.NewProc("SetCurrentProcessExplicitAppUserModelID")
	procGetModuleHandleW           = kernel32.NewProc("GetModuleHandleW")
	procGlobalAlloc                = kernel32.NewProc("GlobalAlloc")
	procGlobalLock                 = kernel32.NewProc("GlobalLock")
	procGlobalUnlock               = kernel32.NewProc("GlobalUnlock")
	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	procCloseHandle                = kernel32.NewProc("CloseHandle")
	trayState                      *trayContext
)

type trayContext struct {
	hwnd     uintptr
	icon     uintptr
	state    *serverState
	localURL string
}

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type point struct {
	x int32
	y int32
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type notifyIconData struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uTimeoutVersion  uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

func runTray(state *serverState, localURL string) error {
	className := syscall.StringToUTF16Ptr("CodexRemoteWinTrayWindow")
	hInstance, _, _ := procGetModuleHandleW.Call(0)
	icon := loadAppIcon(hInstance, 32, 32)
	iconSmall := loadAppIcon(hInstance, 16, 16)
	wndProc := syscall.NewCallback(trayWndProc)
	wc := wndClassEx{
		cbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		lpfnWndProc:   wndProc,
		hInstance:     hInstance,
		hIcon:         icon,
		lpszClassName: className,
		hIconSm:       iconSmall,
	}
	if ret, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
		return fmt.Errorf("RegisterClassExW failed: %v", err)
	}
	hwnd, _, err := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(appName))),
		0,
		0, 0, 0, 0,
		0, 0, hInstance, 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowExW failed: %v", err)
	}
	trayState = &trayContext{hwnd: hwnd, icon: iconSmall, state: state, localURL: localURL}
	addTrayIcon(trayState, "Codex Remote Win", "程序正在运行。配对码："+state.pairCode)
	defer removeTrayIcon(trayState)
	messageLoop()
	return nil
}

func loadAppIcon(hInstance uintptr, width, height int) uintptr {
	const imageIcon = 1
	icon, _, _ := procLoadImageW.Call(hInstance, uintptr(appIconResourceID), imageIcon, uintptr(width), uintptr(height), 0)
	if icon != 0 {
		return icon
	}
	icon, _, _ = procLoadIconW.Call(0, uintptr(idiApplication))
	return icon
}

func trayWndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case trayMessage:
		if lParam == wmRButtonUp {
			showTrayMenu(hwnd)
			return 0
		}
		if lParam == wmLButtonDbl {
			if trayState != nil {
				_ = openBrowser(trayState.localURL)
			}
			return 0
		}
	case wmCommand:
		switch uint32(wParam & 0xffff) {
		case menuOpen:
			if trayState != nil {
				_ = openBrowser(trayState.localURL)
			}
		case menuPair:
			if trayState != nil {
				showMessage("配对信息", "配对码："+trayState.state.pairCode+"\n\n访问地址："+trayState.localURL)
			}
		case menuStartup:
			if startupEnabled() {
				if err := setStartup(false); err != nil {
					showMessage("开机启动", "无法关闭开机启动：\n"+err.Error())
				} else {
					showMessage("开机启动", "已关闭开机启动。")
				}
			} else {
				if err := setStartup(true); err != nil {
					showMessage("开机启动", "无法启用开机启动：\n"+err.Error())
				} else {
					showMessage("开机启动", "已启用开机启动。")
				}
			}
		case menuExit:
			procPostQuitMessage.Call(0)
		}
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
	return ret
}

func messageLoop() {
	var m msg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func addTrayIcon(ctx *trayContext, tip, info string) {
	var nid notifyIconData
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = ctx.hwnd
	nid.uID = 1
	nid.uFlags = nifMessage | nifIcon | nifTip | nifInfo
	nid.uCallbackMessage = trayMessage
	nid.hIcon = ctx.icon
	copy(nid.szTip[:], syscall.StringToUTF16(tip))
	copy(nid.szInfo[:], syscall.StringToUTF16(info))
	copy(nid.szInfoTitle[:], syscall.StringToUTF16(appName))
	nid.dwInfoFlags = niifInfo
	procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
}

func removeTrayIcon(ctx *trayContext) {
	if ctx == nil {
		return
	}
	var nid notifyIconData
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = ctx.hwnd
	nid.uID = 1
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
}

func showTrayMenu(hwnd uintptr) {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)
	appendMenu(menu, menuOpen, "打开网页控制台")
	appendMenu(menu, menuPair, "显示配对码")
	procAppendMenuW.Call(menu, mfSeparator, 0, 0)
	if startupEnabled() {
		appendMenu(menu, menuStartup, "关闭开机启动")
	} else {
		appendMenu(menu, menuStartup, "启用开机启动")
	}
	procAppendMenuW.Call(menu, mfSeparator, 0, 0)
	appendMenu(menu, menuExit, "退出程序")
	var p point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	procSetForegroundWindow.Call(hwnd)
	procTrackPopupMenu.Call(menu, tpmRightButton, uintptr(p.x), uintptr(p.y), 0, hwnd, 0)
}

func appendMenu(menu uintptr, id uintptr, text string) {
	procAppendMenuW.Call(menu, mfString, id, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(text))))
}

func showMessage(title, body string) {
	procMessageBoxW.Call(0,
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(body))),
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(title))),
		0,
	)
}

func startupEnabled() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	out, err := hiddenCommand("reg.exe", "query", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "/v", "CodexRemoteWin").CombinedOutput()
	return err == nil && strings.Contains(strings.ToLower(string(out)), strings.ToLower(exe))
}

func setStartup(enabled bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	key := `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	if enabled {
		return hiddenCommand("reg.exe", "add", key, "/v", "CodexRemoteWin", "/t", "REG_SZ", "/d", exe, "/f").Run()
	}
	return hiddenCommand("reg.exe", "delete", key, "/v", "CodexRemoteWin", "/f").Run()
}

const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<title>Codex Remote Win</title>
<style>
:root{--ink:#17212b;--paper:#fff;--panel:#f6f8f5;--line:#d7ded6;--muted:#67746f;--mint:#2f9c78;--red:#c94f43;--field:#edf3ee;font-family:"Segoe UI","Microsoft YaHei",system-ui,sans-serif;color:var(--ink);background:#dfe7df}*{box-sizing:border-box}body{margin:0;min-height:100vh;background:linear-gradient(90deg,rgba(23,33,43,.05) 1px,transparent 1px),linear-gradient(0deg,rgba(23,33,43,.04) 1px,transparent 1px),#dfe7df;background-size:32px 32px}.app{width:min(1120px,100%);min-height:100vh;margin:0 auto;background:rgba(246,248,245,.96);display:grid;grid-template-rows:auto 1fr}.top{display:grid;grid-template-columns:1fr auto;gap:12px;align-items:center;padding:12px 16px;background:var(--paper);border-bottom:1px solid var(--line)}.brand{display:flex;gap:12px;align-items:center;min-width:0}.mark{width:38px;height:38px;border-radius:7px;background:var(--ink);color:#d7fff0;display:grid;place-items:center;font-family:Consolas,monospace;font-weight:800}h1{font-size:18px;margin:0}.sub{font-size:12px;color:var(--muted);margin-top:3px}.pill{display:inline-flex;align-items:center;gap:7px;min-height:32px;padding:0 10px;border:1px solid var(--line);border-radius:999px;background:var(--field);font-size:12px;color:var(--muted)}.dot{width:8px;height:8px;border-radius:50%;background:#c98519}.dot.ok{background:var(--mint)}.dot.bad{background:var(--red)}.main{display:grid;grid-template-columns:320px 1fr;min-height:0}.side{border-right:1px solid var(--line);background:#f0f5ef;display:grid;grid-template-rows:auto 1fr}.sidehead{padding:12px;display:grid;grid-template-columns:1fr auto;gap:8px;border-bottom:1px solid var(--line)}button,input,textarea{font:inherit}button{min-height:42px;border:1px solid var(--line);border-radius:7px;background:var(--paper);color:var(--ink)}button:disabled{opacity:.55}.icon{width:42px;font-weight:800}.threads{padding:8px;overflow:auto}.thread{width:100%;min-height:74px;text-align:left;padding:10px;margin-bottom:8px;background:transparent;border-color:transparent;display:grid;gap:5px}.thread.active,.thread:hover{background:var(--paper);border-color:var(--line)}.name{font-weight:800;line-height:1.25}.meta{font-size:12px;color:var(--muted);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.work{display:grid;grid-template-rows:auto 1fr auto;min-width:0}.head{padding:12px 16px;display:grid;grid-template-columns:1fr auto;gap:12px;border-bottom:1px solid var(--line);align-items:center}.title{font-weight:800;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.path{font-size:12px;color:var(--muted);font-family:Consolas,monospace;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.messages{padding:16px;overflow:auto;display:flex;flex-direction:column;gap:12px}.msg{max-width:min(760px,94%);display:grid;gap:5px}.msg.user{align-self:end}.bubble{padding:12px 13px;background:var(--paper);border:1px solid var(--line);border-radius:8px;white-space:pre-wrap;overflow-wrap:anywhere;line-height:1.55}.user .bubble{background:#e6f4ee;border-color:#bdd9cf}.empty{margin:auto;max-width:440px;color:var(--muted);line-height:1.6;text-align:center;padding:24px}.composer{padding:12px;border-top:1px solid var(--line);background:var(--paper);display:grid;grid-template-columns:1fr auto;gap:10px;align-items:end}textarea{width:100%;min-height:46px;max-height:180px;border:1px solid var(--line);border-radius:8px;background:var(--field);padding:11px 12px;resize:vertical}.send{min-width:86px;background:var(--ink);color:#fff;border-color:var(--ink);font-weight:800}.overlay{position:fixed;inset:0;background:rgba(23,33,43,.58);display:grid;place-items:center;padding:24px}.pair{width:min(420px,100%);background:var(--paper);border:1px solid var(--line);border-radius:8px;padding:22px;display:grid;gap:14px;box-shadow:0 18px 48px rgba(24,33,43,.16)}.pair h2{margin:0}.pair p{margin:0;color:var(--muted);line-height:1.5}.pairrow{display:grid;grid-template-columns:1fr auto;gap:10px}input{min-height:46px;border:1px solid var(--line);border-radius:8px;background:var(--field);padding:0 12px;font-size:20px;font-family:Consolas,monospace}.primary{padding:0 16px;background:var(--mint);border-color:var(--mint);color:white;font-weight:800}.notice{min-height:20px;color:var(--red);font-size:13px}.hidden{display:none!important}@media(max-width:760px){.top{grid-template-columns:1fr}.main{grid-template-columns:1fr;grid-template-rows:190px 1fr}.side{border-right:0;border-bottom:1px solid var(--line)}.threads{display:flex;gap:8px;overflow-x:auto;overflow-y:hidden}.thread{min-width:220px;margin-bottom:0}.composer{grid-template-columns:1fr}.send{width:100%}}
</style>
</head>
<body>
<div class="app">
<header class="top"><div class="brand"><div class="mark">C&gt;</div><div><h1>Codex Remote Win</h1><div class="sub" id="sub">本机安全桥接</div></div></div><div class="pill"><span class="dot" id="healthDot"></span><span id="healthText">检查中</span></div></header>
<main class="main"><aside class="side"><div class="sidehead"><div><b>Threads</b><div class="meta" id="count">0 个线程</div></div><button class="icon" id="refresh" title="刷新">R</button></div><div class="threads" id="threads"></div></aside>
<section class="work"><div class="head"><div><div class="title" id="title">选择一个线程</div><div class="path" id="path"></div></div><span class="pill"><span class="dot" id="runDot"></span><span id="runText">空闲</span></span></div><div class="messages" id="messages"><div class="empty">输入启动窗口里的配对码后，这里会显示 Windows 上的 Codex 会话。</div></div><form class="composer" id="composer"><textarea id="input" placeholder="发给 Codex..." maxlength="8000"></textarea><button class="send" id="send">发送</button></form></section></main>
</div>
<div class="overlay" id="overlay"><form class="pair" id="pair"><h2>配对这台设备</h2><p>输入 Windows 可执行程序窗口中显示的 6 位配对码。令牌只保存在当前浏览器，不放进 URL。</p><div class="pairrow"><input id="code" inputmode="numeric" maxlength="12" placeholder="000000"><button class="primary">配对</button></div><div class="notice" id="notice"></div></form></div>
<script>
const $=id=>document.getElementById(id);const st={token:localStorage.getItem('crw.token')||'',threads:[],selected:localStorage.getItem('crw.thread')||'',timer:null};const els={overlay:$('overlay'),notice:$('notice'),code:$('code'),threads:$('threads'),count:$('count'),title:$('title'),path:$('path'),messages:$('messages'),input:$('input'),send:$('send'),healthDot:$('healthDot'),healthText:$('healthText'),runDot:$('runDot'),runText:$('runText'),sub:$('sub')};function api(p,o={}){const h=o.headers||{};if(st.token)h.authorization='Bearer '+st.token;return fetch(p,{...o,headers:h})}function showPair(v){els.overlay.classList.toggle('hidden',!v);if(v)setTimeout(()=>els.code.focus(),80)}function setDot(el,kind){el.className='dot '+(kind||'')}function msg(role,text,label){const a=document.createElement('article');a.className='msg '+role;const m=document.createElement('div');m.className='meta';m.textContent=label||(role==='user'?'你':'Codex');const b=document.createElement('div');b.className='bubble';b.textContent=text||'';a.append(m,b);els.messages.appendChild(a);els.messages.scrollTop=els.messages.scrollHeight}function clear(t){els.messages.textContent='';if(t){const e=document.createElement('div');e.className='empty';e.textContent=t;els.messages.appendChild(e)}}function time(v){const t=Date.parse(v||'');return Number.isFinite(t)?new Date(t).toLocaleString([],{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'}):''}function selected(){return st.threads.find(x=>x.id===st.selected)}function renderThreads(){els.threads.textContent='';els.count.textContent=st.threads.length+' 个线程';for(const x of st.threads){const b=document.createElement('button');b.type='button';b.className='thread '+(x.id===st.selected?'active':'');b.onclick=()=>selectThread(x.id);const n=document.createElement('div');n.className='name';n.textContent=x.title||'(untitled)';const m=document.createElement('div');m.className='meta';m.textContent=(x.active?'运行中':'空闲')+' · '+time(x.updatedAt);const p=document.createElement('div');p.className='meta';p.textContent=x.cwd||x.preview||'';b.append(n,m,p);els.threads.appendChild(b)}renderHead()}function renderHead(){const x=selected();els.title.textContent=x?(x.title||'(untitled)'):'选择一个线程';els.path.textContent=x?(x.cwd||x.sessionFile||''):'';setDot(els.runDot,x&&x.active?'ok':'');els.runText.textContent=x&&x.active?'运行中':'空闲'}async function health(){try{const r=await api('/api/health');const d=await r.json();setDot(els.healthDot,'ok');els.healthText.textContent=st.token?'已配对':'等待配对';els.sub.textContent=(d.host||'Windows')+' · '+(d.bind||'127.0.0.1')}catch{setDot(els.healthDot,'bad');els.healthText.textContent='离线'}}async function loadThreads(){const r=await api('/api/threads');const d=await r.json().catch(()=>({}));if(r.status===401)return expire();if(!r.ok||!d.ok)throw new Error(d.message||'读取线程失败');st.threads=d.threads||[];if(!st.selected||!st.threads.some(x=>x.id===st.selected))st.selected=st.threads[0]?.id||'';if(st.selected)localStorage.setItem('crw.thread',st.selected);renderThreads();if(st.selected)await history(st.selected);else clear('还没有找到 Codex 会话。先在 Windows 上打开 Codex Desktop。')}async function history(id){clear('加载中...');const r=await api('/api/history?thread='+encodeURIComponent(id));const d=await r.json().catch(()=>({}));if(r.status===401)return expire();if(!r.ok||!d.ok)throw new Error(d.message||'读取历史失败');clear('');if(!d.available||!d.messages?.length)return clear('这个线程暂无可显示的历史。');for(const x of d.messages)msg(x.role,x.text,x.role==='user'?'你':'Codex')}async function selectThread(id){st.selected=id;localStorage.setItem('crw.thread',id);renderThreads();await history(id);startPoll()}async function poll(){if(!st.selected)return;try{const r=await api('/api/status?thread='+encodeURIComponent(st.selected));const d=await r.json();if(r.status===401)return expire();setDot(els.runDot,d.active?'ok':'');els.runText.textContent=d.active?'运行中':'空闲'}catch{}}function startPoll(){if(st.timer)clearInterval(st.timer);poll();st.timer=setInterval(poll,2500)}async function send(text){els.send.disabled=true;msg('user',text,'你');els.input.value='';try{const r=await api('/api/send',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({text,threadId:st.selected||''})});const d=await r.json().catch(()=>({}));if(r.status===401)return expire();if(!r.ok||!d.ok)throw new Error(d.message||'发送失败');setDot(els.runDot,'ok');els.runText.textContent='已发送';startPoll()}catch(e){msg('assistant',e.message||'发送失败','系统')}finally{els.send.disabled=false;els.input.focus()}}function expire(){st.token='';localStorage.removeItem('crw.token');showPair(true);clear('会话已过期，请重新配对。')}async function boot(){await health();if(!st.token)return showPair(true);showPair(false);try{await loadThreads();startPoll()}catch(e){clear(e.message||'启动失败')}}$('pair').onsubmit=e=>{e.preventDefault();els.notice.textContent='';fetch('/api/pair',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({code:els.code.value.trim(),deviceName:navigator.userAgent.slice(0,60)})}).then(r=>r.json().then(d=>{if(!r.ok||!d.ok)throw new Error(d.message||'配对失败');st.token=d.token;localStorage.setItem('crw.token',st.token);showPair(false);boot()})).catch(e=>els.notice.textContent=e.message||'配对失败')};$('composer').onsubmit=e=>{e.preventDefault();const t=els.input.value.trim();if(t)send(t)};$('refresh').onclick=()=>loadThreads().catch(e=>clear(e.message||'刷新失败'));window.addEventListener('focus',()=>{health();if(st.token)loadThreads().catch(()=>{})});boot();
</script>
</body>
</html>`

const indexHTMLMobileAttachments = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, minimum-scale=1, maximum-scale=1, user-scalable=no, viewport-fit=cover">
<meta name="theme-color" content="#101214">
<link rel="manifest" href="/manifest.webmanifest">
<title>Codex Remote Win</title>
<style>
:root{
  --bg:#101214;
  --top:#15181b;
  --panel:#1b2024;
  --panel2:#242a30;
  --text:#f5f7f8;
  --muted:#a7b0b8;
  --faint:#707b84;
  --line:rgba(255,255,255,.1);
  --user:#28403a;
  --accent:#9ee6c3;
  --warn:#ffb86b;
  --danger:#ff8a8a;
  --bottom:max(14px,env(safe-area-inset-bottom));
  font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",Arial,sans-serif;
  color:var(--text);
}
*{box-sizing:border-box}
html,body{width:100%;height:100%;margin:0;background:var(--bg);overflow:hidden}
body{position:fixed;inset:0;-webkit-font-smoothing:antialiased}
button,input,textarea{font:inherit}
button{border:0;background:none;color:inherit}
.app{position:fixed;inset:0;display:grid;grid-template-rows:auto 1fr auto;background:var(--bg)}
.topbar{height:calc(54px + env(safe-area-inset-top));padding:env(safe-area-inset-top) 12px 0;background:var(--top);display:flex;align-items:center;gap:8px;border-bottom:1px solid var(--line);z-index:5}
.threadBtn{height:34px;min-width:0;max-width:min(58vw,440px);padding:0 12px;border-radius:8px;background:var(--panel);display:flex;align-items:center;gap:8px;font-weight:750;font-size:14px}
.liveDot{width:8px;height:8px;border-radius:50%;background:var(--accent);box-shadow:0 0 10px rgba(158,230,195,.55);flex:0 0 auto}
.threadTitle{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.topActions{margin-left:auto;display:flex;gap:7px;align-items:center}
.badge{height:30px;min-width:30px;padding:0 9px;border-radius:8px;background:var(--panel);color:var(--muted);display:flex;align-items:center;gap:6px;font-size:12px;font-weight:650}
.statusDot{width:7px;height:7px;border-radius:50%;background:var(--faint)}
.statusDot.ok{background:var(--accent)}
.statusDot.bad{background:var(--danger)}
.iconBtn{width:34px;height:34px;border-radius:8px;background:var(--panel);display:grid;place-items:center;color:var(--muted);font-weight:800}
.contextBadge{min-width:46px;justify-content:center}
.contextBadge.warn{color:var(--warn)}
.contextBadge.danger{color:var(--danger)}
.actionMenu{position:fixed;right:10px;top:calc(62px + env(safe-area-inset-top));z-index:35;display:none;width:min(220px,calc(100vw - 20px));padding:6px;border:1px solid var(--line);border-radius:8px;background:#15191d;box-shadow:0 18px 48px rgba(0,0,0,.4)}
.actionMenu.open{display:grid;gap:4px}
.actionMenu button{height:38px;border-radius:7px;padding:0 10px;text-align:left;color:var(--text);background:transparent}
.actionMenu button:active{background:var(--panel2)}
.actionMenu .danger{color:var(--danger)}
.pinMark{color:var(--accent);margin-right:4px}
.rail{position:fixed;left:10px;right:10px;top:calc(62px + env(safe-area-inset-top));max-height:min(62vh,520px);display:none;z-index:25;background:#15191d;border:1px solid var(--line);border-radius:8px;overflow:hidden;box-shadow:0 18px 48px rgba(0,0,0,.4)}
.rail.open{display:grid;grid-template-rows:42px 1fr}
.railHead{padding:0 10px;border-bottom:1px solid var(--line);display:flex;align-items:center;justify-content:space-between;color:var(--muted);font-size:12px;font-weight:700}
.threads{overflow:auto;padding:7px;-webkit-overflow-scrolling:touch}
.thread{width:100%;min-height:58px;padding:8px 9px;border-radius:8px;display:grid;gap:4px;text-align:left}
.thread.active,.thread:active{background:var(--panel2)}
.threadName{font-size:14px;font-weight:700;line-height:1.25;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.threadMeta{font-size:11px;color:var(--faint);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.messages{min-height:0;overflow:auto;-webkit-overflow-scrolling:touch;overscroll-behavior:contain;padding:16px max(12px,calc((100vw - 860px)/2)) 8px}
.message{display:flex;margin:0 0 17px}
.message.user{justify-content:flex-end}
.message.assistant{justify-content:flex-start}
.wrap{max-width:min(760px,88%)}
.message.assistant .wrap{width:100%;max-width:none}
.message.user .wrap{max-width:min(720px,84%)}
.meta{margin:0 0 6px;color:var(--faint);font-size:12px;line-height:1}
.message.user .meta{text-align:right}
.bubble{font-size:15px;line-height:1.6;white-space:pre-wrap;overflow-wrap:anywhere}
.message.user .bubble{padding:10px 13px;border-radius:8px;background:var(--user);line-height:1.48}
.message.assistant .bubble{padding:0}
.bubble:empty{display:none}
.bubbleImages{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:7px;margin-top:7px}
.bubbleImages img{width:100%;aspect-ratio:1.15;object-fit:cover;border-radius:8px;border:1px solid var(--line);background:var(--panel)}
.empty{height:100%;min-height:260px;display:grid;place-items:center;text-align:center;color:var(--muted);font-size:14px;line-height:1.65;padding:28px}
.composerShell{position:relative;z-index:6;padding:7px max(10px,calc((100vw - 860px)/2)) var(--bottom);background:linear-gradient(180deg,rgba(16,18,20,0),var(--bg) 24%)}
.tray{display:none;gap:8px;overflow:auto;padding:0 2px 8px;-webkit-overflow-scrolling:touch}
.tray.hasItems{display:flex}
.chip{position:relative;flex:0 0 68px;width:68px;height:68px;border:1px solid var(--line);border-radius:8px;overflow:hidden;background:var(--panel)}
.chip img{width:100%;height:100%;object-fit:cover;display:block}
.chip.file{padding:8px;display:grid;align-content:end;gap:4px}
.chipIcon{font-size:20px;line-height:1}
.chipName{font-size:10px;line-height:1.15;color:var(--muted);overflow:hidden;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical}
.chip button{position:absolute;right:3px;top:3px;width:22px;height:22px;border-radius:50%;background:rgba(0,0,0,.65);display:grid;place-items:center;font-size:15px;line-height:1}
.composer{display:grid;grid-template-columns:36px minmax(0,1fr) 38px;align-items:end;gap:7px;padding:7px;border:1px solid var(--line);border-radius:8px;background:#171b1f}
.attach,.send{width:36px;height:36px;border-radius:8px;display:grid;place-items:center;font-size:22px;font-weight:800}
.attach{background:var(--panel2);color:var(--muted)}
.send{background:var(--accent);color:#0b1511}
.send:disabled{opacity:.5}
.fileInput{display:none}
textarea{width:100%;min-height:36px;max-height:148px;resize:none;border:0;outline:0;background:transparent;color:var(--text);padding:7px 1px;font-size:16px;line-height:1.38}
textarea::placeholder{color:var(--faint)}
.overlay{position:fixed;inset:0;z-index:50;background:rgba(0,0,0,.72);display:grid;place-items:center;padding:22px;backdrop-filter:blur(10px)}
.pair{width:min(420px,100%);border:1px solid var(--line);border-radius:8px;background:#15191d;padding:18px;display:grid;gap:13px;box-shadow:0 24px 70px rgba(0,0,0,.55)}
.pair h2{margin:0;font-size:22px;letter-spacing:0}
.pair p{margin:0;color:var(--muted);line-height:1.55;font-size:14px}
.pairRow{display:grid;grid-template-columns:1fr 82px;gap:8px}
input{height:46px;border:1px solid var(--line);border-radius:8px;background:#1b2024;color:var(--text);padding:0 12px;font-size:20px;font-family:Consolas,Menlo,monospace;outline:0}
.primary{height:46px;border-radius:8px;background:var(--accent);color:#0b1511;font-weight:850}
.notice{min-height:18px;color:var(--danger);font-size:13px}
.hidden{display:none!important}
@media(min-width:900px) and (min-height:650px){
  .app{grid-template-columns:clamp(304px,28vw,356px) minmax(0,1fr);grid-template-rows:auto 1fr auto}
  .topbar{grid-column:1/-1}
  .rail{display:grid;position:relative;left:auto;right:auto;top:auto;grid-column:1;grid-row:2/4;max-height:none;margin:12px 0 12px 12px;box-shadow:none}
  .rail:not(.open){display:grid}
  .messages{grid-column:2;grid-row:2;padding-top:20px}
  .composerShell{grid-column:2;grid-row:3}
  .threadBtn{pointer-events:none}
}
@media(max-width:520px){
  .badgeText{display:none}
  .threadBtn{max-width:calc(100vw - 148px)}
  .messages{padding-left:12px;padding-right:12px}
  .wrap,.message.user .wrap{max-width:90%}
  .message.assistant .wrap{width:100%;max-width:none}
  .composerShell{padding-left:10px;padding-right:10px}
}
</style>
</head>
<body>
<div class="app">
  <header class="topbar">
    <button class="threadBtn" id="threadButton" type="button"><span class="liveDot"></span><span class="threadTitle" id="threadTitle">选择线程</span></button>
    <div class="topActions">
      <span class="badge contextBadge" id="contextBadge">--</span>
      <span class="badge"><span class="statusDot" id="runDot"></span><span class="badgeText" id="runText">空闲</span></span>
      <span class="badge"><span class="statusDot" id="healthDot"></span><span class="badgeText" id="healthText">连接</span></span>
      <button class="iconBtn" id="newThread" title="新建线程" type="button">+</button>
      <button class="iconBtn" id="refresh" title="刷新" type="button">R</button>
      <button class="iconBtn" id="more" title="线程操作" type="button">⋯</button>
    </div>
  </header>
  <div class="actionMenu" id="actionMenu">
    <button id="pinAction" type="button">置顶线程</button>
    <button id="renameAction" type="button">重命名</button>
    <button id="stopAction" type="button">停止回复</button>
    <button class="danger" id="archiveAction" type="button">归档线程</button>
  </div>
  <aside class="rail" id="rail">
    <div class="railHead"><span id="count">0 threads</span><button class="iconBtn" id="closeRail" type="button">X</button></div>
    <div class="threads" id="threads"></div>
  </aside>
  <section class="messages" id="messages"><div class="empty">配对后会显示 Windows 上的 Codex 会话。</div></section>
  <form class="composerShell" id="composer">
    <div class="tray" id="tray"></div>
    <div class="composer">
      <button class="attach" id="attach" type="button" title="添加附件">+</button>
      <input class="fileInput" id="fileInput" type="file" multiple>
      <textarea id="input" placeholder="发给 Codex..." maxlength="8000" rows="1"></textarea>
      <button class="send" id="send" type="submit" title="发送">↑</button>
    </div>
  </form>
</div>
<div class="overlay" id="overlay">
  <form class="pair" id="pair">
    <h2>配对这台设备</h2>
    <p>输入 Windows 托盘菜单里显示的配对码。配对后本浏览器会长期保存访问令牌。</p>
    <div class="pairRow"><input id="code" inputmode="numeric" maxlength="12" placeholder="000000"><button class="primary" type="submit">配对</button></div>
    <div class="notice" id="notice"></div>
  </form>
</div>
<script>
const $=id=>document.getElementById(id);
const st={token:localStorage.getItem('crw.token')||'',threads:[],selected:localStorage.getItem('crw.thread')||'',timer:null,attachments:[]};
const el={rail:$('rail'),threadButton:$('threadButton'),threadTitle:$('threadTitle'),threads:$('threads'),count:$('count'),messages:$('messages'),input:$('input'),send:$('send'),overlay:$('overlay'),code:$('code'),notice:$('notice'),healthDot:$('healthDot'),healthText:$('healthText'),runDot:$('runDot'),runText:$('runText'),tray:$('tray'),file:$('fileInput'),attach:$('attach'),context:$('contextBadge'),newThread:$('newThread'),more:$('more'),actionMenu:$('actionMenu'),pinAction:$('pinAction'),renameAction:$('renameAction'),stopAction:$('stopAction'),archiveAction:$('archiveAction')};
function api(p,o={}){const h=o.headers||{};if(st.token)h.authorization='Bearer '+st.token;return fetch(p,{...o,headers:h})}
function dot(node,kind){node.className='statusDot '+(kind||'')}
function showPair(v){el.overlay.classList.toggle('hidden',!v);if(v)setTimeout(()=>el.code.focus(),80)}
function empty(t){el.messages.textContent='';const d=document.createElement('div');d.className='empty';d.textContent=t;el.messages.appendChild(d)}
function selected(){return st.threads.find(x=>x.id===st.selected)}
function when(v){const t=Date.parse(v||'');return Number.isFinite(t)?new Date(t).toLocaleString([],{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'}):''}
function contextText(x){const c=x&&x.context;if(!c||!c.available)return '--';return Math.round(c.percent)+'%'}
function setHeader(){const x=selected();el.threadTitle.textContent=x?(x.pinned?'★ ':'')+(x.title||'(untitled)'):'选择线程';dot(el.runDot,x&&x.active?'ok':'');el.runText.textContent=x&&x.active?'运行中':'空闲';el.context.textContent=contextText(x);el.context.className='badge contextBadge '+(x&&x.context&&x.context.percent>=90?'danger':x&&x.context&&x.context.percent>=75?'warn':'');el.pinAction.textContent=x&&x.pinned?'取消置顶':'置顶线程'}
function renderThreads(){el.threads.textContent='';el.count.textContent=st.threads.length+' threads';for(const x of st.threads){const b=document.createElement('button');b.type='button';b.className='thread '+(x.id===st.selected?'active':'');b.onclick=()=>selectThread(x.id);const n=document.createElement('div');n.className='threadName';n.innerHTML=(x.pinned?'<span class="pinMark">★</span>':'')+escapeHTML(x.title||'(untitled)');const m=document.createElement('div');m.className='threadMeta';m.textContent=(x.active?'运行中':'空闲')+' · '+contextText(x)+' · '+when(x.updatedAt);const p=document.createElement('div');p.className='threadMeta';p.textContent=x.cwd||x.preview||'';b.append(n,m,p);el.threads.appendChild(b)}setHeader()}
function escapeHTML(s){return String(s).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
function isImage(item){return (item.type||'').startsWith('image/')}
function shortName(name){name=String(name||'附件');return name.length>18?name.slice(0,8)+'…'+name.slice(-7):name}
function addMsg(role,text,label,attachments){const a=document.createElement('article');a.className='message '+role;const w=document.createElement('div');w.className='wrap';const m=document.createElement('div');m.className='meta';m.textContent=label||(role==='user'?'你':'Codex');const b=document.createElement('div');b.className='bubble';b.textContent=text||'';w.append(m,b);if(attachments&&attachments.length){const imgs=document.createElement('div');imgs.className='bubbleImages';for(const item of attachments){if(isImage(item)){const img=document.createElement('img');img.src=item.dataUrl;img.alt=item.name||'图片';imgs.appendChild(img)}else{const card=document.createElement('div');card.className='chip file';card.style.width='100%';card.style.height='68px';const icon=document.createElement('div');icon.className='chipIcon';icon.textContent='📎';const nm=document.createElement('div');nm.className='chipName';nm.textContent=item.name||'附件';card.append(icon,nm);imgs.appendChild(card)}}w.appendChild(imgs)}a.appendChild(w);el.messages.appendChild(a);el.messages.scrollTop=el.messages.scrollHeight}
function renderTray(){el.tray.textContent='';el.tray.classList.toggle('hasItems',st.attachments.length>0);st.attachments.forEach((item,index)=>{const chip=document.createElement('div');chip.className='chip '+(isImage(item)?'':'file');if(isImage(item)){const img=document.createElement('img');img.src=item.dataUrl;img.alt=item.name||'图片';chip.appendChild(img)}else{const icon=document.createElement('div');icon.className='chipIcon';icon.textContent='📎';const nm=document.createElement('div');nm.className='chipName';nm.textContent=shortName(item.name);chip.append(icon,nm)}const rm=document.createElement('button');rm.type='button';rm.textContent='×';rm.title='移除附件';rm.onclick=()=>{st.attachments.splice(index,1);renderTray()};chip.appendChild(rm);el.tray.appendChild(chip)})}
function fileToDataURL(file){return new Promise((resolve,reject)=>{const reader=new FileReader();reader.onload=()=>resolve(reader.result);reader.onerror=()=>reject(reader.error||new Error('读取附件失败'));reader.readAsDataURL(file)})}
async function health(){try{const r=await api('/api/health');const d=await r.json().catch(()=>({}));dot(el.healthDot,'ok');el.healthText.textContent=st.token?(d.cdp&&d.cdp.available?'CDP':'粘贴'):'待配对'}catch{dot(el.healthDot,'bad');el.healthText.textContent='离线'}}
async function loadThreads(){const r=await api('/api/threads');const d=await r.json().catch(()=>({}));if(r.status===401)return expire();if(!r.ok||!d.ok)throw new Error(d.message||'读取线程失败');st.threads=d.threads||[];if(!st.selected||!st.threads.some(x=>x.id===st.selected))st.selected=st.threads[0]?.id||'';if(st.selected)localStorage.setItem('crw.thread',st.selected);renderThreads();if(st.selected)await history(st.selected);else empty('还没有找到 Codex 会话。先在 Windows 上打开 Codex Desktop。')}
async function history(id){empty('加载中...');const r=await api('/api/history?thread='+encodeURIComponent(id));const d=await r.json().catch(()=>({}));if(r.status===401)return expire();if(!r.ok||!d.ok)throw new Error(d.message||'读取历史失败');el.messages.textContent='';if(!d.available||!d.messages||!d.messages.length)return empty('这个线程暂无可显示的历史。');for(const x of d.messages)addMsg(x.role,x.text,x.role==='user'?'你':'Codex')}
async function selectThread(id){st.selected=id;localStorage.setItem('crw.thread',id);el.rail.classList.remove('open');renderThreads();await history(id);startPoll()}
async function poll(){if(!st.selected)return;try{const r=await api('/api/status?thread='+encodeURIComponent(st.selected));const d=await r.json();if(r.status===401)return expire();if(d.thread){const idx=st.threads.findIndex(x=>x.id===d.thread.id);if(idx>=0)st.threads[idx]=d.thread}dot(el.runDot,d.active?'ok':'');el.runText.textContent=d.active?'运行中':'空闲';setHeader()}catch{}}
function startPoll(){if(st.timer)clearInterval(st.timer);poll();st.timer=setInterval(poll,2500)}
async function send(){const text=el.input.value.trim();const attachments=st.attachments.slice();if(!text&&!attachments.length)return;el.send.disabled=true;addMsg('user',text||' ',attachments.length?'你 · '+attachments.length+' 个附件':'你',attachments);el.input.value='';st.attachments=[];renderTray();autosize();try{const payload={text:text,threadId:st.selected||'',attachments:attachments.map(x=>({name:x.name,type:x.type,dataUrl:x.dataUrl}))};const r=await api('/api/send',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify(payload)});const d=await r.json().catch(()=>({}));if(r.status===401)return expire();if(!r.ok||!d.ok)throw new Error(d.message||'发送失败');dot(el.runDot,'ok');el.runText.textContent=d.queued?'已排队':'已发送';startPoll()}catch(e){addMsg('assistant',e.message||'发送失败','系统')}finally{el.send.disabled=false;el.input.focus()}}
function expire(){st.token='';localStorage.removeItem('crw.token');showPair(true);dot(el.healthDot,'');el.healthText.textContent='待配对';empty('会话已过期，请重新配对。')}
async function boot(){await health();if(!st.token)return showPair(true);showPair(false);try{await loadThreads();startPoll()}catch(e){empty(e.message||'启动失败')}}
function autosize(){el.input.style.height='auto';el.input.style.height=Math.min(el.input.scrollHeight,148)+'px'}
function closeActions(){el.actionMenu.classList.remove('open')}
async function threadAction(action,name){const x=selected();if(!x)return;closeActions();const r=await api('/api/thread-action',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({threadId:x.id,action,name})});const d=await r.json().catch(()=>({}));if(r.status===401)return expire();if(!r.ok||!d.ok)throw new Error(d.message||'操作失败');await loadThreads();if(d.nextThreadId){st.selected=d.nextThreadId;localStorage.setItem('crw.thread',st.selected);await history(st.selected)}}
async function stopCurrent(){const x=selected();if(!x)return;closeActions();const r=await api('/api/stop',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({threadId:x.id})});const d=await r.json().catch(()=>({}));if(r.status===401)return expire();if(!r.ok||!d.ok)throw new Error(d.message||'停止失败');addMsg('assistant','已发送停止指令。','系统');setTimeout(()=>loadThreads().catch(()=>{}),900)}
async function newThread(){closeActions();const r=await api('/api/new-thread',{method:'POST'});const d=await r.json().catch(()=>({}));if(r.status===401)return expire();if(!r.ok||!d.ok)throw new Error(d.message||'新建线程失败');addMsg('assistant','已请求 Codex 新建线程。','系统')}
$('pair').onsubmit=e=>{e.preventDefault();el.notice.textContent='';fetch('/api/pair',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({code:el.code.value.trim(),deviceName:navigator.userAgent.slice(0,60)})}).then(r=>r.json().then(d=>{if(!r.ok||!d.ok)throw new Error(d.message||'配对失败');st.token=d.token;localStorage.setItem('crw.token',st.token);showPair(false);boot()})).catch(e=>el.notice.textContent=e.message||'配对失败')};
$('composer').onsubmit=e=>{e.preventDefault();send()};
el.input.addEventListener('input',autosize);
el.input.addEventListener('keydown',e=>{if(e.key==='Enter'&&!e.shiftKey&&!e.isComposing){e.preventDefault();send()}});
el.attach.onclick=()=>el.file.click();
el.file.onchange=async()=>{const files=Array.from(el.file.files||[]);el.file.value='';for(const file of files){if(st.attachments.length>=6){addMsg('assistant','一次最多发送 6 个附件。','系统');break}if(file.size>12*1024*1024){addMsg('assistant','单个附件不能超过 12MB。','系统');continue}try{const dataUrl=await fileToDataURL(file);st.attachments.push({name:file.name||'attachment',type:file.type||'application/octet-stream',dataUrl:dataUrl})}catch(e){addMsg('assistant',e.message||'读取附件失败','系统')}}renderTray()};
el.threadButton.onclick=()=>el.rail.classList.toggle('open');
$('closeRail').onclick=()=>el.rail.classList.remove('open');
$('refresh').onclick=()=>loadThreads().catch(e=>empty(e.message||'刷新失败'));
el.newThread.onclick=()=>newThread().catch(e=>addMsg('assistant',e.message||'新建线程失败','系统'));
el.more.onclick=e=>{e.stopPropagation();el.actionMenu.classList.toggle('open')};
el.pinAction.onclick=()=>{const x=selected();threadAction(x&&x.pinned?'unpin':'pin').catch(e=>addMsg('assistant',e.message||'置顶失败','系统'))};
el.renameAction.onclick=()=>{const x=selected();if(!x)return;const name=prompt('线程名称',x.title||'');if(name!==null)threadAction('rename',name).catch(e=>addMsg('assistant',e.message||'重命名失败','系统'))};
el.archiveAction.onclick=()=>{const x=selected();if(x&&confirm('归档这个线程？'))threadAction('archive').catch(e=>addMsg('assistant',e.message||'归档失败','系统'))};
el.stopAction.onclick=()=>stopCurrent().catch(e=>addMsg('assistant',e.message||'停止失败','系统'));
document.addEventListener('click',e=>{if(!el.actionMenu.contains(e.target)&&!el.more.contains(e.target))closeActions()});
window.addEventListener('focus',()=>{health();if(st.token)loadThreads().catch(()=>{})});
boot();
</script>
</body>
</html>`

const indexHTMLProjects = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="theme-color" content="#111315">
<link rel="manifest" href="/manifest.webmanifest">
<link rel="icon" type="image/png" sizes="192x192" href="/icon-blue-192.png">
<link rel="apple-touch-icon" href="/icon-blue-192.png">
<title>Codex Remote Win</title>
<style>
:root{--bg:#111315;--sidebar:#171a1d;--surface:#1d2125;--raised:#252a2f;--line:#30363b;--text:#f1f3f4;--muted:#9aa4ac;--faint:#6f7a82;--green:#82d8ad;--green-bg:#1c3b31;--amber:#efbd72;--red:#ee8c8c;--blue:#8eb9ef;font-family:"Segoe UI","Microsoft YaHei",Arial,sans-serif;color:var(--text);background:var(--bg)}
*{box-sizing:border-box}html,body{height:100%;margin:0;overflow:hidden;background:var(--bg)}button,input,textarea{font:inherit}button{border:0;color:inherit;cursor:pointer}button:disabled{cursor:default;opacity:.5}.app{height:100%;display:grid;grid-template-columns:330px minmax(0,1fr);grid-template-rows:56px minmax(0,1fr);background:var(--bg)}
.topbar{grid-column:2;grid-row:1;display:flex;align-items:center;gap:9px;padding:0 14px;border-bottom:1px solid var(--line);background:#141719;min-width:0}.sideToggle{display:none}.iconBtn{width:34px;height:34px;border-radius:6px;background:var(--surface);display:grid;place-items:center;color:var(--muted);font-size:18px}.iconBtn:hover{background:var(--raised);color:var(--text)}.headText{min-width:0}.threadTitle{font-size:14px;font-weight:700;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.threadPath{margin-top:2px;color:var(--faint);font-size:11px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.headActions{display:flex;align-items:center;gap:7px;margin-left:auto}.badge{height:28px;padding:0 9px;border:1px solid var(--line);border-radius:6px;display:flex;align-items:center;gap:6px;color:var(--muted);font-size:11px;white-space:nowrap}.dot{width:7px;height:7px;border-radius:50%;background:var(--faint)}.dot.ok{background:var(--green)}.dot.bad{background:var(--red)}.context.warn{color:var(--amber)}.context.danger{color:var(--red)}
.sidebar{grid-column:1;grid-row:1/3;display:grid;grid-template-rows:auto auto auto minmax(0,1fr);background:var(--sidebar);border-right:1px solid var(--line);min-height:0;z-index:20}.brand{height:56px;padding:0 15px;display:flex;align-items:center;border-bottom:1px solid var(--line)}.brandMark{width:25px;height:25px;border-radius:5px;background:var(--green);color:#10231b;display:grid;place-items:center;font-weight:900;margin-right:9px}.brandText{font-size:14px;font-weight:750}.version{margin-left:auto;color:var(--faint);font-size:10px}.searchWrap{padding:10px 10px 7px}.search{width:100%;height:36px;border:1px solid var(--line);border-radius:6px;background:#121517;color:var(--text);padding:0 11px;outline:0;font-size:13px}.search:focus{border-color:#4b6559}.tabs{display:grid;grid-template-columns:1fr 1fr;gap:4px;padding:0 10px 8px}.tab{height:32px;border-radius:5px;background:transparent;color:var(--muted);font-size:12px}.tab.active{background:var(--raised);color:var(--text)}.projectList{overflow:auto;padding:0 8px 14px;overscroll-behavior:contain}.group{margin-top:7px}.groupHead{width:100%;height:34px;padding:0 6px;display:flex;align-items:center;gap:7px;background:transparent;color:var(--muted);text-align:left}.groupHead:hover{color:var(--text)}.chev{width:13px;font-size:11px}.projectGlyph{width:22px;height:22px;border-radius:4px;background:#293027;display:grid;place-items:center;color:var(--green);font-size:11px;font-weight:800}.groupName{min-width:0;flex:1;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;font-size:12px;font-weight:700}.groupCount{color:var(--faint);font-size:10px}.groupBody.collapsed{display:none}.thread{width:100%;min-height:59px;margin:2px 0;padding:8px 8px 8px 38px;border-radius:6px;background:transparent;display:block;text-align:left;position:relative}.thread:hover{background:#1e2326}.thread.active{background:var(--raised)}.threadRun{position:absolute;left:17px;top:16px;width:7px;height:7px;border-radius:50%;background:#59636a}.threadRun.live{background:var(--green);box-shadow:0 0 7px rgba(130,216,173,.45)}.threadName{font-size:13px;font-weight:650;line-height:1.25;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.threadMeta{margin-top:5px;color:var(--faint);font-size:10px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.restoreBtn{position:absolute;right:7px;top:19px;width:26px;height:26px;border-radius:5px;background:var(--surface);color:var(--green)}.emptySide{padding:30px 16px;text-align:center;color:var(--faint);font-size:12px;line-height:1.6}
.main{grid-column:2;grid-row:2;min-width:0;min-height:0;display:grid;grid-template-rows:minmax(0,1fr) auto}.messages{overflow:auto;padding:22px max(18px,calc((100% - 880px)/2)) 12px;overscroll-behavior:contain}.loadMore{display:block;margin:0 auto 22px;height:30px;padding:0 12px;border-radius:5px;background:var(--surface);color:var(--muted);font-size:12px}.message{display:flex;margin-bottom:20px}.message.user{justify-content:flex-end}.messageWrap{max-width:min(800px,92%)}.message.assistant .messageWrap{width:100%}.messageMeta{margin-bottom:6px;color:var(--faint);font-size:11px}.message.user .messageMeta{text-align:right}.bubble{font-size:14px;line-height:1.68;overflow-wrap:anywhere}.message.user .bubble{padding:10px 13px;border-radius:7px;background:var(--green-bg);white-space:pre-wrap}.md p{margin:0 0 12px}.md p:last-child{margin-bottom:0}.md h1,.md h2,.md h3{margin:18px 0 9px;font-size:16px;line-height:1.4}.md h1{font-size:19px}.md ul,.md ol{margin:7px 0 12px;padding-left:23px}.md li{margin:4px 0}.md blockquote{margin:10px 0;padding:4px 11px;border-left:3px solid #526159;color:var(--muted)}.md code{font-family:Consolas,"SFMono-Regular",monospace;background:#22272b;border-radius:4px;padding:2px 5px;font-size:12px}.codeBlock{position:relative;margin:11px 0;border:1px solid var(--line);border-radius:6px;background:#0c0e0f;overflow:hidden}.codeBlock pre{margin:0;padding:14px;overflow:auto}.codeBlock code{background:transparent;padding:0;white-space:pre;font-size:12px}.copyCode{position:absolute;right:7px;top:7px;height:26px;padding:0 8px;border-radius:4px;background:#2a3034;color:var(--muted);font-size:10px}.md a{color:var(--blue);text-decoration:none}.md a:hover{text-decoration:underline}.emptyChat{height:100%;min-height:260px;display:grid;place-items:center;text-align:center;color:var(--muted);font-size:13px;padding:24px}
.activity{display:none;margin:0 max(18px,calc((100% - 880px)/2)) 8px;border-left:2px solid var(--green);background:#161a1c;padding:9px 12px}.activity.show{display:block}.activityHead{display:flex;align-items:center;gap:7px;color:var(--green);font-size:11px;font-weight:700}.activitySteps{margin-top:7px;display:grid;gap:5px}.step{display:grid;grid-template-columns:9px minmax(0,1fr);gap:7px;color:var(--muted);font-size:11px;line-height:1.35}.stepMark{margin-top:4px;width:5px;height:5px;border-radius:50%;background:var(--faint)}.step.error{color:var(--red)}.step.complete{color:var(--green)}.stepDetail{color:var(--faint);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.queue{display:none;margin:0 max(18px,calc((100% - 880px)/2)) 7px}.queue.show{display:grid;gap:5px}.queueItem{min-height:34px;border:1px solid var(--line);border-radius:5px;background:var(--surface);display:flex;align-items:center;gap:8px;padding:5px 8px;color:var(--muted);font-size:11px}.queueText{min-width:0;flex:1;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.retry{height:24px;padding:0 8px;border-radius:4px;background:var(--raised);color:var(--amber)}
.composerShell{padding:5px max(14px,calc((100% - 900px)/2)) max(12px,env(safe-area-inset-bottom));background:linear-gradient(180deg,rgba(17,19,21,0),var(--bg) 18%)}.attachTray{display:none;gap:7px;overflow:auto;padding:0 2px 7px}.attachTray.show{display:flex}.fileChip{position:relative;flex:0 0 68px;height:62px;border:1px solid var(--line);border-radius:6px;background:var(--surface);overflow:hidden;padding:7px}.fileChip img{width:100%;height:100%;object-fit:cover}.fileName{font-size:10px;color:var(--muted);line-height:1.2;overflow:hidden}.removeFile{position:absolute;right:3px;top:3px;width:20px;height:20px;border-radius:50%;background:rgba(0,0,0,.75);color:#fff}.composer{display:grid;grid-template-columns:36px minmax(0,1fr) 38px;align-items:end;gap:7px;padding:7px;border:1px solid var(--line);border-radius:7px;background:#181c1f}.composer:focus-within{border-color:#4b6559}.attach,.send{width:36px;height:36px;border-radius:5px;display:grid;place-items:center;font-size:19px}.attach{background:var(--raised);color:var(--muted)}.send{background:var(--green);color:#10231b;font-weight:900}.fileInput{display:none}textarea{width:100%;min-height:36px;max-height:150px;resize:none;border:0;outline:0;background:transparent;color:var(--text);padding:7px 1px;font-size:15px;line-height:1.45}textarea::placeholder{color:var(--faint)}
.menu{position:fixed;right:12px;top:51px;z-index:50;display:none;width:190px;padding:5px;border:1px solid var(--line);border-radius:6px;background:#171b1e;box-shadow:0 15px 42px rgba(0,0,0,.45)}.menu.open{display:grid;gap:2px}.menu button{height:36px;border-radius:4px;background:transparent;text-align:left;padding:0 9px;font-size:12px}.menu button:hover{background:var(--raised)}.menu .danger{color:var(--red)}
.overlay{position:fixed;inset:0;z-index:100;background:rgba(0,0,0,.72);display:grid;place-items:center;padding:20px;backdrop-filter:blur(8px)}.hidden{display:none!important}.pairBox{width:min(390px,100%);padding:19px;border:1px solid var(--line);border-radius:7px;background:#171b1e}.pairBox h2{margin:0 0 8px;font-size:19px}.pairBox p{margin:0 0 15px;color:var(--muted);font-size:13px;line-height:1.6}.pairRow{display:grid;grid-template-columns:1fr 82px;gap:7px}.pairCode{height:44px;border:1px solid var(--line);border-radius:6px;background:#111416;color:var(--text);padding:0 11px;font-size:19px;outline:0}.primary{border-radius:6px;background:var(--green);color:#10231b;font-weight:800}.notice{min-height:18px;margin-top:9px;color:var(--red);font-size:12px}.scrim{display:none}
@media(max-width:820px){.app{grid-template-columns:1fr;grid-template-rows:54px minmax(0,1fr)}.topbar{grid-column:1;grid-row:1;padding:0 10px}.sideToggle{display:grid}.sidebar{position:fixed;left:0;top:0;bottom:0;width:min(86vw,340px);transform:translateX(-102%);transition:transform .18s ease;box-shadow:12px 0 36px rgba(0,0,0,.45)}.sidebar.open{transform:translateX(0)}.scrim.show{display:block;position:fixed;inset:0;z-index:19;background:rgba(0,0,0,.55)}.main{grid-column:1;grid-row:2}.messages{padding:16px 12px 8px}.activity,.queue{margin-left:12px;margin-right:12px}.composerShell{padding-left:9px;padding-right:9px}.headActions .modelBadge{display:none}.threadPath{max-width:43vw}.messageWrap{max-width:92%}.message.assistant .messageWrap{max-width:100%}.brand{padding-top:env(safe-area-inset-top);height:calc(56px + env(safe-area-inset-top))}}
@media(max-width:480px){.headActions .healthBadge{display:none}.badge{padding:0 7px}.threadTitle{max-width:42vw}}
</style>
</head>
<body>
<div class="app">
  <header class="topbar">
    <button class="iconBtn sideToggle" id="sideToggle" type="button" title="任务列表">☰</button>
    <div class="headText"><div class="threadTitle" id="threadTitle">选择任务</div><div class="threadPath" id="threadPath">按项目整理的 Codex 任务</div></div>
    <div class="headActions">
      <span class="badge modelBadge" id="modelBadge">Codex</span>
      <span class="badge context" id="contextBadge">--</span>
      <span class="badge"><span class="dot" id="runDot"></span><span id="runText">空闲</span></span>
      <span class="badge healthBadge"><span class="dot" id="healthDot"></span><span id="healthText">连接</span></span>
      <button class="iconBtn" id="newThread" type="button" title="新建任务">＋</button>
      <button class="iconBtn" id="more" type="button" title="任务操作">⋯</button>
    </div>
  </header>
  <aside class="sidebar" id="sidebar">
    <div class="brand"><span class="brandMark">C</span><span class="brandText">Codex Remote</span><span class="version">v0.10</span></div>
    <div class="searchWrap"><input class="search" id="search" type="search" placeholder="搜索任务或项目"></div>
    <div class="tabs"><button class="tab active" id="activeTab" type="button">任务</button><button class="tab" id="archiveTab" type="button">归档</button></div>
    <div class="projectList" id="projectList"></div>
  </aside>
  <div class="scrim" id="scrim"></div>
  <main class="main">
    <section class="messages" id="messages"><div class="emptyChat">选择一个任务开始使用</div></section>
    <div>
      <section class="activity" id="activity"><div class="activityHead"><span class="dot ok"></span><span id="activityTitle">Codex 正在处理</span></div><div class="activitySteps" id="activitySteps"></div></section>
      <section class="queue" id="queue"></section>
      <form class="composerShell" id="composer">
        <div class="attachTray" id="attachTray"></div>
        <div class="composer">
          <button class="attach" id="attach" type="button" title="添加附件">＋</button>
          <input class="fileInput" id="fileInput" type="file" multiple>
          <textarea id="input" maxlength="8000" rows="1" placeholder="发给 Codex"></textarea>
          <button class="send" id="send" type="submit" title="发送">↑</button>
        </div>
      </form>
    </div>
  </main>
</div>
<div class="menu" id="menu"><button id="pinAction" type="button">置顶任务</button><button id="renameAction" type="button">重命名</button><button id="stopAction" type="button">停止回复</button><button class="danger" id="archiveAction" type="button">归档任务</button></div>
<div class="overlay" id="overlay"><form class="pairBox" id="pair"><h2>配对这台设备</h2><p>输入 Windows 托盘菜单中显示的配对码。成功后，本浏览器会保存可信令牌。</p><div class="pairRow"><input class="pairCode" id="code" inputmode="numeric" maxlength="12" placeholder="000000"><button class="primary" type="submit">配对</button></div><div class="notice" id="notice"></div></form></div>
<script>
const $=id=>document.getElementById(id);
const st={token:localStorage.getItem('crw.token')||'',threads:[],projects:[],selected:localStorage.getItem('crw.thread')||'',view:'active',query:'',collapsed:new Set(JSON.parse(localStorage.getItem('crw.collapsed')||'[]')),attachments:[],queue:[],timer:null,lastActive:false,historyBefore:0};
const el={sidebar:$('sidebar'),scrim:$('scrim'),list:$('projectList'),search:$('search'),activeTab:$('activeTab'),archiveTab:$('archiveTab'),title:$('threadTitle'),path:$('threadPath'),messages:$('messages'),input:$('input'),send:$('send'),file:$('fileInput'),tray:$('attachTray'),overlay:$('overlay'),code:$('code'),notice:$('notice'),runDot:$('runDot'),runText:$('runText'),healthDot:$('healthDot'),healthText:$('healthText'),context:$('contextBadge'),model:$('modelBadge'),activity:$('activity'),steps:$('activitySteps'),activityTitle:$('activityTitle'),queue:$('queue'),menu:$('menu'),pin:$('pinAction'),archive:$('archiveAction')};
function api(path,options={}){const headers=options.headers||{};if(st.token)headers.authorization='Bearer '+st.token;return fetch(path,Object.assign({},options,{headers}))}
function esc(value){return String(value==null?'':value).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
function selected(){return st.threads.find(x=>x.id===st.selected)}
function when(value){const t=Date.parse(value||'');return Number.isFinite(t)?new Date(t).toLocaleString([],{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'}):''}
function contextText(x){return x&&x.context&&x.context.available?Math.round(x.context.percent)+'%':'--'}
function setDot(node,kind){node.className='dot '+(kind||'')}
function showPair(show){el.overlay.classList.toggle('hidden',!show);if(show)setTimeout(()=>el.code.focus(),60)}
function openSide(){el.sidebar.classList.add('open');el.scrim.classList.add('show')}
function closeSide(){el.sidebar.classList.remove('open');el.scrim.classList.remove('show')}
function syncHeader(){const x=selected();el.title.textContent=x?(x.title||'(untitled)'):'选择任务';el.path.textContent=x?(x.cwd||x.projectName||'未归类'):'按项目整理的 Codex 任务';setDot(el.runDot,x&&x.active?'ok':'');el.runText.textContent=x&&x.active?'运行中':'空闲';el.context.textContent=contextText(x);const pct=x&&x.context?x.context.percent:0;el.context.className='badge context '+(pct>=90?'danger':pct>=75?'warn':'');el.pin.textContent=x&&x.pinned?'取消置顶':'置顶任务';el.archive.textContent=x&&x.archived?'恢复任务':'归档任务';el.input.disabled=!!(x&&x.archived);el.send.disabled=!!(x&&x.archived);el.input.placeholder=x&&x.archived?'恢复任务后才能发送':'发给 Codex'}
function filteredThreads(){const q=st.query.trim().toLowerCase();return st.threads.filter(x=>(st.view==='archive'?x.archived:!x.archived)&&(!q||(x.title+' '+x.cwd+' '+x.projectName).toLowerCase().includes(q)))}
function makeThread(x){const b=document.createElement('button');b.type='button';b.className='thread '+(x.id===st.selected?'active':'');b.onclick=()=>selectThread(x.id);const run=document.createElement('span');run.className='threadRun '+(x.active?'live':'');const name=document.createElement('div');name.className='threadName';name.textContent=x.title||'(untitled)';const meta=document.createElement('div');meta.className='threadMeta';meta.textContent=(x.active?'运行中':'空闲')+' · '+contextText(x)+' · '+when(x.updatedAt);b.append(run,name,meta);if(x.archived){const restore=document.createElement('button');restore.type='button';restore.className='restoreBtn';restore.title='恢复任务';restore.textContent='↩';restore.onclick=e=>{e.stopPropagation();threadActionFor(x,'restore')};b.appendChild(restore)}return b}
function makeGroup(key,name,rows,kind){const box=document.createElement('section');box.className='group';const head=document.createElement('button');head.type='button';head.className='groupHead';const collapsed=st.collapsed.has(key);head.innerHTML='<span class="chev">'+(collapsed?'›':'⌄')+'</span><span class="projectGlyph">'+(kind==='pinned'?'P':esc((name||'?').slice(0,1).toUpperCase()))+'</span><span class="groupName">'+esc(name)+'</span><span class="groupCount">'+rows.length+'</span>';head.onclick=()=>{if(st.collapsed.has(key))st.collapsed.delete(key);else st.collapsed.add(key);localStorage.setItem('crw.collapsed',JSON.stringify(Array.from(st.collapsed)));renderProjects()};const body=document.createElement('div');body.className='groupBody '+(collapsed?'collapsed':'');rows.forEach(x=>body.appendChild(makeThread(x)));box.append(head,body);return box}
function renderProjects(){el.list.textContent='';el.activeTab.classList.toggle('active',st.view==='active');el.archiveTab.classList.toggle('active',st.view==='archive');const rows=filteredThreads();if(!rows.length){const d=document.createElement('div');d.className='emptySide';d.textContent=st.view==='archive'?'没有归档任务':'没有匹配的任务';el.list.appendChild(d);syncHeader();return}const pinned=rows.filter(x=>x.pinned&&!x.archived);if(pinned.length)el.list.appendChild(makeGroup('__pinned__','置顶任务',pinned,'pinned'));const normal=rows.filter(x=>!x.pinned||x.archived);const groups=new Map();normal.forEach(x=>{const key=x.projectKey||'__unassigned__';if(!groups.has(key))groups.set(key,{name:x.projectName||'未归类',rows:[]});groups.get(key).rows.push(x)});groups.forEach((g,key)=>el.list.appendChild(makeGroup(key,g.name,g.rows,'project')));syncHeader()}
function emptyChat(text){el.messages.textContent='';const d=document.createElement('div');d.className='emptyChat';d.textContent=text;el.messages.appendChild(d)}
function inlineMD(text){let s=esc(text);s=s.replace(/\x60([^\x60]+)\x60/g,'<code>$1</code>');s=s.replace(/\*\*([^*]+)\*\*/g,'<strong>$1</strong>');s=s.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g,'<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>');return s}
function markdown(source){const lines=String(source||'').replace(/\r/g,'').split('\n');let out='',paragraph=[],list='',inCode=false,code=[],lang='';const fence='\x60\x60\x60';const flush=()=>{if(paragraph.length){out+='<p>'+paragraph.map(inlineMD).join('<br>')+'</p>';paragraph=[]}if(list){out+='</'+list+'>';list=''}};for(const line of lines){if(line.startsWith(fence)){if(inCode){out+='<div class="codeBlock"><button class="copyCode" type="button">复制</button><pre><code>'+esc(code.join('\n'))+'</code></pre></div>';inCode=false;code=[]}else{flush();inCode=true;lang=line.slice(3).trim()}continue}if(inCode){code.push(line);continue}const h=line.match(/^(#{1,3})\s+(.+)$/);if(h){flush();const n=h[1].length;out+='<h'+n+'>'+inlineMD(h[2])+'</h'+n+'>';continue}const ul=line.match(/^\s*[-*]\s+(.+)$/);const ol=line.match(/^\s*\d+[.]\s+(.+)$/);if(ul||ol){flush();const wanted=ul?'ul':'ol';if(list!==wanted){if(list)out+='</'+list+'>';out+='<'+wanted+'>';list=wanted}out+='<li>'+inlineMD((ul||ol)[1])+'</li>';continue}if(list){out+='</'+list+'>';list=''}if(line.startsWith('> ')){flush();out+='<blockquote>'+inlineMD(line.slice(2))+'</blockquote>';continue}if(!line.trim()){flush()}else paragraph.push(line)}if(inCode)out+='<div class="codeBlock"><pre><code>'+esc(code.join('\n'))+'</code></pre></div>';flush();return out}
function bindCopy(root){root.querySelectorAll('.copyCode').forEach(btn=>btn.onclick=()=>{const text=btn.parentElement.querySelector('code').textContent;navigator.clipboard&&navigator.clipboard.writeText(text).then(()=>{btn.textContent='已复制';setTimeout(()=>btn.textContent='复制',1200)})})}
function addMessage(role,text,label,attachments,prepend,target){const article=document.createElement('article');article.className='message '+role;const wrap=document.createElement('div');wrap.className='messageWrap';const meta=document.createElement('div');meta.className='messageMeta';meta.textContent=label||(role==='user'?'你':'Codex');const bubble=document.createElement('div');bubble.className='bubble '+(role==='assistant'?'md':'');if(role==='assistant'){bubble.innerHTML=markdown(text);bindCopy(bubble)}else bubble.textContent=text||'';wrap.append(meta,bubble);article.appendChild(wrap);const parent=target||el.messages;if(prepend)parent.insertBefore(article,parent.firstChild);else parent.appendChild(article);return article}
async function loadHistory(id,before,prepend){if(!prepend)emptyChat('加载中...');let path='/api/history?thread='+encodeURIComponent(id)+'&limit=80';if(before)path+='&before='+before;const r=await api(path);const d=await r.json().catch(()=>({}));if(r.status===401)return expire();if(!r.ok||!d.ok)throw new Error(d.message||'读取历史失败');if(!prepend)el.messages.textContent='';const anchor=prepend?el.messages.scrollHeight:0;const messages=d.messages||[];if(!messages.length&&!prepend)return emptyChat('这个任务暂无消息');const frag=document.createDocumentFragment();messages.forEach(x=>addMessage(x.role,x.text,x.role==='user'?'你':'Codex',null,false,frag));if(prepend)el.messages.insertBefore(frag,el.messages.firstChild);else el.messages.appendChild(frag);if(d.hasMore){const more=document.createElement('button');more.type='button';more.className='loadMore';more.textContent='加载更早消息';more.onclick=()=>{more.remove();loadHistory(id,d.nextBefore,true)};el.messages.insertBefore(more,el.messages.firstChild)}st.historyBefore=d.nextBefore||0;if(prepend)el.messages.scrollTop=el.messages.scrollHeight-anchor;else el.messages.scrollTop=el.messages.scrollHeight}
async function loadThreads(keepHistory){const r=await api('/api/threads?includeArchived=1');const d=await r.json().catch(()=>({}));if(r.status===401)return expire();if(!r.ok||!d.ok)throw new Error(d.message||'读取任务失败');st.threads=d.threads||[];st.projects=d.projects||[];if(!st.selected||!st.threads.some(x=>x.id===st.selected)){const first=st.threads.find(x=>!x.archived)||st.threads[0];st.selected=first?first.id:''}if(st.selected)localStorage.setItem('crw.thread',st.selected);renderProjects();if(!keepHistory){if(st.selected)await loadHistory(st.selected,0,false);else emptyChat('还没有找到 Codex 任务')}}
async function selectThread(id){st.selected=id;localStorage.setItem('crw.thread',id);renderProjects();closeSide();await loadHistory(id,0,false);startPoll()}
function renderActivity(steps,active){el.steps.textContent='';const show=(steps&&steps.length)&&(active||steps[steps.length-1].kind==='error');el.activity.classList.toggle('show',!!show);if(!show)return;el.activityTitle.textContent=active?'Codex 正在处理':'最近一次执行';steps.slice(-7).forEach(x=>{const row=document.createElement('div');row.className='step '+x.kind;const mark=document.createElement('span');mark.className='stepMark';const body=document.createElement('div');body.textContent=x.label;if(x.detail){const detail=document.createElement('div');detail.className='stepDetail';detail.textContent=x.detail;body.appendChild(detail)}row.append(mark,body);el.steps.appendChild(row)})}
async function poll(){if(!st.selected)return;try{const r=await api('/api/status?thread='+encodeURIComponent(st.selected));const d=await r.json();if(r.status===401)return expire();if(!r.ok)return;if(d.thread){const i=st.threads.findIndex(x=>x.id===d.thread.id);if(i>=0)st.threads[i]=d.thread}if(d.model)el.model.textContent=d.model+(d.reasoning?' · '+d.reasoning:'');renderActivity(d.steps||[],!!d.active);const finished=st.lastActive&&!d.active;st.lastActive=!!d.active;syncHeader();if(finished){await loadHistory(st.selected,0,false);await loadThreads(true)}else renderProjects()}catch{}}
function startPoll(){if(st.timer)clearInterval(st.timer);poll();st.timer=setInterval(poll,1600)}
function requestID(){return crypto.randomUUID?crypto.randomUUID():Date.now().toString(36)+Math.random().toString(36).slice(2)}
function renderQueue(){el.queue.textContent='';el.queue.classList.toggle('show',st.queue.length>0);st.queue.forEach(item=>{const row=document.createElement('div');row.className='queueItem';const status=document.createElement('span');status.textContent=item.status==='sending'?'发送中':item.status==='failed'?'发送失败':item.status==='queued'?'已排队':'已发送';const text=document.createElement('span');text.className='queueText';text.textContent=item.text||((item.attachments||[]).length+' 个附件');row.append(status,text);if(item.status==='failed'){const retry=document.createElement('button');retry.type='button';retry.className='retry';retry.textContent='重试';retry.onclick=()=>dispatch(item);row.appendChild(retry)}el.queue.appendChild(row)})}
async function dispatch(item){item.status='sending';renderQueue();try{const payload={text:item.text,threadId:item.threadId,clientRequestId:item.id,attachments:item.attachments.map(x=>({name:x.name,type:x.type,dataUrl:x.dataUrl}))};const r=await api('/api/send',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify(payload)});const d=await r.json().catch(()=>({}));if(r.status===401)return expire();if(!r.ok||!d.ok)throw new Error(d.message||'发送失败');item.status=d.queued?'queued':'sent';renderQueue();setTimeout(()=>{st.queue=st.queue.filter(x=>x!==item);renderQueue()},2600);startPoll()}catch(error){item.status='failed';item.error=error.message||'发送失败';renderQueue()}}
function submitMessage(){const text=el.input.value.trim();const attachments=st.attachments.slice();if(!text&&!attachments.length)return;const item={id:requestID(),threadId:st.selected||'',text,attachments,status:'sending'};st.queue.push(item);addMessage('user',text||' ',attachments.length?'你 · '+attachments.length+' 个附件':'你');el.messages.scrollTop=el.messages.scrollHeight;el.input.value='';st.attachments=[];renderAttachments();autosize();dispatch(item)}
function isImage(x){return String(x.type||'').startsWith('image/')}
function renderAttachments(){el.tray.textContent='';el.tray.classList.toggle('show',st.attachments.length>0);st.attachments.forEach((x,i)=>{const chip=document.createElement('div');chip.className='fileChip';if(isImage(x)){const img=document.createElement('img');img.src=x.dataUrl;chip.appendChild(img)}else{const name=document.createElement('div');name.className='fileName';name.textContent=x.name;chip.appendChild(name)}const remove=document.createElement('button');remove.type='button';remove.className='removeFile';remove.textContent='×';remove.onclick=()=>{st.attachments.splice(i,1);renderAttachments()};chip.appendChild(remove);el.tray.appendChild(chip)})}
function fileData(file){return new Promise((resolve,reject)=>{const reader=new FileReader();reader.onload=()=>resolve(reader.result);reader.onerror=()=>reject(new Error('读取附件失败'));reader.readAsDataURL(file)})}
async function health(){try{const r=await api('/api/health');const d=await r.json();setDot(el.healthDot,'ok');el.healthText.textContent=st.token?(d.cdp&&d.cdp.available?'CDP':'已连接'):'待配对'}catch{setDot(el.healthDot,'bad');el.healthText.textContent='离线'}}
function expire(){st.token='';localStorage.removeItem('crw.token');showPair(true);emptyChat('访问令牌已过期，请重新配对')}
async function threadActionFor(x,action,name){if(!x)return;const r=await api('/api/thread-action',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({threadId:x.id,action,name})});const d=await r.json().catch(()=>({}));if(r.status===401)return expire();if(!r.ok||!d.ok)throw new Error(d.message||'操作失败');if(action==='archive'){st.view='active';st.selected=d.nextThreadId||''}if(action==='restore'){st.view='active';st.selected=x.id}await loadThreads(false)}
function currentAction(action,name){return threadActionFor(selected(),action,name)}
async function stopCurrent(){const x=selected();if(!x)return;const r=await api('/api/stop',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({threadId:x.id})});const d=await r.json().catch(()=>({}));if(!r.ok||!d.ok)throw new Error(d.message||'停止失败')}
async function newThread(){const r=await api('/api/new-thread',{method:'POST'});const d=await r.json().catch(()=>({}));if(!r.ok||!d.ok)throw new Error(d.message||'新建任务失败');setTimeout(()=>loadThreads(false),1200)}
function closeMenu(){el.menu.classList.remove('open')}
function autosize(){el.input.style.height='auto';el.input.style.height=Math.min(el.input.scrollHeight,150)+'px'}
async function boot(){await health();if(!st.token)return showPair(true);showPair(false);try{await loadThreads(false);startPoll()}catch(error){emptyChat(error.message||'启动失败')}}
$('pair').onsubmit=async e=>{e.preventDefault();el.notice.textContent='';try{const r=await fetch('/api/pair',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({code:el.code.value.trim(),deviceName:navigator.userAgent.slice(0,70)})});const d=await r.json();if(!r.ok||!d.ok)throw new Error(d.message||'配对失败');st.token=d.token;localStorage.setItem('crw.token',st.token);showPair(false);boot()}catch(error){el.notice.textContent=error.message||'配对失败'}};
$('composer').onsubmit=e=>{e.preventDefault();submitMessage()};el.input.oninput=autosize;el.input.onkeydown=e=>{if(e.key==='Enter'&&!e.shiftKey&&!e.isComposing){e.preventDefault();submitMessage()}};
$('attach').onclick=()=>el.file.click();el.file.onchange=async()=>{const files=Array.from(el.file.files||[]);el.file.value='';for(const file of files){if(st.attachments.length>=6)break;if(file.size>12*1024*1024)continue;try{st.attachments.push({name:file.name||'attachment',type:file.type||'application/octet-stream',dataUrl:await fileData(file)})}catch{}}renderAttachments()};
$('sideToggle').onclick=openSide;el.scrim.onclick=closeSide;el.search.oninput=()=>{st.query=el.search.value;renderProjects()};el.activeTab.onclick=()=>{st.view='active';renderProjects()};el.archiveTab.onclick=()=>{st.view='archive';renderProjects()};
$('newThread').onclick=()=>newThread();$('more').onclick=e=>{e.stopPropagation();el.menu.classList.toggle('open')};el.pin.onclick=()=>{const x=selected();currentAction(x&&x.pinned?'unpin':'pin')};$('renameAction').onclick=()=>{const x=selected();if(!x)return;const name=prompt('任务名称',x.title||'');if(name!==null)currentAction('rename',name)};$('archiveAction').onclick=()=>{const x=selected();if(!x)return;currentAction(x.archived?'restore':'archive')};$('stopAction').onclick=()=>stopCurrent();document.addEventListener('click',e=>{if(!el.menu.contains(e.target)&&e.target!==$('more'))closeMenu()});window.addEventListener('focus',()=>{health();if(st.token)loadThreads(true)});boot();
</script>
</body>
</html>`

const manifestJSON = `{
  "name": "Codex Remote Win",
  "short_name": "Codex Remote",
  "description": "在手机上远程使用 Windows Codex Desktop",
  "scope": "./",
  "start_url": "./",
  "display": "standalone",
  "background_color": "#101214",
  "theme_color": "#101214",
  "icons": [
    {"src":"/icon-blue-192.png","sizes":"192x192","type":"image/png","purpose":"any maskable"},
    {"src":"/icon-blue-512.png","sizes":"512x512","type":"image/png","purpose":"any maskable"}
  ]
}`

const indexHTMLRefined = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, minimum-scale=1, maximum-scale=1, user-scalable=no, viewport-fit=cover">
<meta name="theme-color" content="#000000">
<title>Codex Remote Win</title>
<style>
:root{
  --bg:#0d0d0d;
  --top:#000;
  --panel:#171717;
  --panel2:#242424;
  --text:#f4f4f5;
  --muted:#a1a1aa;
  --faint:#71717a;
  --line:rgba(255,255,255,.09);
  --user:#2f2f2f;
  --ok:#8ef0b7;
  --blue:#5eb4ff;
  --danger:#ff8a8a;
  --composer-bottom:max(18px,env(safe-area-inset-bottom));
  font-family:ui-sans-serif,-apple-system,BlinkMacSystemFont,"SF Pro Text","Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif;
  color:var(--text);
  background:var(--top);
}
*{box-sizing:border-box}
html,body{width:100%;height:100%;margin:0;background:var(--top);overflow:hidden;overscroll-behavior:none}
body{position:fixed;inset:0;color:var(--text);-webkit-font-smoothing:antialiased;text-rendering:geometricPrecision}
button,input,textarea{font:inherit}
button{appearance:none;border:0;color:inherit;background:none}
.app{position:fixed;inset:0;display:grid;grid-template-rows:auto 1fr auto;background:var(--bg)}
.topbar{height:calc(52px + env(safe-area-inset-top));padding:env(safe-area-inset-top) 14px 0;background:var(--top);display:flex;align-items:center;gap:10px;z-index:5}
.threadButton{height:30px;min-width:0;max-width:min(58vw,420px);padding:0 11px;border-radius:999px;background:var(--panel);display:flex;align-items:center;gap:7px;color:var(--text);font-weight:760;font-size:14px;letter-spacing:0}
.titleDot{width:8px;height:8px;border-radius:50%;background:var(--ok);box-shadow:0 0 8px 2px rgba(142,240,183,.38);flex:0 0 auto}
.threadTitle{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.topActions{margin-left:auto;display:flex;align-items:center;gap:8px}
.badge{height:28px;min-width:28px;padding:0 9px;border-radius:999px;background:var(--panel);color:var(--muted);display:inline-flex;align-items:center;justify-content:center;gap:6px;font-size:12px;font-weight:680}
.statusDot{width:7px;height:7px;border-radius:50%;background:var(--faint)}
.statusDot.ok{background:var(--ok);box-shadow:0 0 8px rgba(142,240,183,.45)}
.statusDot.bad{background:var(--danger)}
.iconBtn{width:30px;height:30px;border-radius:999px;background:var(--panel);display:grid;place-items:center;color:var(--muted);font-size:14px;font-weight:800}
.main{min-height:0;display:grid;grid-template-columns:1fr}
.rail{position:fixed;left:10px;right:10px;top:calc(56px + env(safe-area-inset-top));max-height:min(62vh,520px);z-index:20;display:none;border:1px solid var(--line);border-radius:18px;background:#111;box-shadow:0 20px 60px rgba(0,0,0,.42);overflow:hidden}
.rail.open{display:grid;grid-template-rows:auto 1fr}
.railHead{height:44px;padding:0 12px;border-bottom:1px solid var(--line);display:flex;align-items:center;justify-content:space-between;color:var(--muted);font-size:12px;font-weight:760;text-transform:uppercase}
.threads{overflow:auto;padding:8px;-webkit-overflow-scrolling:touch}
.thread{width:100%;min-height:56px;padding:8px 10px;border-radius:13px;display:grid;gap:4px;text-align:left;color:var(--text)}
.thread:active,.thread.active{background:var(--panel2)}
.threadName{font-size:14px;font-weight:700;line-height:1.25;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.threadMeta{font-size:11px;color:var(--faint);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.messages{min-height:0;overflow:auto;overscroll-behavior:contain;-webkit-overflow-scrolling:touch;padding:18px max(14px,calc((100vw - 860px)/2)) 8px;scroll-behavior:smooth}
.message{display:flex;margin:0 0 18px}
.message.user{justify-content:flex-end}
.message.assistant{justify-content:flex-start}
.wrap{max-width:min(760px,86%)}
.message.assistant .wrap{width:100%;max-width:none}
.message.user .wrap{max-width:min(720px,82%)}
.meta{margin:0 0 6px;color:var(--faint);font-size:12px;line-height:1}
.message.user .meta{text-align:right}
.bubble{color:var(--text);font-size:15px;line-height:1.62;overflow-wrap:anywhere;white-space:pre-wrap}
.message.user .bubble{padding:11px 14px;border-radius:19px;background:var(--user);line-height:1.48}
.message.assistant .bubble{padding:0;background:transparent}
.empty{height:100%;min-height:260px;display:grid;place-items:center;text-align:center;color:var(--muted);font-size:14px;line-height:1.65;padding:28px}
.composerShell{position:relative;z-index:4;padding:6px max(12px,calc((100vw - 860px)/2)) var(--composer-bottom);background:linear-gradient(180deg,rgba(13,13,13,0),var(--bg) 26%)}
.composer{display:grid;grid-template-columns:minmax(0,1fr) 36px;align-items:end;gap:6px;padding:8px 8px 8px 15px;border:1px solid var(--line);border-radius:29px;background:#151515}
textarea{width:100%;min-height:38px;max-height:150px;resize:none;border:0;outline:0;background:transparent;color:var(--text);padding:8px 2px;font-size:16px;line-height:1.35}
textarea::placeholder{color:var(--faint)}
.send{width:36px;height:36px;border-radius:999px;background:var(--text);color:#111;display:grid;place-items:center;font-size:20px;font-weight:900}
.send:disabled{opacity:.45}
.overlay{position:fixed;inset:0;z-index:40;background:rgba(0,0,0,.72);display:grid;place-items:center;padding:22px;backdrop-filter:blur(10px)}
.pair{width:min(420px,100%);border:1px solid var(--line);border-radius:20px;background:#111;padding:20px;display:grid;gap:14px;box-shadow:0 24px 70px rgba(0,0,0,.55)}
.pair h2{margin:0;font-size:22px;letter-spacing:0}
.pair p{margin:0;color:var(--muted);line-height:1.55;font-size:14px}
.pairRow{display:grid;grid-template-columns:1fr 82px;gap:9px}
input{height:46px;border:1px solid var(--line);border-radius:14px;background:#1b1b1b;color:var(--text);padding:0 13px;font-size:20px;font-family:ui-monospace,SFMono-Regular,"SF Mono",Menlo,Consolas,monospace;outline:0}
.primary{height:46px;border-radius:14px;background:var(--text);color:#111;font-weight:850}
.notice{min-height:18px;color:var(--danger);font-size:13px}
.hidden{display:none!important}
@media(min-width:900px) and (min-height:650px){
  .app{grid-template-columns:clamp(304px,28vw,356px) minmax(0,1fr);grid-template-rows:auto 1fr auto;gap:0;background:var(--bg)}
  .topbar{grid-column:1/-1}
  .rail{display:grid;position:relative;left:auto;right:auto;top:auto;z-index:1;grid-column:1;grid-row:2/4;max-height:none;margin:14px 0 14px 14px;border-radius:18px;box-shadow:none;background:#111}
  .rail:not(.open){display:grid}
  .main{display:contents}
  .messages{grid-column:2;grid-row:2;padding-top:22px}
  .composerShell{grid-column:2;grid-row:3}
  .threadButton{pointer-events:none}
}
@media(max-width:520px){
  .badgeText{display:none}
  .threadButton{max-width:calc(100vw - 150px)}
  .messages{padding:16px 12px 8px}
  .wrap,.message.user .wrap{max-width:88%}
  .message.assistant .wrap{width:100%;max-width:none}
  .bubble{font-size:15px}
  .composerShell{padding-left:12px;padding-right:12px}
}
</style>
</head>
<body>
<div class="app">
  <header class="topbar">
    <button class="threadButton" id="threadButton" type="button"><span class="titleDot" id="titleDot"></span><span class="threadTitle" id="threadTitle">选择线程</span></button>
    <div class="topActions">
      <span class="badge"><span class="statusDot" id="runDot"></span><span class="badgeText" id="runText">空闲</span></span>
      <span class="badge"><span class="statusDot" id="healthDot"></span><span class="badgeText" id="healthText">连接</span></span>
      <button class="iconBtn" id="refresh" title="刷新" type="button">R</button>
    </div>
  </header>
  <main class="main">
    <aside class="rail" id="rail">
      <div class="railHead"><span id="count">0 threads</span><button class="iconBtn" id="closeRail" type="button">X</button></div>
      <div class="threads" id="threads"></div>
    </aside>
    <section class="messages" id="messages"><div class="empty">输入启动窗口里的配对码后，这里会显示 Windows 上的 Codex 会话。</div></section>
  </main>
  <form class="composerShell" id="composer">
    <div class="composer">
      <textarea id="input" placeholder="发给 Codex..." maxlength="8000" rows="1"></textarea>
      <button class="send" id="send" type="submit">↑</button>
    </div>
  </form>
</div>
<div class="overlay" id="overlay">
  <form class="pair" id="pair">
    <h2>配对这台设备</h2>
    <p>输入 Windows 程序窗口中显示的 6 位配对码。令牌只保存在当前浏览器，不放进 URL。</p>
    <div class="pairRow"><input id="code" inputmode="numeric" maxlength="12" placeholder="000000"><button class="primary" type="submit">配对</button></div>
    <div class="notice" id="notice"></div>
  </form>
</div>
<script>
const $=id=>document.getElementById(id);
const st={token:localStorage.getItem('crw.token')||'',threads:[],selected:localStorage.getItem('crw.thread')||'',timer:null};
const el={rail:$('rail'),threadButton:$('threadButton'),threadTitle:$('threadTitle'),threads:$('threads'),count:$('count'),messages:$('messages'),input:$('input'),send:$('send'),overlay:$('overlay'),code:$('code'),notice:$('notice'),healthDot:$('healthDot'),healthText:$('healthText'),runDot:$('runDot'),runText:$('runText')};
function api(p,o={}){const h=o.headers||{};if(st.token)h.authorization='Bearer '+st.token;return fetch(p,{...o,headers:h})}
function dot(node,kind){node.className='statusDot '+(kind||'')}
function showPair(v){el.overlay.classList.toggle('hidden',!v);if(v)setTimeout(()=>el.code.focus(),80)}
function empty(t){el.messages.textContent='';const d=document.createElement('div');d.className='empty';d.textContent=t;el.messages.appendChild(d)}
function selected(){return st.threads.find(x=>x.id===st.selected)}
function when(v){const t=Date.parse(v||'');return Number.isFinite(t)?new Date(t).toLocaleString([],{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'}):''}
function setHeader(){const x=selected();el.threadTitle.textContent=x?(x.title||'(untitled)'):'选择线程';dot(el.runDot,x&&x.active?'ok':'');el.runText.textContent=x&&x.active?'运行中':'空闲'}
function renderThreads(){el.threads.textContent='';el.count.textContent=st.threads.length+' threads';for(const x of st.threads){const b=document.createElement('button');b.type='button';b.className='thread '+(x.id===st.selected?'active':'');b.onclick=()=>selectThread(x.id);const n=document.createElement('div');n.className='threadName';n.textContent=x.title||'(untitled)';const m=document.createElement('div');m.className='threadMeta';m.textContent=(x.active?'运行中':'空闲')+' · '+when(x.updatedAt);const p=document.createElement('div');p.className='threadMeta';p.textContent=x.cwd||x.preview||'';b.append(n,m,p);el.threads.appendChild(b)}setHeader()}
function addMsg(role,text,label){const a=document.createElement('article');a.className='message '+role;const w=document.createElement('div');w.className='wrap';const m=document.createElement('div');m.className='meta';m.textContent=label||(role==='user'?'你':'Codex');const b=document.createElement('div');b.className='bubble';b.textContent=text||'';w.append(m,b);a.appendChild(w);el.messages.appendChild(a);el.messages.scrollTop=el.messages.scrollHeight}
async function health(){try{const r=await api('/api/health');const d=await r.json();dot(el.healthDot,'ok');el.healthText.textContent=st.token?'已配对':'待配对'}catch{dot(el.healthDot,'bad');el.healthText.textContent='离线'}}
async function loadThreads(){const r=await api('/api/threads');const d=await r.json().catch(()=>({}));if(r.status===401)return expire();if(!r.ok||!d.ok)throw new Error(d.message||'读取线程失败');st.threads=d.threads||[];if(!st.selected||!st.threads.some(x=>x.id===st.selected))st.selected=st.threads[0]?.id||'';if(st.selected)localStorage.setItem('crw.thread',st.selected);renderThreads();if(st.selected)await history(st.selected);else empty('还没有找到 Codex 会话。先在 Windows 上打开 Codex Desktop。')}
async function history(id){empty('加载中...');const r=await api('/api/history?thread='+encodeURIComponent(id));const d=await r.json().catch(()=>({}));if(r.status===401)return expire();if(!r.ok||!d.ok)throw new Error(d.message||'读取历史失败');el.messages.textContent='';if(!d.available||!d.messages||!d.messages.length)return empty('这个线程暂无可显示的历史。');for(const x of d.messages)addMsg(x.role,x.text,x.role==='user'?'你':'Codex')}
async function selectThread(id){st.selected=id;localStorage.setItem('crw.thread',id);el.rail.classList.remove('open');renderThreads();await history(id);startPoll()}
async function poll(){if(!st.selected)return;try{const r=await api('/api/status?thread='+encodeURIComponent(st.selected));const d=await r.json();if(r.status===401)return expire();dot(el.runDot,d.active?'ok':'');el.runText.textContent=d.active?'运行中':'空闲'}catch{}}
function startPoll(){if(st.timer)clearInterval(st.timer);poll();st.timer=setInterval(poll,2500)}
async function send(text){el.send.disabled=true;addMsg('user',text,'你');el.input.value='';autosize();try{const r=await api('/api/send',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({text,threadId:st.selected||''})});const d=await r.json().catch(()=>({}));if(r.status===401)return expire();if(!r.ok||!d.ok)throw new Error(d.message||'发送失败');dot(el.runDot,'ok');el.runText.textContent=d.queued?'已排队':'已发送';startPoll()}catch(e){addMsg('assistant',e.message||'发送失败','系统')}finally{el.send.disabled=false;el.input.focus()}}
function expire(){st.token='';localStorage.removeItem('crw.token');showPair(true);dot(el.healthDot,'');el.healthText.textContent='待配对';empty('会话已过期，请重新配对。')}
async function boot(){await health();if(!st.token)return showPair(true);showPair(false);try{await loadThreads();startPoll()}catch(e){empty(e.message||'启动失败')}}
function autosize(){el.input.style.height='auto';el.input.style.height=Math.min(el.input.scrollHeight,150)+'px'}
$('pair').onsubmit=e=>{e.preventDefault();el.notice.textContent='';fetch('/api/pair',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({code:el.code.value.trim(),deviceName:navigator.userAgent.slice(0,60)})}).then(r=>r.json().then(d=>{if(!r.ok||!d.ok)throw new Error(d.message||'配对失败');st.token=d.token;localStorage.setItem('crw.token',st.token);showPair(false);boot()})).catch(e=>el.notice.textContent=e.message||'配对失败')};
$('composer').onsubmit=e=>{e.preventDefault();const t=el.input.value.trim();if(t)send(t)};
el.input.addEventListener('input',autosize);
el.input.addEventListener('keydown',e=>{if(e.key==='Enter'&&!e.shiftKey&&!e.isComposing){e.preventDefault();const t=el.input.value.trim();if(t)send(t)}});
el.threadButton.onclick=()=>el.rail.classList.toggle('open');
$('closeRail').onclick=()=>el.rail.classList.remove('open');
$('refresh').onclick=()=>loadThreads().catch(e=>empty(e.message||'刷新失败'));
window.addEventListener('focus',()=>{health();if(st.token)loadThreads().catch(()=>{})});
boot();
</script>
</body>
</html>`
