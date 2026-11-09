package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cc-webui/internal/boarddb"
)

// AgentDeck artifact hosting.
//
// A board task may own many artifacts. Hosted artifacts (html / markdown /
// file) are uploaded by whoever produced them — a local main agent or a worker
// on another machine — and stored under
// <data_dir>/artifacts/<task>/<artifact>/<safe-filename>. The bytes are served
// back by GET /api/agent-board/artifacts/{id}; HTML is additionally served with
// a CSP sandbox so it can never reach the AgentDeck origin's cookies/DOM even
// when opened top-level.
//
// URL policy: hosted artifact URLs are minted from config.public_base_url when
// set (required for remote workers/viewers), otherwise from the request's
// scheme/host, and finally from the listen address for in-process MCP calls.
// The gateway token is never baked into an artifact record: viewers use the
// normal login cookie, or append ?token= at view time.

const (
	// maxArtifactBytes caps one uploaded artifact (HTML is expected to be well
	// under this; the limit also bounds multipart/raw uploads).
	maxArtifactBytes = 10 << 20
	// maxArtifactBodyBytes leaves room for JSON/base64 expansion of a max-size
	// artifact. It is also the loopback MCP request cap.
	maxArtifactBodyBytes = 16 << 20
	// htmlArtifactCSP turns every HTML artifact into an opaque-origin sandbox
	// that may run its own inline scripts (self-contained pages) but cannot
	// read AgentDeck cookies/localStorage or reach same-origin APIs.
	htmlArtifactCSP = "sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads; " +
		"default-src 'unsafe-inline' data: blob: https: http:;"
)

type artifactUpload struct {
	Title, Kind, Filename, MIME, Encoding, URL, SourceSession string
	Content                                                   []byte
}

type artifactHTTPError struct {
	status  int
	message string
}

func (e *artifactHTTPError) Error() string { return e.message }

func artifactErr(status int, format string, args ...any) *artifactHTTPError {
	return &artifactHTTPError{status: status, message: fmt.Sprintf(format, args...)}
}

func (s *Server) artifactsRoot() string {
	return filepath.Join(s.cfg().DataDir, "artifacts")
}

// artifactPublicBase returns the base URL used to mint artifact links.
func (s *Server) artifactPublicBase(r *http.Request) string {
	if base := strings.TrimSpace(s.cfg().PublicBaseURL); base != "" {
		return strings.TrimRight(base, "/")
	}
	if r != nil && strings.TrimSpace(r.Host) != "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); proto == "http" || proto == "https" {
			scheme = proto
		}
		return scheme + "://" + r.Host
	}
	// In-process MCP calls have no client host. Fall back to the listen
	// address, normalizing wildcard binds to loopback so the link is usable.
	host, port, err := net.SplitHostPort(strings.TrimSpace(s.cfg().Listen))
	if err != nil {
		return "http://127.0.0.1:8321"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// decorateArtifact mints the public URL for a hosted artifact (external URLs
// and legacy report_file entries keep their stored/no URL).
func (s *Server) decorateArtifact(r *http.Request, artifact map[string]any) {
	if artifact == nil {
		return
	}
	if legacy, _ := artifact["legacy"].(bool); legacy {
		return
	}
	if kind, _ := artifact["kind"].(string); kind == "url" {
		return
	}
	id, _ := artifact["id"].(string)
	if id == "" {
		return
	}
	artifact["url"] = s.artifactPublicBase(r) + "/api/agent-board/artifacts/" + url.PathEscape(id)
}

func (s *Server) decorateTaskArtifacts(r *http.Request, task map[string]any) {
	list, ok := task["artifacts"].([]map[string]any)
	if !ok {
		return
	}
	for _, artifact := range list {
		s.decorateArtifact(r, artifact)
	}
}

func (s *Server) decorateBoardSnapshot(r *http.Request, snapshot map[string]any) {
	tasks, ok := snapshot["tasks"].([]any)
	if !ok {
		return
	}
	for _, item := range tasks {
		if task, ok := item.(map[string]any); ok {
			s.decorateTaskArtifacts(r, task)
		}
	}
}

func (s *Server) handleAgentBoardCreateArtifact(w http.ResponseWriter, r *http.Request) {
	st, ok := s.boardDBStore(w)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxArtifactBodyBytes)
	up, err := parseArtifactUpload(r)
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	artifact, status, err := s.publishArtifact(st, r, r.PathValue("id"), up)
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}
	s.broadcastAgentBoard()
	writeJSON(w, status, map[string]any{"ok": true, "task_id": r.PathValue("id"), "artifact": artifact})
}

func (s *Server) handleAgentBoardTaskArtifacts(w http.ResponseWriter, r *http.Request) {
	st, ok := s.boardDBStore(w)
	if !ok {
		return
	}
	task, err := st.GetTask(r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "任务不存在: "+r.PathValue("id"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取任务失败: "+err.Error())
		return
	}
	s.decorateTaskArtifacts(r, task)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "task_id": r.PathValue("id"), "artifacts": task["artifacts"],
	})
}

func (s *Server) handleAgentBoardArtifact(w http.ResponseWriter, r *http.Request) {
	st, ok := s.boardDBStore(w)
	if !ok {
		return
	}
	artifact, err := st.GetArtifact(r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "产物不存在: "+r.PathValue("id"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取产物失败: "+err.Error())
		return
	}
	if artifact.Kind == "url" {
		if strings.TrimSpace(artifact.URL) == "" {
			writeErr(w, http.StatusNotFound, "产物链接为空")
			return
		}
		http.Redirect(w, r, artifact.URL, http.StatusFound)
		return
	}
	root := s.artifactsRoot()
	abs := filepath.Join(root, artifact.StoragePath)
	if strings.TrimSpace(artifact.StoragePath) == "" || !pathWithin(root, abs) {
		writeErr(w, http.StatusNotFound, "产物文件不存在")
		return
	}
	file, err := os.Open(abs)
	if err != nil {
		writeErr(w, http.StatusNotFound, "产物文件不存在")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		writeErr(w, http.StatusNotFound, "产物文件不存在")
		return
	}
	contentType := artifact.MIME
	if contentType == "" {
		contentType = artifactMIME(artifact.Kind, artifact.Filename, "")
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	// Apply the sandbox to every artifact, not just HTML: a "file" artifact can
	// be an SVG/XHTML (or a lied-about MIME) that a browser would otherwise
	// render as an active document on the AgentDeck origin. CSP sandbox on a
	// non-document response is harmless.
	w.Header().Set("Content-Security-Policy", htmlArtifactCSP)
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment",
			map[string]string{"filename": artifact.Filename}))
		w.Header().Set("Cache-Control", "private, no-store")
	}
	http.ServeContent(w, r, artifact.Filename, info.ModTime(), file)
}

// publishArtifact is the single validation/storage path shared by the HTTP
// endpoint and the artifact_publish MCP tool.
func (s *Server) publishArtifact(st *boarddb.Store, r *http.Request, taskID string, up artifactUpload) (map[string]any, int, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("task_id required")
	}
	if _, err := st.GetTask(taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, http.StatusNotFound, fmt.Errorf("任务不存在: %s", taskID)
		}
		return nil, http.StatusInternalServerError, fmt.Errorf("读取任务失败: %v", err)
	}
	kind, err := normalizeArtifactKind(&up)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	filename := sanitizeArtifactFilename(up.Filename, kind)
	title := clipArtifactRunes(strings.TrimSpace(up.Title), 200)
	if title == "" {
		title = filename
	}
	mimeType := artifactMIME(kind, filename, up.MIME)
	sourceSession := strings.TrimSpace(up.SourceSession)

	if kind == "url" {
		target := strings.TrimSpace(up.URL)
		parsed, err := url.Parse(target)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return nil, http.StatusBadRequest, fmt.Errorf("kind=url 需要合法的 http(s) URL")
		}
		record, err := st.CreateArtifact(map[string]any{
			"task_id": taskID, "title": title, "kind": kind, "mime": "",
			"filename": filename, "size": int64(0), "url": target,
			"source_session": sourceSession,
		})
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		s.decorateArtifact(r, record)
		return record, http.StatusCreated, nil
	}

	if len(up.Content) == 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("产物内容为空")
	}
	if len(up.Content) > maxArtifactBytes {
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("产物超过 %d MiB 上限", maxArtifactBytes>>20)
	}
	id := newArtifactID()
	rel := filepath.Join(safeArtifactPathSegment(taskID), id, filename)
	root := s.artifactsRoot()
	abs := filepath.Join(root, rel)
	if !pathWithin(root, abs) {
		return nil, http.StatusBadRequest, fmt.Errorf("非法产物路径")
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("创建产物目录失败: %v", err)
	}
	if err := os.WriteFile(abs, up.Content, 0o644); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("写入产物失败: %v", err)
	}
	record, err := st.CreateArtifact(map[string]any{
		"id": id, "task_id": taskID, "title": title, "kind": kind, "mime": mimeType,
		"filename": filename, "size": len(up.Content), "storage_path": rel,
		"source_session": sourceSession,
	})
	if err != nil {
		_ = os.RemoveAll(filepath.Dir(abs))
		return nil, http.StatusBadRequest, err
	}
	s.decorateArtifact(r, record)
	return record, http.StatusCreated, nil
}

func (s *Server) agentdeckPublishArtifact(sessionID string, args map[string]any) (string, error) {
	if s.boardDB == nil {
		return "", fmt.Errorf("看板数据库未初始化")
	}
	taskID, _ := args["task_id"].(string)
	if strings.TrimSpace(taskID) == "" {
		taskID, _ = args["id"].(string)
	}
	up := artifactUpload{
		Title:         stringArg(args, "title"),
		Kind:          stringArg(args, "kind"),
		Filename:      stringArg(args, "filename"),
		MIME:          stringArg(args, "mime"),
		Encoding:      stringArg(args, "encoding"),
		URL:           stringArg(args, "url"),
		SourceSession: stringArg(args, "source_session"),
	}
	if strings.TrimSpace(up.SourceSession) == "" {
		up.SourceSession = sessionID
	}
	if err := decodeArtifactContent(&up, stringArg(args, "content"), up.Encoding); err != nil {
		return "", err
	}
	artifact, _, err := s.publishArtifact(s.boardDB, nil, taskID, up)
	if err != nil {
		return "", err
	}
	s.broadcastAgentBoard()
	raw, err := json.MarshalIndent(map[string]any{
		"ok": true, "task_id": strings.TrimSpace(taskID), "artifact": artifact,
	}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func parseArtifactUpload(r *http.Request) (artifactUpload, error) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch {
	case mediaType == "application/json":
		return parseArtifactJSON(r)
	case mediaType == "multipart/form-data":
		return parseArtifactMultipart(r)
	default:
		return parseArtifactRaw(r, mediaType)
	}
}

func parseArtifactJSON(r *http.Request) (artifactUpload, error) {
	var body struct {
		Title         string `json:"title"`
		Kind          string `json:"kind"`
		Filename      string `json:"filename"`
		MIME          string `json:"mime"`
		Content       string `json:"content"`
		Encoding      string `json:"encoding"`
		URL           string `json:"url"`
		SourceSession string `json:"source_session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var max *http.MaxBytesError
		if errors.As(err, &max) {
			return artifactUpload{}, artifactErr(http.StatusRequestEntityTooLarge,
				"请求体超过 %d MiB 上限", maxArtifactBodyBytes>>20)
		}
		return artifactUpload{}, artifactErr(http.StatusBadRequest, "请求体不是合法 JSON")
	}
	up := artifactUpload{
		Title: body.Title, Kind: body.Kind, Filename: body.Filename, MIME: body.MIME,
		Encoding: body.Encoding, URL: body.URL, SourceSession: body.SourceSession,
	}
	if err := decodeArtifactContent(&up, body.Content, body.Encoding); err != nil {
		return artifactUpload{}, err
	}
	return up, nil
}

func parseArtifactRaw(r *http.Request, mediaType string) (artifactUpload, error) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var max *http.MaxBytesError
		if errors.As(err, &max) {
			return artifactUpload{}, artifactErr(http.StatusRequestEntityTooLarge,
				"产物超过 %d MiB 上限", maxArtifactBytes>>20)
		}
		return artifactUpload{}, artifactErr(http.StatusBadRequest, "读取请求体失败")
	}
	if len(data) == 0 {
		return artifactUpload{}, artifactErr(http.StatusBadRequest, "产物内容为空")
	}
	q := r.URL.Query()
	// curl -d defaults to form-urlencoded and --data-binary often labels raw
	// HTML as octet-stream; neither is a real MIME hint, so infer from the
	// filename/kind instead.
	if mediaType == "application/x-www-form-urlencoded" || mediaType == "application/octet-stream" {
		mediaType = ""
	}
	return artifactUpload{
		Title: q.Get("title"), Kind: q.Get("kind"), Filename: q.Get("filename"),
		MIME: firstNonEmpty(q.Get("mime"), mediaType), URL: q.Get("url"),
		SourceSession: q.Get("source_session"), Content: data,
	}, nil
}

func parseArtifactMultipart(r *http.Request) (artifactUpload, error) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return artifactUpload{}, artifactErr(http.StatusBadRequest, "multipart 请求体不合法")
	}
	up := artifactUpload{
		Title: r.FormValue("title"), Kind: r.FormValue("kind"), Filename: r.FormValue("filename"),
		MIME: r.FormValue("mime"), Encoding: r.FormValue("encoding"), URL: r.FormValue("url"),
		SourceSession: r.FormValue("source_session"),
	}
	file, header, err := r.FormFile("file")
	if errors.Is(err, http.ErrMissingFile) {
		if err := decodeArtifactContent(&up, r.FormValue("content"), up.Encoding); err != nil {
			return artifactUpload{}, err
		}
		if len(up.Content) == 0 && strings.TrimSpace(up.URL) == "" {
			return artifactUpload{}, artifactErr(http.StatusBadRequest, "multipart 需要 file 字段或 content 字段")
		}
		return up, nil
	}
	if err != nil {
		return artifactUpload{}, artifactErr(http.StatusBadRequest, "读取上传文件失败")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxArtifactBytes+1))
	if err != nil {
		return artifactUpload{}, artifactErr(http.StatusBadRequest, "读取上传文件失败")
	}
	if len(data) > maxArtifactBytes {
		return artifactUpload{}, artifactErr(http.StatusRequestEntityTooLarge,
			"产物超过 %d MiB 上限", maxArtifactBytes>>20)
	}
	if len(data) == 0 {
		return artifactUpload{}, artifactErr(http.StatusBadRequest, "产物内容为空")
	}
	if up.Filename == "" && header != nil {
		up.Filename = header.Filename
	}
	if up.MIME == "" && header != nil {
		up.MIME = header.Header.Get("Content-Type")
	}
	up.Content = data
	return up, nil
}

func decodeArtifactContent(up *artifactUpload, content, encoding string) error {
	if content == "" {
		return nil
	}
	// Accept a browser-style data: URL as a convenience for pasted content.
	if strings.HasPrefix(content, "data:") {
		if idx := strings.Index(content, ";base64,"); idx >= 0 {
			content = content[idx+len(";base64,"):]
			encoding = "base64"
		}
	}
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "utf8", "utf-8", "text":
		up.Content = []byte(content)
	case "base64":
		data, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return artifactErr(http.StatusBadRequest, "content 不是合法 base64")
		}
		up.Content = data
	default:
		return artifactErr(http.StatusBadRequest, "不支持的 encoding: %s（只用 utf8 或 base64）", encoding)
	}
	return nil
}

func normalizeArtifactKind(up *artifactUpload) (string, error) {
	kind := strings.ToLower(strings.TrimSpace(up.Kind))
	switch kind {
	case "html", "markdown", "file", "url":
	case "md":
		kind = "markdown"
	case "":
		kind = inferArtifactKind(up.Filename, up.MIME)
	default:
		return "", artifactErr(http.StatusBadRequest,
			"不支持的产物类型: %s（html / markdown / file / url）", up.Kind)
	}
	return kind, nil
}

func inferArtifactKind(filename, mimeType string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".html", ".htm":
		return "html"
	case ".md", ".markdown":
		return "markdown"
	}
	lower := strings.ToLower(mimeType)
	switch {
	case strings.Contains(lower, "html"):
		return "html"
	case strings.Contains(lower, "markdown"):
		return "markdown"
	}
	return "file"
}

func defaultArtifactFilename(kind string) string {
	switch kind {
	case "html":
		return "index.html"
	case "markdown":
		return "artifact.md"
	case "url":
		return "link"
	default:
		return "artifact.bin"
	}
}

// sanitizeArtifactFilename reduces a caller-supplied name to one safe path
// component: basename only, no separators/control characters, bounded length.
func sanitizeArtifactFilename(name, kind string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`/\:*?"<>|`, r) {
			b.WriteRune('_')
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if out == "" || out == "." || out == ".." {
		return defaultArtifactFilename(kind)
	}
	runes := []rune(out)
	if len(runes) > 120 {
		ext := filepath.Ext(out)
		base := []rune(strings.TrimSuffix(out, ext))
		max := 120 - len([]rune(ext))
		if max < 1 {
			max = 1
		}
		if len(base) > max {
			base = base[:max]
		}
		out = string(base) + ext
	}
	if out == "" || out == "." || out == ".." {
		return defaultArtifactFilename(kind)
	}
	return out
}

func safeArtifactPathSegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "_"
	}
	var b strings.Builder
	for _, r := range s {
		if r == '/' || r == '\\' || r == ':' || r < 0x20 || r == 0x7f {
			b.WriteRune('_')
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if runes := []rune(out); len(runes) > 80 {
		out = string(runes[:80])
	}
	if out == "." || out == ".." {
		out = "_"
	}
	return out
}

func pathWithin(root, path string) bool {
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return false
	}
	return absPath == absRoot || strings.HasPrefix(absPath, absRoot+string(os.PathSeparator))
}

func artifactMIME(kind, filename, provided string) string {
	// application/octet-stream is the multipart default, not a real hint.
	if value := strings.TrimSpace(provided); value != "" && !strings.EqualFold(value, "application/octet-stream") {
		return value
	}
	switch kind {
	case "html":
		return "text/html; charset=utf-8"
	case "markdown":
		return "text/markdown; charset=utf-8"
	case "url":
		return ""
	}
	if value := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename))); value != "" {
		return value
	}
	return "application/octet-stream"
}

func newArtifactID() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("A-%d", time.Now().UnixNano())
	}
	return "A-" + time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(buf)
}

func clipArtifactRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func writeArtifactError(w http.ResponseWriter, err error) {
	var httpErr *artifactHTTPError
	if errors.As(err, &httpErr) {
		writeErr(w, httpErr.status, httpErr.message)
		return
	}
	writeErr(w, http.StatusBadRequest, err.Error())
}
