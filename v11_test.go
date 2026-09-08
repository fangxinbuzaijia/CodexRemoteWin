package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testThreadID = "11111111-2222-4333-8444-555555555555"

type fakeDesktop struct {
	sendCount int
	uncertain bool
	rejected  bool
	project   bool
	lastArgs  map[string]any
	lastTool  string
}

func (f *fakeDesktop) Call(_ context.Context, tool string, args map[string]any, _ string) (map[string]any, error) {
	switch tool {
	case "get_usage_limits":
		return map[string]any{
			"accountId": "must-not-leak",
			"rateLimitsByLimitId": map[string]any{"codex": map[string]any{
				"planType":  "plus",
				"primary":   map[string]any{"usedPercent": 58, "windowDurationMins": 300, "resetsAt": 1788878855},
				"secondary": map[string]any{"usedPercent": 13, "windowDurationMins": 10080, "resetsAt": 1789447374},
				"credits":   map[string]any{"hasCredits": false, "unlimited": false, "balance": "0"},
			}},
			"rateLimitResetCredits": map[string]any{"availableCount": 1, "credits": []any{map[string]any{"id": "secret-reset-id"}}},
		}, nil
	case "read_thread":
		return map[string]any{"thread": map[string]any{"id": args["threadId"], "title": "renamed"}}, nil
	case "list_projects":
		return map[string]any{"projects": []any{map[string]any{"projectId": "local-project", "hostId": "local", "isGitRepository": true}}}, nil
	case "send_message_to_thread", "create_thread":
		f.sendCount++
		f.lastArgs = args
		f.lastTool = tool
		if f.uncertain {
			return nil, &desktopError{Message: "test receipt timeout", Uncertain: true}
		}
		if f.rejected {
			return nil, &desktopError{Message: "test rejected"}
		}
		return map[string]any{"ok": true, "threadId": testThreadID}, nil
	case "set_thread_title":
		f.lastArgs = args
		f.lastTool = tool
		return map[string]any{"ok": true}, nil
	}
	return map[string]any{"threads": []any{}, "pinnedThreads": []any{}}, nil
}

func TestUsageEndpointReturnsOnlyDisplayData(t *testing.T) {
	s, _ := testServer(t, &fakeDesktop{})
	response := testRequest(s, "GET", "/api/usage", nil)
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	result := decodeTest(t, response)
	windows := asSlice(result["windows"])
	if result["available"] != true || len(windows) != 2 || asString(result["planType"]) != "plus" {
		t.Fatal(result)
	}
	primary, secondary := asMap(windows[0]), asMap(windows[1])
	if intFromAny(primary["remainingPercent"]) != 42 || asString(primary["label"]) != "5 小时" || asString(primary["resetsAt"]) == "" {
		t.Fatal(primary)
	}
	if intFromAny(secondary["remainingPercent"]) != 87 || asString(secondary["label"]) != "7 天" {
		t.Fatal(secondary)
	}
	serialized := response.Body.String()
	if strings.Contains(serialized, "must-not-leak") || strings.Contains(serialized, "secret-reset-id") || strings.Contains(serialized, "accountId") {
		t.Fatal("private desktop usage fields leaked to browser")
	}
}

func testServer(t *testing.T, desktop desktopCaller) (*serverState, session) {
	t.Helper()
	sess := session{TokenHash: hash("test-token"), ExpiresAt: time.Now().Add(time.Hour)}
	s := &serverState{dataDir: t.TempDir(), home: t.TempDir(), audit: log.New(io.Discard, "", 0), desktop: desktop, sessions: map[string]session{sess.TokenHash: sess}, trusted: map[string]session{}}
	return s, sess
}
func testRequest(s *serverState, method, path string, body any) *httptest.ResponseRecorder {
	encoded, _ := json.Marshal(body)
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	s.handle(response, request)
	return response
}
func decodeTest(t *testing.T, r *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
		t.Fatal(err, r.Body.String())
	}
	return result
}
func TestDeliveryIdempotenceAndConflict(t *testing.T) {
	fake := &fakeDesktop{}
	s, _ := testServer(t, fake)
	input := deliveryInput{RequestID: "same-id", ThreadID: testThreadID, Text: "hello"}
	for i := 0; i < 2; i++ {
		result := decodeTest(t, testRequest(s, "POST", "/api/deliver", input))
		if asMap(result["receipt"])["state"] != "accepted" {
			t.Fatal(result)
		}
	}
	if fake.sendCount != 1 {
		t.Fatalf("sent %d times", fake.sendCount)
	}
	input.Text = "different"
	if r := testRequest(s, "POST", "/api/deliver", input); r.Code != 409 {
		t.Fatal(r.Code)
	}
}
func TestUncertainDeliverySurvivesRestart(t *testing.T) {
	fake := &fakeDesktop{uncertain: true}
	s, sess := testServer(t, fake)
	input := deliveryInput{RequestID: "uncertain", ThreadID: testThreadID, Text: "hello"}
	result := decodeTest(t, testRequest(s, "POST", "/api/deliver", input))
	if asMap(result["receipt"])["state"] != "unknown" {
		t.Fatal(result)
	}
	restarted := &serverState{dataDir: s.dataDir, home: s.home, audit: s.audit, desktop: fake, sessions: map[string]session{sess.TokenHash: sess}, trusted: map[string]session{}}
	testRequest(restarted, "POST", "/api/deliver", input)
	if fake.sendCount != 1 {
		t.Fatal("uncertain send replayed after restart")
	}
	result = decodeTest(t, testRequest(restarted, "GET", "/api/receipt?id=uncertain", nil))
	if asMap(result["receipt"])["state"] != "unknown" {
		t.Fatal(result)
	}
}
func TestCreateProjectUsesActualProjectAndEnvironment(t *testing.T) {
	fake := &fakeDesktop{}
	s, _ := testServer(t, fake)
	input := deliveryInput{RequestID: "new-project", ProjectID: "local-project", Text: "first"}
	r := testRequest(s, "POST", "/api/new-thread", input)
	if !strings.Contains(r.Body.String(), "accepted") {
		t.Fatal(r.Body.String())
	}
	target := asMap(fake.lastArgs["target"])
	if target["projectId"] != "local-project" || asMap(target["environment"])["type"] != "worktree" {
		t.Fatal(target)
	}
	input.RequestID = "invalid-project"
	input.ProjectID = "unknown"
	r = testRequest(s, "POST", "/api/new-thread", input)
	if r.Code != 400 || fake.sendCount != 1 {
		t.Fatal("unknown project created a task")
	}
}

func TestReceiptReconciliationIgnoresHistoricalMessage(t *testing.T) {
	fake := &fakeDesktop{uncertain: true}
	s, _ := testServer(t, fake)
	os.MkdirAll(s.sessionsDir(), 0700)
	path := filepath.Join(s.sessionsDir(), "rollout-"+testThreadID+".jsonl")
	line := `{"type":"event_msg","payload":{"type":"user_message","message":"hello"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	input := deliveryInput{RequestID: "reconcile", ThreadID: testThreadID, Text: "hello"}
	testRequest(s, "POST", "/api/deliver", input)
	result := decodeTest(t, testRequest(s, "GET", "/api/receipt?id=reconcile", nil))
	if asMap(result["receipt"])["state"] != "unknown" {
		t.Fatal("historical message counted as new receipt")
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString(line)
	file.Close()
	result = decodeTest(t, testRequest(s, "GET", "/api/receipt?id=reconcile", nil))
	if asMap(result["receipt"])["state"] != "accepted" || fake.sendCount != 1 {
		t.Fatal(result)
	}
}

func TestLargeAttachmentsUseSmallDeliveryRequest(t *testing.T) {
	fake := &fakeDesktop{}
	s, _ := testServer(t, fake)
	input := deliveryInput{RequestID: "large-files", ThreadID: testThreadID, Text: "read files"}
	for i := 0; i < 3; i++ {
		r := testUpload(t, s, "large.txt", make([]byte, maxAttachmentBytes))
		if r.Code != 200 {
			t.Fatal(r.Body.String())
		}
		input.AttachmentIDs = append(input.AttachmentIDs, asString(asMap(decodeTest(t, r)["attachment"])["id"]))
	}
	r := testRequest(s, "POST", "/api/deliver", input)
	if asMap(decodeTest(t, r)["receipt"])["state"] != "accepted" {
		t.Fatal(r.Body.String())
	}
	if len(asString(fake.lastArgs["prompt"])) > 4096 {
		t.Fatal("binary content included in message")
	}
}

func TestAttachmentOnlyHistoryRestoresCards(t *testing.T) {
	s, sess := testServer(t, &fakeDesktop{})
	upload := asMap(decodeTest(t, testUpload(t, s, "notes.txt", []byte("notes")))["attachment"])
	input := deliveryInput{RequestID: "attachment-only", ThreadID: testThreadID, AttachmentIDs: []string{asString(upload["id"])}}
	result := decodeTest(t, testRequest(s, "POST", "/api/deliver", input))
	if asMap(result["receipt"])["state"] != "accepted" {
		t.Fatal(result)
	}
	record, err := s.readDelivery(sess, input.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	history := s.restoreAttachmentMessages(testThreadID, []messageRow{{Role: "user", Text: strings.TrimSpace(record.Prompt)}})
	if history[0].Text != "" || len(history[0].Attachments) != 1 {
		t.Fatal("attachment-only card was not restored")
	}
}

func TestAuthoritativeRename(t *testing.T) {
	fake := &fakeDesktop{}
	s, _ := testServer(t, fake)
	response := testRequest(s, "POST", "/api/thread-action", map[string]any{"threadId": testThreadID, "action": "rename", "name": "renamed"})
	result := decodeTest(t, response)
	if result["confirmed"] != true || fake.lastTool != "set_thread_title" {
		t.Fatal(result)
	}
	if _, err := os.Stat(s.localStatePath()); !os.IsNotExist(err) {
		t.Fatal("rename wrote local override")
	}
}
func testUpload(t *testing.T, s *serverState, name string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", name)
	part.Write(content)
	writer.Close()
	request := httptest.NewRequest("POST", "/api/upload", &body)
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	s.handle(response, request)
	return response
}
func TestAttachmentLifecycleAndAuthenticatedDownload(t *testing.T) {
	fake := &fakeDesktop{}
	s, sess := testServer(t, fake)
	response := testUpload(t, s, "notes.txt", []byte("private attachment"))
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	upload := asMap(decodeTest(t, response)["attachment"])
	id := asString(upload["id"])
	input := deliveryInput{RequestID: "file-message", ThreadID: testThreadID, Text: "read this", AttachmentIDs: []string{id}}
	sent := testRequest(s, "POST", "/api/deliver", input)
	if sent.Code != 200 {
		t.Fatal(sent.Body.String())
	}
	record, err := s.readDelivery(sess, input.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(record.Prompt, "notes.txt") {
		t.Fatal("missing local reference")
	}
	history := s.restoreAttachmentMessages(testThreadID, []messageRow{{Role: "user", Text: record.Prompt}})
	if history[0].Text != input.Text || len(history[0].Attachments) != 1 {
		t.Fatal(history)
	}
	download := testRequest(s, "GET", "/api/attachment?id="+id, nil)
	if download.Code != 200 || download.Body.String() != "private attachment" {
		t.Fatal(download.Body.String())
	}
	request := httptest.NewRequest("GET", "/api/attachment?id="+id, nil)
	unauthorized := httptest.NewRecorder()
	s.handle(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatal("attachment accessible without authentication")
	}
	if testRequest(s, "GET", "/api/attachment?id=../../auth.json", nil).Code != 404 {
		t.Fatal("path traversal not rejected")
	}
}
func TestUploadSizeAndMultipleFileLimits(t *testing.T) {
	s, _ := testServer(t, &fakeDesktop{})
	if r := testUpload(t, s, "too-big.txt", make([]byte, maxAttachmentBytes+1)); r.Code != 413 {
		t.Fatal(r.Code)
	}
	if r := testUpload(t, s, "empty.txt", nil); r.Code != 413 {
		t.Fatal(r.Code)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for i := 0; i < 2; i++ {
		part, _ := writer.CreateFormFile("file", "a.txt")
		part.Write([]byte("one"))
	}
	writer.Close()
	request := httptest.NewRequest("POST", "/api/upload", &body)
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	s.handle(response, request)
	if response.Code != 400 {
		t.Fatal(response.Code)
	}
}
func TestDesktopRowsExcludeNonlocalAndCloud(t *testing.T) {
	result := map[string]any{"threads": []any{
		map[string]any{"id": testThreadID, "kind": "codex", "hostId": "local", "title": "local"},
		map[string]any{"id": testThreadID, "kind": "codex", "hostId": "remote", "title": "remote"},
		map[string]any{"id": testThreadID, "kind": "chatgpt", "hostId": "local", "title": "cloud"},
	}}
	rows := desktopRows(result, false)
	if len(rows) != 1 || rows[0].Title != "local" {
		t.Fatal(rows)
	}
}
func TestDesktopMessagesPreserveRoles(t *testing.T) {
	data := map[string]any{"turns": []any{map[string]any{"items": []any{
		map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "question"}}},
		map[string]any{"type": "agentMessage", "phase": "commentary", "text": "working"},
		map[string]any{"type": "agentMessage", "phase": "final", "text": "answer"},
	}}}}
	messages, steps := desktopMessages(data)
	if len(messages) != 3 || messages[0].Role != "user" || messages[1].Text != "working" || messages[2].Text != "answer" || len(steps) != 1 {
		t.Fatal(messages, steps)
	}
}
func TestProjectPageBootsV11Once(t *testing.T) {
	page := projectPage()
	if strings.Contains(page, "});boot();") || strings.Count(page, `src="/v11.js"`) != 1 || strings.Count(page, `href="/theme.css"`) != 1 {
		t.Fatal("legacy boot still executes")
	}
	if strings.Index(page, `localStorage.getItem("crw.theme")`) > strings.Index(page, "<body>") {
		t.Fatal("theme must be selected before the body is rendered")
	}
}
func TestLiveDesktopReadOnly(t *testing.T) {
	if os.Getenv("CRW_TEST_DESKTOP") != "1" {
		t.Skip("opt-in local desktop read-only check")
	}
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CODEX_APP_TOOLS_PIPE_PATH", "")
	t.Setenv("CODEX_REMOTE_DESKTOP_PIPE", "")
	home, _ := os.UserHomeDir()
	s, _ := testServer(t, nil)
	s.home = home
	s.desktop = &nativeDesktop{contextID: s.desktopContextID()}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, projects, err := s.desktopSnapshot(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 || len(projects) == 0 {
		t.Fatal("desktop snapshot empty")
	}
	usage, err := s.desktopCall(ctx, "get_usage_limits", map[string]any{}, "")
	if err != nil {
		t.Fatal(err)
	}
	normalized := normalizeUsage(usage)
	if normalized["available"] != true || len(asSlice(normalized["windows"])) == 0 {
		t.Fatal("desktop usage limits unavailable")
	}
	t.Logf("Verified %d desktop tasks, %d projects, and %d usage windows", len(rows), len(projects), len(asSlice(normalized["windows"])))
}
