package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (s *serverState) handleV11API(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	methods := map[string]string{
		"/api/threads": "GET", "/api/history": "GET", "/api/status": "GET", "/api/usage": "GET", "/api/thread-action": "POST",
		"/api/upload": "POST", "/api/attachment": "GET", "/api/deliver": "POST", "/api/new-thread": "POST",
		"/api/receipt": "GET", "/api/send": "POST", "/api/stop": "POST",
	}
	method, handled := methods[path]
	if !handled {
		return false
	}
	sess, ok := s.requireSession(w, r)
	if !ok {
		return true
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeJSON(w, 405, errJSON("METHOD_NOT_ALLOWED", "不支持此请求方法"))
		return true
	}
	switch path {
	case "/api/usage":
		data, err := s.desktopCall(r.Context(), "get_usage_limits", map[string]any{}, "")
		if err != nil {
			writeJSON(w, 503, errJSON("USAGE_UNAVAILABLE", "暂时无法读取 Codex 额度"))
			break
		}
		writeJSON(w, 200, normalizeUsage(data))
	case "/api/stop":
		writeJSON(w, 501, errJSON("STOP_UNAVAILABLE", "当前桌面接口不提供停止操作，请在桌面停止任务"))
	case "/api/send":
		writeJSON(w, 409, errJSON("CLIENT_OUTDATED", "网页版本已更新，请刷新页面后发送"))
	case "/api/upload":
		s.handleUpload(w, r, sess)
	case "/api/attachment":
		s.serveUpload(w, r)
	case "/api/deliver":
		s.handleDelivery(w, r, sess, false)
	case "/api/new-thread":
		s.handleDelivery(w, r, sess, true)
	case "/api/receipt":
		s.sendMu.Lock()
		defer s.sendMu.Unlock()
		record, err := s.readDelivery(sess, r.URL.Query().Get("id"))
		if err != nil {
			writeJSON(w, 404, errJSON("RECEIPT_NOT_FOUND", "暂未找到回执，请勿重复发送"))
			break
		}
		record = s.reconcileDelivery(sess, record)
		writeJSON(w, 200, map[string]any{"ok": true, "receipt": record})
	case "/api/threads":
		rows, projects, err := s.desktopSnapshot(r.Context(), r.URL.Query().Get("includeArchived") == "1")
		if err != nil {
			writeJSON(w, 503, errJSON("DESKTOP_UNAVAILABLE", err.Error()))
			break
		}
		writeJSON(w, 200, map[string]any{"ok": true, "threads": rows, "projects": projects, "source": "desktop", "limit": 50})
	case "/api/history", "/api/status":
		id := r.URL.Query().Get("thread")
		if !validThreadID(id) {
			writeJSON(w, 400, errJSON("BAD_THREAD_ID", "任务编号不正确"))
			break
		}
		args := map[string]any{"threadId": id, "hostId": "local", "turnLimit": 10, "includeOutputs": false, "maxOutputCharsPerItem": 20000}
		if cursor := r.URL.Query().Get("cursor"); cursor != "" {
			args["cursor"] = cursor
		}
		data, err := s.desktopCall(r.Context(), "read_thread", args, "")
		if err != nil {
			writeJSON(w, 503, errJSON("DESKTOP_UNAVAILABLE", err.Error()))
			break
		}
		thread := asMap(data["thread"])
		if asString(thread["id"]) != id {
			writeJSON(w, 502, errJSON("WRONG_THREAD", "桌面返回了不同任务，已停止更新"))
			break
		}
		messages, steps := desktopMessages(data)
		messages = s.restoreAttachmentMessages(id, messages)
		status := asString(asMap(thread["status"])["type"])
		page := asMap(data["page"])
		writeJSON(w, 200, map[string]any{"ok": true, "threadId": id, "available": true, "active": status == "active" || status == "running", "status": status, "messages": messages, "steps": steps, "hasMore": page["hasMore"], "cursor": page["nextCursor"]})
	case "/api/thread-action":
		s.handleDesktopAction(w, r)
	}
	return true
}

func normalizeUsage(data map[string]any) map[string]any {
	limits := asMap(asMap(data["rateLimitsByLimitId"])["codex"])
	if len(limits) == 0 {
		limits = asMap(data["rateLimits"])
	}
	windows := []any{}
	for _, item := range []struct {
		key      string
		fallback string
	}{{"primary", "主要窗口"}, {"secondary", "次要窗口"}} {
		window := asMap(limits[item.key])
		if len(window) == 0 {
			continue
		}
		duration := intFromAny(window["windowDurationMins"])
		used := clampPercent(intFromAny(window["usedPercent"]))
		row := map[string]any{
			"id": item.key, "label": usageWindowLabel(duration, item.fallback),
			"usedPercent": used, "remainingPercent": 100 - used,
			"windowDurationMins": duration,
		}
		if reset := intFromAny(window["resetsAt"]); reset > 0 {
			row["resetsAt"] = time.Unix(int64(reset), 0).Format(time.RFC3339)
		}
		windows = append(windows, row)
	}

	result := map[string]any{
		"ok": true, "available": len(windows) > 0, "windows": windows,
		"planType": asString(limits["planType"]),
	}
	credits := asMap(limits["credits"])
	if len(credits) > 0 {
		result["credits"] = map[string]any{
			"hasCredits": credits["hasCredits"] == true,
			"unlimited":  credits["unlimited"] == true,
			"balance":    asString(credits["balance"]),
		}
	}
	if available := intFromAny(asMap(data["rateLimitResetCredits"])["availableCount"]); available > 0 {
		result["resetCreditsAvailable"] = available
	}
	return result
}

func clampPercent(value int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func usageWindowLabel(minutes int, fallback string) string {
	switch minutes {
	case 300:
		return "5 小时"
	case 10080:
		return "7 天"
	}
	if minutes > 0 && minutes%(24*60) == 0 {
		return fmt.Sprintf("%d 天", minutes/(24*60))
	}
	if minutes > 0 && minutes%60 == 0 {
		return fmt.Sprintf("%d 小时", minutes/60)
	}
	if minutes > 0 {
		return fmt.Sprintf("%d 分钟", minutes)
	}
	return fallback
}

func desktopMessages(data map[string]any) ([]messageRow, []statusStep) {
	messages := []messageRow{}
	steps := []statusStep{}
	turns := asSlice(data["turns"])
	for i := len(turns) - 1; i >= 0; i-- {
		turn := asMap(turns[i])
		timestamp := time.Unix(int64(intFromAny(turn["startedAt"])), 0).Format(time.RFC3339)
		for _, item := range asSlice(turn["items"]) {
			x := asMap(item)
			typ := asString(x["type"])
			switch typ {
			case "userMessage":
				parts := []string{}
				for _, block := range asSlice(x["content"]) {
					b := asMap(block)
					if b["type"] == "text" {
						parts = append(parts, asString(b["text"]))
					}
				}
				messages = append(messages, messageRow{Seq: len(messages) + 1, Role: "user", Text: strings.Join(parts, "\n"), Timestamp: timestamp})
			case "agentMessage":
				if x["phase"] == "commentary" {
					text := strings.TrimSpace(asString(x["text"]))
					if text != "" {
						messages = append(messages, messageRow{Seq: len(messages) + 1, Role: "assistant", Text: text, Timestamp: timestamp})
						steps = append(steps, statusStep{ID: asString(x["id"]), Kind: "message", Label: "回复中", Detail: text})
					}
					continue
				}
				messages = append(messages, messageRow{Seq: len(messages) + 1, Role: "assistant", Text: asString(x["text"]), Timestamp: timestamp})
			case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall":
				label := map[string]string{"commandExecution": "执行命令", "fileChange": "修改文件", "mcpToolCall": "调用工具", "dynamicToolCall": "调用工具"}[typ]
				kind := "tool"
				if x["status"] == "failed" {
					kind = "error"
				}
				steps = append(steps, statusStep{ID: asString(x["id"]), Kind: kind, Label: label, Detail: asString(x["status"])})
			}
		}
	}
	if len(steps) > 10 {
		steps = steps[len(steps)-10:]
	}
	return messages, steps
}

func (s *serverState) handleDesktopAction(w http.ResponseWriter, r *http.Request) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	var body struct {
		ThreadID string `json:"threadId"`
		Action   string `json:"action"`
		Name     string `json:"name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	if json.NewDecoder(r.Body).Decode(&body) != nil || !validThreadID(body.ThreadID) {
		writeJSON(w, 400, errJSON("BAD_ACTION", "任务操作参数不正确"))
		return
	}
	tool := ""
	args := map[string]any{"threadId": body.ThreadID}
	switch body.Action {
	case "rename":
		body.Name = strings.TrimSpace(body.Name)
		if body.Name == "" || len([]rune(body.Name)) > 120 {
			writeJSON(w, 400, errJSON("BAD_NAME", "名称需为 1 到 120 个字"))
			return
		}
		tool = "set_thread_title"
		args["title"] = body.Name
	case "pin", "unpin":
		tool = "move_thread_to_sidebar_section"
		args["hostId"] = "local"
		args["sectionId"] = nil
		if body.Action == "pin" {
			args["sectionId"] = "pinned"
		}
	case "archive", "restore":
		tool = "set_thread_archived"
		args["hostId"] = "local"
		args["archived"] = body.Action == "archive"
	default:
		writeJSON(w, 400, errJSON("BAD_ACTION", "不支持的任务操作"))
		return
	}
	if _, err := s.desktopThread(r.Context(), body.ThreadID); err != nil {
		writeJSON(w, 400, errJSON("THREAD_UNAVAILABLE", err.Error()))
		return
	}
	_, err := s.desktopCall(r.Context(), tool, args, randomToken())
	if err != nil {
		writeJSON(w, 502, errJSON("ACTION_FAILED", err.Error()))
		return
	}
	// Read back the authoritative state instead of applying a local override.
	confirmed := false
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(150 * time.Millisecond)
		}
		if body.Action == "rename" {
			result, e := s.desktopThread(r.Context(), body.ThreadID)
			confirmed = e == nil && asMap(result["thread"])["title"] == body.Name
		} else {
			rows, _, e := s.desktopSnapshot(r.Context(), true)
			if e == nil {
				for _, row := range rows {
					if row.ID == body.ThreadID {
						switch body.Action {
						case "pin":
							confirmed = row.Pinned
						case "unpin":
							confirmed = !row.Pinned
						case "archive":
							confirmed = row.Archived
						case "restore":
							confirmed = !row.Archived
						}
					}
				}
			}
		}
		if confirmed {
			break
		}
	}
	if !confirmed {
		writeJSON(w, 202, map[string]any{"ok": true, "confirmed": false, "message": "桌面已接受操作，状态同步中"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "confirmed": true, "threadId": body.ThreadID, "nextThreadId": body.ThreadID})
}
