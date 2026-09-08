package main

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

type previewDesktop struct {
	mu      sync.Mutex
	prompts map[string]string
	sends   int
	live    int
}

const previewSecondID = "66666666-2222-4333-8444-555555555555"

func (p *previewDesktop) Call(_ context.Context, tool string, args map[string]any, _ string) (map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch tool {
	case "list_threads":
		return map[string]any{"threads": []any{
			map[string]any{"id": testThreadID, "kind": "codex", "hostId": "local", "title": "附件和项目任务测试", "projectId": "preview-project", "cwd": "C:\\Preview", "status": "idle"},
			map[string]any{"id": previewSecondID, "kind": "codex", "hostId": "local", "title": "第二个任务", "projectId": "preview-project", "cwd": "C:\\Preview", "status": "idle"},
		}}, nil
	case "list_archived_threads":
		return map[string]any{"threads": []any{}}, nil
	case "list_projects":
		return map[string]any{"projects": []any{map[string]any{"projectId": "preview-project", "hostId": "local", "label": "测试项目", "isGitRepository": true, "path": "C:\\Preview"}}}, nil
	case "read_thread":
		id := asString(args["threadId"])
		question := p.prompts[id]
		if question == "" {
			question = "测试消息"
		}
		status := "idle"
		items := []any{map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": question}}}}
		if p.live > 0 {
			status = "active"
			items = append(items, map[string]any{"type": "agentMessage", "phase": "commentary", "text": "正在生成第一段回复。"})
			if p.live >= 2 {
				items = append(items, map[string]any{"type": "agentMessage", "phase": "commentary", "text": "继续补充第二段回复。"})
			}
			if p.live >= 3 {
				status = "idle"
				items = append(items, map[string]any{"type": "agentMessage", "phase": "final", "text": "实时回复完成。"})
			}
		} else {
			items = append(items, map[string]any{"type": "agentMessage", "phase": "final", "text": "这是浏览器自动化测试的模拟回复。\n\n```go\nfmt.Println(\"hello\")\n```"})
		}
		return map[string]any{"thread": map[string]any{"id": id, "status": map[string]any{"type": status}}, "page": map[string]any{"hasMore": false}, "turns": []any{map[string]any{"items": items}}}, nil
	case "send_message_to_thread":
		p.prompts[asString(args["threadId"])] = asString(args["prompt"])
		p.sends++
		return map[string]any{"threadId": args["threadId"], "status": "sent"}, nil
	case "create_thread":
		p.prompts[previewSecondID] = asString(args["prompt"])
		p.sends++
		return map[string]any{"threadId": previewSecondID}, nil
	}
	return map[string]any{"ok": true}, nil
}

func TestWebPreview(t *testing.T) {
	if os.Getenv("CRW_UI_TEST") != "1" {
		t.Skip("local browser test helper")
	}
	preview := &previewDesktop{prompts: map[string]string{}}
	state, _ := testServer(t, preview)
	mux := http.NewServeMux()
	server := &http.Server{Addr: "127.0.0.1:8790", Handler: mux}
	mux.HandleFunc("/test/shutdown", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		go server.Shutdown(context.Background())
	})
	mux.HandleFunc("/test/state", func(w http.ResponseWriter, r *http.Request) {
		preview.mu.Lock()
		defer preview.mu.Unlock()
		writeJSON(w, 200, map[string]any{"sends": preview.sends})
	})
	mux.HandleFunc("/test/live", func(w http.ResponseWriter, r *http.Request) {
		stage, _ := strconv.Atoi(r.URL.Query().Get("stage"))
		preview.mu.Lock()
		preview.live = stage
		preview.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true, "stage": stage})
	})
	mux.HandleFunc("/", state.handle)
	timer := time.AfterFunc(8*time.Minute, func() { server.Close() })
	defer timer.Stop()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
