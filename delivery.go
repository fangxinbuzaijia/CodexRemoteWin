package main

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type uploadRecord struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	Size  int64  `json:"size"`
	Kind  string `json:"kind"`
	Owner string `json:"owner,omitempty"`
}

type deliveryRecord struct {
	ID             string         `json:"requestId"`
	Owner          string         `json:"owner,omitempty"`
	Fingerprint    string         `json:"fingerprint"`
	ThreadID       string         `json:"threadId"`
	ClientThreadID string         `json:"clientThreadId,omitempty"`
	Text           string         `json:"text"`
	Prompt         string         `json:"prompt"`
	State          string         `json:"state"`
	Error          string         `json:"error,omitempty"`
	Attachments    []uploadRecord `json:"attachments"`
	UpdatedAt      string         `json:"updatedAt"`
	LogOffset      int64          `json:"logOffset"`
}

type deliveryInput struct {
	RequestID     string   `json:"clientRequestId"`
	ThreadID      string   `json:"threadId"`
	Text          string   `json:"text"`
	AttachmentIDs []string `json:"attachmentIds"`
	ProjectID     string   `json:"projectId"`
	Environment   string   `json:"environment"`
}

func atomicJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".state-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (s *serverState) uploadPath(id string) string {
	return filepath.Join(s.dataDir, "attachments", id)
}
func (s *serverState) attachmentFile(item uploadRecord) string {
	return s.uploadPath(item.ID) + "-" + safeFileName(item.Name)
}
func validUploadID(id string) bool {
	if len(id) != 64 {
		return false
	}
	return strings.Trim(id, "0123456789abcdef") == ""
}
func (s *serverState) readUpload(id string) (uploadRecord, error) {
	var item uploadRecord
	if !validUploadID(id) {
		return item, errors.New("Invalid attachment id")
	}
	data, err := os.ReadFile(s.uploadPath(id) + ".json")
	if err != nil {
		return item, err
	}
	if err = json.Unmarshal(data, &item); err != nil {
		return item, err
	}
	return item, nil
}

func (s *serverState) handleUpload(w http.ResponseWriter, r *http.Request, sess session) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAttachmentBytes+1024*1024)
	reader, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, 400, errJSON("BAD_UPLOAD", "请选择要上传的文件"))
		return
	}
	part, err := reader.NextPart()
	if err != nil || part.FormName() != "file" || part.FileName() == "" {
		writeJSON(w, 400, errJSON("BAD_UPLOAD", "需要一个文件"))
		return
	}
	defer part.Close()
	name := safeFileName(part.FileName())
	ext := strings.ToLower(filepath.Ext(name))
	media := part.Header.Get("Content-Type")
	kind := attachmentKind(media, ext)
	if kind == "" {
		writeJSON(w, 400, errJSON("BAD_TYPE", "暂不支持此附件类型"))
		return
	}
	id := hash(randomToken())
	path := s.uploadPath(id) + "-" + name
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		writeJSON(w, 500, errJSON("UPLOAD_FAILED", err.Error()))
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		writeJSON(w, 500, errJSON("UPLOAD_FAILED", err.Error()))
		return
	}
	size, copyErr := io.Copy(file, io.LimitReader(part, maxAttachmentBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || size == 0 || size > maxAttachmentBytes {
		os.Remove(path)
		writeJSON(w, 413, errJSON("UPLOAD_TOO_LARGE", "单个文件需大于 0 字节且不超过 12 MB"))
		return
	}
	// Each request carries exactly one file, so limits match browser progress.
	if _, err = reader.NextPart(); err != io.EOF {
		os.Remove(path)
		writeJSON(w, 400, errJSON("BAD_UPLOAD", "每次请求只能上传一个文件"))
		return
	}
	item := uploadRecord{ID: id, Name: name, Type: media, Size: size, Kind: kind, Owner: sess.TokenHash}
	if err = atomicJSON(s.uploadPath(id)+".json", item); err != nil {
		os.Remove(path)
		writeJSON(w, 500, errJSON("UPLOAD_FAILED", err.Error()))
		return
	}
	item.Owner = ""
	writeJSON(w, 200, map[string]any{"ok": true, "attachment": item})
}

func (s *serverState) serveUpload(w http.ResponseWriter, r *http.Request) {
	item, err := s.readUpload(r.URL.Query().Get("id"))
	if err != nil {
		writeJSON(w, 404, errJSON("NOT_FOUND", "附件不存在"))
		return
	}
	file, err := os.Open(s.attachmentFile(item))
	if err != nil {
		writeJSON(w, 404, errJSON("NOT_FOUND", "附件文件不存在"))
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeJSON(w, 500, errJSON("READ_FAILED", err.Error()))
		return
	}
	disposition := "attachment"
	contentType := "application/octet-stream"
	if r.URL.Query().Get("preview") == "1" && item.Kind == "image" {
		header := make([]byte, 512)
		n, _ := file.Read(header)
		file.Seek(0, io.SeekStart)
		detected := http.DetectContentType(header[:n])
		if detected == "image/png" || detected == "image/jpeg" || detected == "image/gif" || detected == "image/webp" {
			disposition = "inline"
			contentType = detected
		}
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": item.Name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, item.Name, info.ModTime(), file)
}

func (s *serverState) deliveryPath(sess session, id string) string {
	return filepath.Join(s.dataDir, "deliveries", hash(sess.TokenHash+":"+id)+".json")
}
func (s *serverState) readDelivery(sess session, id string) (deliveryRecord, error) {
	var record deliveryRecord
	data, err := os.ReadFile(s.deliveryPath(sess, id))
	if err != nil {
		return record, err
	}
	err = json.Unmarshal(data, &record)
	return record, err
}

func (s *serverState) handleDelivery(w http.ResponseWriter, r *http.Request, sess session, newThread bool) {
	// Serialize mutations while receipts are committed. An in-flight record survives
	// process restarts; an uncertain send is never replayed automatically.
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	var input deliveryInput
	r.Body = http.MaxBytesReader(w, r.Body, 128*1024)
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeJSON(w, 400, errJSON("BAD_JSON", "消息格式不正确"))
		return
	}
	input.Text = strings.TrimSpace(input.Text)
	if input.RequestID == "" || len(input.RequestID) > 100 {
		writeJSON(w, 400, errJSON("BAD_REQUEST_ID", "缺少有效消息编号"))
		return
	}
	if len([]rune(input.Text)) > maxTextLength || len(input.AttachmentIDs) > maxAttachments || (input.Text == "" && len(input.AttachmentIDs) == 0) {
		writeJSON(w, 400, errJSON("BAD_MESSAGE", "消息不能为空，正文最多 8000 字，附件最多 6 个"))
		return
	}
	if !newThread && !validThreadID(input.ThreadID) {
		writeJSON(w, 400, errJSON("BAD_THREAD_ID", "请先选择一个任务"))
		return
	}
	encoded, _ := json.Marshal(struct {
		Input deliveryInput
		New   bool
	}{input, newThread})
	fingerprint := hash(string(encoded))
	previous, err := s.readDelivery(sess, input.RequestID)
	if err == nil {
		if previous.Fingerprint != fingerprint {
			writeJSON(w, 409, errJSON("REQUEST_CONFLICT", "消息编号已被其他内容使用"))
			return
		}
		writeJSON(w, 200, map[string]any{"ok": previous.State == "accepted", "receipt": previous, "message": previous.Error})
		return
	}
	if !os.IsNotExist(err) {
		writeJSON(w, 500, errJSON("RECEIPT_FAILED", "无法读取回执，请勿重复发送"))
		return
	}
	attachments := []uploadRecord{}
	paths := []map[string]string{}
	seen := map[string]bool{}
	for _, id := range input.AttachmentIDs {
		if seen[id] {
			writeJSON(w, 400, errJSON("BAD_ATTACHMENT", "附件重复"))
			return
		}
		seen[id] = true
		item, e := s.readUpload(id)
		if e != nil || item.Owner != sess.TokenHash {
			writeJSON(w, 400, errJSON("BAD_ATTACHMENT", "附件不存在或不属于此设备，请重新上传"))
			return
		}
		// Keep the extension on the local reference for file-reading tools.
		path := s.attachmentFile(item)
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != item.Size {
			writeJSON(w, 400, errJSON("BAD_ATTACHMENT", "附件文件已丢失或改变，请重新上传"))
			return
		}
		item.Owner = ""
		attachments = append(attachments, item)
		paths = append(paths, map[string]string{"name": item.Name, "path": path, "type": item.Type})
	}
	prompt := input.Text
	if len(paths) > 0 {
		manifest, _ := json.MarshalIndent(paths, "", "  ")
		prompt += "\n\n[Codex Remote attachments]\n以下附件已保存在本机。请根据需要读取文件，图片请使用图片读取工具查看。文件清单：\n```json\n" + string(manifest) + "\n```"
	}
	args := map[string]any{"threadId": input.ThreadID, "hostId": "local", "prompt": prompt}
	tool := "send_message_to_thread"
	if newThread {
		tool = "create_thread"
		target := map[string]any{"type": "projectless"}
		if input.ProjectID != "" {
			catalog, e := s.desktopCall(r.Context(), "list_projects", map[string]any{}, "")
			if e != nil {
				writeJSON(w, 503, errJSON("DESKTOP_UNAVAILABLE", e.Error()))
				return
			}
			var project map[string]any
			for _, p := range asSlice(catalog["projects"]) {
				x := asMap(p)
				if x["projectId"] == input.ProjectID && x["hostId"] == "local" {
					project = x
					break
				}
			}
			if project == nil {
				writeJSON(w, 400, errJSON("BAD_PROJECT", "此项目不在当前电脑上"))
				return
			}
			environment := input.Environment
			if environment == "" {
				environment = "local"
				if project["isGitRepository"] == true {
					environment = "worktree"
				}
			}
			if (environment != "local" && environment != "worktree") || (environment == "worktree" && project["isGitRepository"] != true) {
				writeJSON(w, 400, errJSON("BAD_ENVIRONMENT", "项目不支持所选环境"))
				return
			}
			target = map[string]any{"type": "project", "projectId": input.ProjectID, "environment": map[string]any{"type": environment}}
		}
		args = map[string]any{"target": target, "prompt": prompt}
	} else {
		result, e := s.desktopThread(r.Context(), input.ThreadID)
		if e != nil || asString(asMap(result["thread"])["id"]) != input.ThreadID {
			writeJSON(w, 400, errJSON("THREAD_UNAVAILABLE", "桌面端无法确认此任务，请刷新列表"))
			return
		}
	}
	offset := int64(-1)
	if !newThread {
		if file := s.fileForThread(input.ThreadID); file != "" {
			offset = fileSize(file)
		}
	}
	record := deliveryRecord{ID: input.RequestID, Owner: sess.TokenHash, Fingerprint: fingerprint, ThreadID: input.ThreadID, Text: input.Text, Prompt: prompt, State: "unknown", Error: "等待桌面回执，请勿重复发送", Attachments: attachments, UpdatedAt: time.Now().Format(time.RFC3339), LogOffset: offset}
	path := s.deliveryPath(sess, input.RequestID)
	if err = atomicJSON(path, record); err != nil {
		writeJSON(w, 500, errJSON("RECEIPT_FAILED", "无法保存发送记录"))
		return
	}
	result, err := s.desktopCall(r.Context(), tool, args, input.RequestID)
	if err != nil {
		record.Error = err.Error()
		var de *desktopError
		if !errors.As(err, &de) || !de.Uncertain {
			record.State = "failed"
		}
	} else {
		record.State = "accepted"
		record.Error = ""
		if newThread {
			record.ThreadID = asString(result["threadId"])
			record.ClientThreadID = asString(result["clientThreadId"])
			if record.ThreadID == "" && record.ClientThreadID == "" {
				record.State = "unknown"
				record.Error = "桌面已响应但没有返回任务编号，请检查桌面新任务"
			}
		}
	}
	record.UpdatedAt = time.Now().Format(time.RFC3339)
	if saveErr := atomicJSON(path, record); saveErr != nil {
		writeJSON(w, 500, errJSON("RECEIPT_FAILED", "操作可能已成功，但回执保存失败，请勿重发"))
		return
	}
	s.auditEvent("desktop_delivery", map[string]any{"requestId": record.ID, "threadId": record.ThreadID, "state": record.State})
	writeJSON(w, 200, map[string]any{"ok": record.State == "accepted", "receipt": record, "message": record.Error})
}

func (s *serverState) restoreAttachmentMessages(threadID, owner string, messages []messageRow) []messageRow {
	records := s.deliveryRecords(threadID, owner)
	byPrompt := map[string]deliveryRecord{}
	for _, record := range records {
		if len(record.Attachments) > 0 {
			byPrompt[strings.TrimSpace(record.Prompt)] = record
		}
	}
	for i := range messages {
		if messages[i].Role != "user" {
			continue
		}
		if record, ok := byPrompt[strings.TrimSpace(messages[i].Text)]; ok {
			messages[i].Text = record.Text
			messages[i].Attachments = record.Attachments
		}
	}
	return messages
}

func (s *serverState) deliveryRecords(threadID, owner string) []deliveryRecord {
	entries, _ := os.ReadDir(filepath.Join(s.dataDir, "deliveries"))
	records := []deliveryRecord{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dataDir, "deliveries", entry.Name()))
		if err != nil {
			continue
		}
		var record deliveryRecord
		if json.Unmarshal(data, &record) == nil && record.ThreadID == threadID && (record.Owner == "" || record.Owner == owner) {
			records = append(records, record)
		}
	}
	return records
}

func (s *serverState) reconcileDelivery(sess session, record deliveryRecord) deliveryRecord {
	if record.State != "unknown" || record.LogOffset < 0 || !validThreadID(record.ThreadID) {
		return record
	}
	file := s.fileForThread(record.ThreadID)
	if file != "" && waitForUserMessage(file, record.Prompt, record.LogOffset, 100*time.Millisecond) {
		record.State = "accepted"
		record.Error = ""
		record.UpdatedAt = time.Now().Format(time.RFC3339)
		if atomicJSON(s.deliveryPath(sess, record.ID), record) != nil {
			record.State = "unknown"
			record.Error = "已找到消息，但回执保存失败"
		}
	}
	return record
}
