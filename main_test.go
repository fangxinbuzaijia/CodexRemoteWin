package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestMessageLineContainsTextFormats(t *testing.T) {
	target := "手机发送回执测试"
	lines := [][]byte{
		[]byte(`{"type":"event_msg","payload":{"type":"user_message","message":"手机发送回执测试\n"}}`),
		[]byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"手机发送回执测试"}]}}`),
	}
	for i, line := range lines {
		if !messageLineContainsText(line, target) {
			t.Fatalf("format %d was not recognized", i)
		}
	}
}

func TestWaitForUserMessageAfterOffset(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "session-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	path := file.Name()
	if _, err := file.WriteString(`{"type":"event_msg","payload":{"type":"task_complete"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	offset := info.Size()
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(80 * time.Millisecond)
		out, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if openErr != nil {
			return
		}
		defer out.Close()
		_, _ = out.WriteString(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"已由 Codex 接收"}]}}` + "\n")
	}()

	if !waitForUserMessage(path, "已由 Codex 接收", offset, 2*time.Second) {
		t.Fatal("appended user message was not detected")
	}
}

func TestIsSessionActiveBeyondNormalTail(t *testing.T) {
	path := t.TempDir() + `\long-session.jsonl`
	content := `{"type":"event_msg","payload":{"type":"task_started"}}` + "\n" + strings.Repeat("x", 6*1024*1024) + "\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if !isSessionActive(path) {
		t.Fatal("long running session was reported idle")
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"event_msg","payload":{"type":"task_complete"}}` + "\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	_ = file.Close()
	if isSessionActive(path) {
		t.Fatal("completed session was reported active")
	}
}
