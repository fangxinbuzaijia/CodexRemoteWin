package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
)

// Desktop's local app-tools transport is version-dependent. Discover capabilities
// before exposing controls; never fall back to GUI clicks after an ambiguous send.
type desktopCaller interface {
	Call(context.Context, string, map[string]any, string) (map[string]any, error)
}

type desktopError struct {
	Message   string
	Uncertain bool
}

func (e *desktopError) Error() string { return e.Message }

type nativeDesktop struct {
	mu        sync.Mutex
	pipe      string
	contextID string
}

func (s *serverState) desktopContextID() string {
	if id := os.Getenv("CODEX_THREAD_ID"); validThreadID(id) {
		return id
	}
	if id := os.Getenv("CODEX_REMOTE_CONTEXT_THREAD"); validThreadID(id) {
		return id
	}
	// An existing local task supplies the context required by app-tools.
	items := parseJSONLTail(s.indexPath(), 1024*1024)
	for i := len(items) - 1; i >= 0; i-- {
		if id := asString(items[i]["id"]); validThreadID(id) {
			return id
		}
	}
	return ""
}

func desktopPipePaths() []string {
	paths := []string{}
	for _, key := range []string{"CODEX_REMOTE_DESKTOP_PIPE", "CODEX_APP_TOOLS_PIPE_PATH"} {
		if p := os.Getenv(key); strings.HasPrefix(p, `\\.\pipe\codex-browser-use-`) {
			paths = append(paths, p)
		}
	}
	var data syscall.Win32finddata
	h, err := syscall.FindFirstFile(syscall.StringToUTF16Ptr(`\\.\pipe\codex-browser-use-*`), &data)
	if err == nil {
		defer syscall.FindClose(h)
		for {
			paths = append(paths, `\\.\pipe\`+syscall.UTF16ToString(data.FileName[:]))
			if syscall.FindNextFile(h, &data) != nil {
				break
			}
		}
	}
	return paths
}

func pipeRPC(ctx context.Context, pipe, method string, params map[string]any) (map[string]any, error) {
	conn, err := winio.DialPipeContext(ctx, pipe)
	if err != nil {
		return nil, &desktopError{Message: "Cannot connect to desktop: " + err.Error()}
	}
	defer conn.Close()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-finished:
		}
	}()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(45 * time.Second)
	}
	_ = conn.SetDeadline(deadline)
	request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if len(request) > 8*1024*1024 {
		return nil, errors.New("Desktop request too large")
	}
	frame := make([]byte, 4+len(request))
	binary.LittleEndian.PutUint32(frame, uint32(len(request)))
	copy(frame[4:], request)
	for len(frame) > 0 {
		n, e := conn.Write(frame)
		if e != nil {
			return nil, &desktopError{Message: "Desktop delivery could not be confirmed: " + e.Error(), Uncertain: true}
		}
		if n == 0 {
			return nil, &desktopError{Message: "Desktop connection stopped writing", Uncertain: true}
		}
		frame = frame[n:]
	}
	var size [4]byte
	if _, err = io.ReadFull(conn, size[:]); err != nil {
		return nil, &desktopError{Message: "Desktop receipt timed out; do not resend yet", Uncertain: true}
	}
	length := binary.LittleEndian.Uint32(size[:])
	if length > 8*1024*1024 {
		return nil, &desktopError{Message: "Desktop response too large", Uncertain: true}
	}
	body := make([]byte, length)
	if _, err = io.ReadFull(conn, body); err != nil {
		return nil, &desktopError{Message: "Incomplete desktop receipt", Uncertain: true}
	}
	var response map[string]any
	if json.Unmarshal(body, &response) != nil {
		return nil, &desktopError{Message: "Invalid desktop receipt", Uncertain: true}
	}
	if problem := asMap(response["error"]); len(problem) > 0 {
		return nil, &desktopError{Message: asString(problem["message"])}
	}
	result, ok := response["result"].(map[string]any)
	if !ok {
		return nil, &desktopError{Message: "Missing desktop result", Uncertain: true}
	}
	return result, nil
}

func (d *nativeDesktop) Call(ctx context.Context, tool string, args map[string]any, requestID string) (map[string]any, error) {
	d.mu.Lock()
	pipe := d.pipe
	d.mu.Unlock()
	if pipe == "" {
		for _, candidate := range desktopPipePaths() {
			probe, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
			catalog, err := pipeRPC(probe, candidate, "tools/list", map[string]any{"threadStartKind": "all"})
			cancel()
			if err != nil {
				continue
			}
			found := false
			for _, entry := range asSlice(catalog["tools"]) {
				if asString(asMap(entry)["name"]) == tool {
					found = true
					break
				}
			}
			if found {
				pipe = candidate
				break
			}
		}
		if pipe == "" {
			return nil, &desktopError{Message: "Desktop bridge unavailable. Open a task in the current ChatGPT/Codex desktop app."}
		}
		d.mu.Lock()
		d.pipe = pipe
		d.mu.Unlock()
	}
	origin := d.contextID
	if origin == "" {
		origin = asString(args["threadId"])
	}
	if !validThreadID(origin) {
		return nil, errors.New("Open an existing task in Codex Desktop before connecting")
	}
	if requestID == "" {
		requestID = randomToken()
	}
	raw, err := pipeRPC(ctx, pipe, "tools/call", map[string]any{
		"namespace": "codex_app", "tool": tool, "arguments": args,
		"threadId": origin, "callId": "crw-" + requestID, "turnId": "crw-" + requestID,
	})
	if err != nil {
		d.mu.Lock()
		d.pipe = ""
		d.mu.Unlock()
		return nil, err
	}
	var texts []string
	for _, item := range asSlice(raw["contentItems"]) {
		block := asMap(item)
		if asString(block["type"]) == "inputText" {
			texts = append(texts, asString(block["text"]))
		}
	}
	if raw["success"] != true {
		return nil, &desktopError{Message: strings.Join(texts, "\n")}
	}
	for _, text := range texts {
		var result map[string]any
		if json.Unmarshal([]byte(text), &result) == nil && result != nil {
			return result, nil
		}
	}
	return map[string]any{"ok": true, "message": strings.Join(texts, "\n")}, nil
}

func (s *serverState) desktopCall(ctx context.Context, tool string, args map[string]any, requestID string) (map[string]any, error) {
	if s.desktop == nil {
		return nil, errors.New("Desktop bridge unavailable")
	}
	child, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	return s.desktop.Call(child, tool, args, requestID)
}

func desktopRows(result map[string]any, archived bool) []threadRow {
	rows := []threadRow{}
	for _, key := range []string{"pinnedThreads", "threads"} {
		for _, item := range asSlice(result[key]) {
			x := asMap(item)
			if asString(x["kind"]) != "codex" || (asString(x["hostId"]) != "" && asString(x["hostId"]) != "local") {
				continue
			}
			id := asString(x["id"])
			if !validThreadID(id) {
				continue
			}
			updated := time.Unix(int64(intFromAny(x["updatedAt"])), 0)
			cwd := asString(x["cwd"])
			status := asString(x["status"])
			rows = append(rows, threadRow{ID: id, Title: asString(x["title"]), Preview: asString(x["summary"]), Cwd: cwd,
				UpdatedAt: updated.Format(time.RFC3339), Status: status, Active: status == "active" || status == "running",
				Archived: archived, Pinned: key == "pinnedThreads", ProjectKey: asString(x["projectId"]), ProjectName: filepath.Base(cwd), mtime: updated})
		}
	}
	return rows
}

func (s *serverState) desktopSnapshot(ctx context.Context, archived bool) ([]threadRow, []map[string]any, error) {
	result, err := s.desktopCall(ctx, "list_threads", map[string]any{"limit": 50}, "")
	if err != nil {
		return nil, nil, err
	}
	rows := desktopRows(result, false)
	if archived {
		old, e := s.desktopCall(ctx, "list_archived_threads", map[string]any{"limit": 50, "hostId": "local"}, "")
		if e != nil {
			return nil, nil, e
		}
		rows = append(rows, desktopRows(old, true)...)
	}
	projectResult, err := s.desktopCall(ctx, "list_projects", map[string]any{}, "")
	if err != nil {
		return nil, nil, err
	}
	projects := []map[string]any{}
	names := map[string]string{}
	for _, p := range asSlice(projectResult["projects"]) {
		x := asMap(p)
		if asString(x["hostId"]) != "local" {
			continue
		}
		names[asString(x["projectId"])] = asString(x["label"])
		projects = append(projects, x)
	}
	for i := range rows {
		if n := names[rows[i].ProjectKey]; n != "" {
			rows[i].ProjectName = n
		}
	}
	return rows, projects, nil
}

func (s *serverState) desktopThread(ctx context.Context, id string) (map[string]any, error) {
	if !validThreadID(id) {
		return nil, errors.New("Invalid thread id")
	}
	return s.desktopCall(ctx, "read_thread", map[string]any{"threadId": id, "hostId": "local", "turnLimit": 1, "maxOutputCharsPerItem": 0}, "")
}
