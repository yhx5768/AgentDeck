package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func createArtifactTestTask(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.boardDB.CreateTask(map[string]any{"id": "T-1", "title": "产物托管"}); err != nil {
		t.Fatal(err)
	}
}

func artifactReq(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	return req
}

func TestAgentBoardArtifactPublishJSONAndServe(t *testing.T) {
	s, _ := boardMCPServer(t)
	createArtifactTestTask(t, s)

	payload := map[string]any{
		"title": "验收报告", "kind": "html", "filename": "report.html",
		"content": "<!doctype html><html><body><h1>ok</h1><script>document.title='x'</script></body></html>",
	}
	raw, _ := json.Marshal(payload)
	req := artifactReq(http.MethodPost, "http://deck.local/api/agent-board/tasks/T-1/artifacts", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "T-1")
	rec := httptest.NewRecorder()
	s.handleAgentBoardCreateArtifact(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish status = %d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		OK       bool           `json:"ok"`
		Artifact map[string]any `json:"artifact"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	artifact := created.Artifact
	if artifact["kind"] != "html" || artifact["filename"] != "report.html" || artifact["title"] != "验收报告" {
		t.Fatalf("artifact = %+v", artifact)
	}
	url, _ := artifact["url"].(string)
	if !strings.HasPrefix(url, "http://deck.local/api/agent-board/artifacts/") {
		t.Fatalf("url = %q", url)
	}
	id, _ := artifact["id"].(string)
	storage, _ := artifact["storage_path"].(string)
	if !strings.HasPrefix(storage, filepath.Join("T-1", id)) {
		t.Fatalf("storage_path = %q", storage)
	}
	if _, err := os.Stat(filepath.Join(s.artifactsRoot(), storage)); err != nil {
		t.Fatalf("artifact file: %v", err)
	}

	// GET task returns artifacts[] with the minted URL.
	req = artifactReq(http.MethodGet, "http://deck.local/api/agent-board/tasks/T-1", nil)
	req.SetPathValue("id", "T-1")
	rec = httptest.NewRecorder()
	s.handleAgentBoardGetTask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get task status = %d", rec.Code)
	}
	var got struct {
		Task map[string]any `json:"task"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	list := got.Task["artifacts"].([]any)
	if len(list) != 1 {
		t.Fatalf("task artifacts = %+v", list)
	}
	if list[0].(map[string]any)["url"] != url {
		t.Fatalf("task artifact url = %+v", list[0])
	}

	// Serve the artifact: HTML content type, nosniff, CSP sandbox.
	req = artifactReq(http.MethodGet, "http://deck.local/api/agent-board/artifacts/"+id, nil)
	req.SetPathValue("id", id)
	rec = httptest.NewRecorder()
	s.handleAgentBoardArtifact(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("serve status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox") || strings.Contains(csp, "allow-same-origin") {
		t.Fatalf("csp = %q", csp)
	}
	if !strings.Contains(rec.Body.String(), "<h1>ok</h1>") {
		t.Fatalf("body = %s", rec.Body.String())
	}

	// download=1 forces an attachment.
	req = artifactReq(http.MethodGet, "http://deck.local/api/agent-board/artifacts/"+id+"?download=1", nil)
	req.SetPathValue("id", id)
	rec = httptest.NewRecorder()
	s.handleAgentBoardArtifact(rec, req)
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("content-disposition = %q", cd)
	}
}

func TestAgentBoardArtifactRawAndMultipartUpload(t *testing.T) {
	s, _ := boardMCPServer(t)
	createArtifactTestTask(t, s)

	// Raw body + query metadata (the remote-worker curl path).
	body := "<!doctype html><title>raw</title><p>hello</p>"
	req := artifactReq(http.MethodPost,
		"http://deck.local/api/agent-board/tasks/T-1/artifacts?title=Raw+Page&kind=html&filename=raw.html",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "text/html")
	req.SetPathValue("id", "T-1")
	rec := httptest.NewRecorder()
	s.handleAgentBoardCreateArtifact(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("raw publish = %d %s", rec.Code, rec.Body.String())
	}

	// Multipart file upload.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("title", "Markdown 报告")
	_ = mw.WriteField("kind", "markdown")
	_ = mw.WriteField("filename", "report.md")
	part, _ := mw.CreateFormFile("file", "report.md")
	_, _ = part.Write([]byte("# hi\n\n详情"))
	mw.Close()
	req = artifactReq(http.MethodPost, "http://deck.local/api/agent-board/tasks/T-1/artifacts", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetPathValue("id", "T-1")
	rec = httptest.NewRecorder()
	s.handleAgentBoardCreateArtifact(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("multipart publish = %d %s", rec.Code, rec.Body.String())
	}

	task, err := s.boardDB.GetTask("T-1")
	if err != nil {
		t.Fatal(err)
	}
	list := task["artifacts"].([]map[string]any)
	if len(list) != 2 {
		t.Fatalf("artifacts = %+v", list)
	}
	if list[1]["kind"] != "markdown" || list[1]["mime"] != "text/markdown; charset=utf-8" {
		t.Fatalf("markdown artifact = %+v", list[1])
	}
	// The sandbox CSP is universal, not HTML-only: SVG/XHTML published as a
	// generic "file" must not become an active same-origin document either.
	mdID := list[1]["id"].(string)
	req = artifactReq(http.MethodGet, "http://deck.local/api/agent-board/artifacts/"+mdID, nil)
	req.SetPathValue("id", mdID)
	rec = httptest.NewRecorder()
	s.handleAgentBoardArtifact(rec, req)
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Fatalf("non-html artifact missing sandbox CSP: %q", csp)
	}
}

func TestAgentBoardArtifactSecurityBoundaries(t *testing.T) {
	s, _ := boardMCPServer(t)
	createArtifactTestTask(t, s)

	// Filename traversal is reduced to a basename.
	payload, _ := json.Marshal(map[string]any{
		"kind": "html", "filename": "../../etc/evil.html", "content": "x",
	})
	req := artifactReq(http.MethodPost, "http://deck.local/api/agent-board/tasks/T-1/artifacts", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "T-1")
	rec := httptest.NewRecorder()
	s.handleAgentBoardCreateArtifact(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Artifact map[string]any `json:"artifact"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	storage := created.Artifact["storage_path"].(string)
	if strings.Contains(storage, "..") || strings.Contains(storage, "/etc/") {
		t.Fatalf("storage escaped: %q", storage)
	}
	if !strings.HasSuffix(storage, "evil.html") {
		t.Fatalf("filename = %q", storage)
	}
	if !pathWithin(s.artifactsRoot(), filepath.Join(s.artifactsRoot(), storage)) {
		t.Fatalf("file outside artifacts root: %q", storage)
	}

	// A tampered storage_path in the DB is rejected by the serve handler.
	id := created.Artifact["id"].(string)
	if _, err := s.boardDB.GetArtifact(id); err != nil {
		t.Fatal(err)
	}
	// Write a malicious record directly through the public API surface is not
	// possible; simulate a corrupted row by inserting a storage_path that
	// escapes the root.
	if _, err := s.boardDB.CreateArtifact(map[string]any{
		"id": "A-evil", "task_id": "T-1", "kind": "file", "filename": "passwd",
		"storage_path": "../../etc/passwd", "size": 1,
	}); err != nil {
		t.Fatal(err)
	}
	req = artifactReq(http.MethodGet, "http://deck.local/api/agent-board/artifacts/A-evil", nil)
	req.SetPathValue("id", "A-evil")
	rec = httptest.NewRecorder()
	s.handleAgentBoardArtifact(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("escaped storage must 404, got %d", rec.Code)
	}

	// Oversized raw upload is rejected.
	big := bytes.Repeat([]byte("a"), maxArtifactBytes+1)
	req = artifactReq(http.MethodPost, "http://deck.local/api/agent-board/tasks/T-1/artifacts?kind=file&filename=big.bin", bytes.NewReader(big))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.SetPathValue("id", "T-1")
	rec = httptest.NewRecorder()
	s.handleAgentBoardCreateArtifact(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d %s", rec.Code, rec.Body.String())
	}

	// Unknown task is a 404.
	req = artifactReq(http.MethodPost, "http://deck.local/api/agent-board/tasks/NOPE/artifacts", strings.NewReader("x"))
	req.Header.Set("Content-Type", "text/plain")
	req.SetPathValue("id", "NOPE")
	rec = httptest.NewRecorder()
	s.handleAgentBoardCreateArtifact(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task status = %d", rec.Code)
	}
}

func TestAgentBoardArtifactPublicBaseURLAndURLKind(t *testing.T) {
	s, _ := boardMCPServer(t)
	createArtifactTestTask(t, s)
	s.cm.Get().PublicBaseURL = "https://deck.example.com/"

	publishHTML := func(target string) map[string]any {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"kind": "html", "filename": "x.html", "content": "x"})
		req := artifactReq(http.MethodPost, target, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", "T-1")
		rec := httptest.NewRecorder()
		s.handleAgentBoardCreateArtifact(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("publish = %d %s", rec.Code, rec.Body.String())
		}
		var created struct {
			Artifact map[string]any `json:"artifact"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &created)
		return created.Artifact
	}

	artifact := publishHTML("http://ignored/api/agent-board/tasks/T-1/artifacts")
	url := artifact["url"].(string)
	if !strings.HasPrefix(url, "https://deck.example.com/api/agent-board/artifacts/") {
		t.Fatalf("public_base_url not used: %q", url)
	}
	if artifact["url_source"] != "public_base_url" || artifact["url_local_only"] != false {
		t.Fatalf("public_base_url source = %+v", artifact)
	}

	// browser.trusted_origins[0] is the fallback when public_base_url is unset.
	// Browser.Enabled stays nil on purpose: the address is still readable and
	// the browser security semantics are not involved.
	s.cm.Get().PublicBaseURL = ""
	s.cm.Get().Browser.TrustedOrigins = []string{"https://ccview.xpsl.soarx.cc/"}
	artifact = publishHTML("http://ignored/api/agent-board/tasks/T-1/artifacts")
	if !strings.HasPrefix(artifact["url"].(string), "https://ccview.xpsl.soarx.cc/api/agent-board/artifacts/") {
		t.Fatalf("trusted_origins fallback not used: %+v", artifact)
	}
	if strings.Contains(artifact["url"].(string), "ccview.xpsl.soarx.cc//") {
		t.Fatalf("trusted origin trailing slash not normalized: %+v", artifact)
	}
	if artifact["url_source"] != "trusted_origins" {
		t.Fatalf("trusted_origins source = %+v", artifact)
	}

	// public_base_url still wins over trusted_origins.
	s.cm.Get().PublicBaseURL = "https://explicit.example.com"
	artifact = publishHTML("http://ignored/api/agent-board/tasks/T-1/artifacts")
	if artifact["url_source"] != "public_base_url" {
		t.Fatalf("priority = %+v", artifact)
	}

	// Then the request host.
	s.cm.Get().PublicBaseURL = ""
	s.cm.Get().Browser.TrustedOrigins = nil
	artifact = publishHTML("http://deck.local/api/agent-board/tasks/T-1/artifacts")
	if artifact["url_source"] != "request" || !strings.HasPrefix(artifact["url"].(string), "http://deck.local/") {
		t.Fatalf("request fallback = %+v", artifact)
	}

	// Only trusted_origins[0] is considered, and it must be a bare http(s)
	// origin: an invalid/path-bearing first entry falls through to the request
	// layer instead of minting an unusable link (later entries are not used).
	for _, origin := range []string{"not-an-origin", "ftp://good.example.com", "https://good.example.com/path", "https://good.example.com/?x=1"} {
		s.cm.Get().Browser.TrustedOrigins = []string{origin, "https://good.example.com"}
		artifact = publishHTML("http://deck.local/api/agent-board/tasks/T-1/artifacts")
		if artifact["url_source"] != "request" || !strings.HasPrefix(artifact["url"].(string), "http://deck.local/") {
			t.Fatalf("invalid trusted origin %q must fall back to request: %+v", origin, artifact)
		}
	}

	// kind=url stores the external link and redirects from the id route.
	payload, _ := json.Marshal(map[string]any{"kind": "url", "title": "设计稿", "url": "https://example.com/design"})
	req := artifactReq(http.MethodPost, "http://deck.local/api/agent-board/tasks/T-1/artifacts", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "T-1")
	rec := httptest.NewRecorder()
	s.handleAgentBoardCreateArtifact(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("url publish = %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Artifact map[string]any `json:"artifact"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created.Artifact["id"].(string)
	if created.Artifact["url"] != "https://example.com/design" {
		t.Fatalf("url artifact = %+v", created.Artifact)
	}
	req = artifactReq(http.MethodGet, "http://deck.local/api/agent-board/artifacts/"+id, nil)
	req.SetPathValue("id", id)
	rec = httptest.NewRecorder()
	s.handleAgentBoardArtifact(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://example.com/design" {
		t.Fatalf("url redirect = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// Non-http(s) URLs are rejected.
	payload, _ = json.Marshal(map[string]any{"kind": "url", "url": "file:///etc/passwd"})
	req = artifactReq(http.MethodPost, "http://deck.local/api/agent-board/tasks/T-1/artifacts", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "T-1")
	rec = httptest.NewRecorder()
	s.handleAgentBoardCreateArtifact(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("file url status = %d", rec.Code)
	}
}

func TestAgentdeckArtifactPublishUsesTrustedOrigins(t *testing.T) {
	s, _ := boardMCPServer(t)
	createArtifactTestTask(t, s)
	// Browser.Enabled stays nil: the address is usable even when the browser
	// panel is off.
	s.cm.Get().Browser.TrustedOrigins = []string{"https://ccview.xpsl.soarx.cc/"}

	out, err := s.agentdeckToolCall("worker", "artifact_publish", map[string]any{
		"task_id": "T-1", "kind": "html", "filename": "x.html", "content": "<h1>x</h1>",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "\"url\": \"https://ccview.xpsl.soarx.cc/api/agent-board/artifacts/") {
		t.Fatalf("artifact_publish did not use trusted_origins: %s", out)
	}
	if !strings.Contains(out, "\"url_source\": \"trusted_origins\"") {
		t.Fatalf("artifact_publish source = %s", out)
	}

	// public_base_url still wins over the trusted-origin fallback.
	s.cm.Get().PublicBaseURL = "https://explicit.example.com"
	out, err = s.agentdeckToolCall("worker", "artifact_publish", map[string]any{
		"task_id": "T-1", "kind": "html", "filename": "y.html", "content": "<h1>y</h1>",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "\"url\": \"https://explicit.example.com/api/agent-board/artifacts/") ||
		!strings.Contains(out, "\"url_source\": \"public_base_url\"") {
		t.Fatalf("artifact_publish priority = %s", out)
	}
}

func TestAgentdeckArtifactPublishToolAvailableToWorker(t *testing.T) {
	s, _ := boardMCPServer(t)
	createArtifactTestTask(t, s)

	// A plain worker (not main_agent) may publish artifacts.
	out, err := s.agentdeckToolCall("worker", "artifact_publish", map[string]any{
		"task_id": "T-1", "title": "Worker 报告", "kind": "html",
		"filename": "report.html", "content": "<h1>from worker</h1>",
	})
	if err != nil {
		t.Fatalf("worker publish: %v", err)
	}
	if !strings.Contains(out, "http://127.0.0.1:8321/api/agent-board/artifacts/") {
		t.Fatalf("listen fallback URL missing: %s", out)
	}
	if !strings.Contains(out, "\"source_session\": \"worker\"") {
		t.Fatalf("source_session default missing: %s", out)
	}
	if !strings.Contains(out, "\"url_source\": \"listen\"") || !strings.Contains(out, "\"url_local_only\": true") {
		t.Fatalf("listen fallback must be marked local-only: %s", out)
	}

	// base64 content is decoded before storage.
	b64 := base64.StdEncoding.EncodeToString([]byte("binary\x00payload"))
	if _, err := s.agentdeckToolCall("worker", "artifact_publish", map[string]any{
		"task_id": "T-1", "kind": "file", "filename": "data.bin", "content": b64, "encoding": "base64",
	}); err != nil {
		t.Fatalf("base64 publish: %v", err)
	}
	task, _ := s.boardDB.GetTask("T-1")
	list := task["artifacts"].([]map[string]any)
	last := list[len(list)-1]
	abs := filepath.Join(s.artifactsRoot(), last["storage_path"].(string))
	data, err := os.ReadFile(abs)
	if err != nil || string(data) != "binary\x00payload" {
		t.Fatalf("stored bytes = %q err=%v", data, err)
	}

	// Board tools stay main-agent only; artifact_publish is not a board tool.
	if _, err := s.agentdeckToolCall("worker", "board_create", map[string]any{"title": "x"}); err == nil || !strings.Contains(err.Error(), "仅主 Agent") {
		t.Fatalf("board_create should still be main-agent only: %v", err)
	}
}
