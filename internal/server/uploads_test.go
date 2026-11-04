package server

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cc-webui/internal/runner"
)

// 用户消息里的图片不落进事件（base64 会让事件日志和每次 SSE 回放膨胀），
// 而是存到 data/uploads/<sid>/，事件只记文件名，前端经会话上传接口取回。
// 这个接口是新的读取口子，所以「必须拒绝」的几条都要钉住。

func getUpload(t *testing.T, s *Server, sid, name string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid+"/uploads/"+name, nil)
	req.SetPathValue("id", sid)
	req.SetPathValue("name", name)
	w := httptest.NewRecorder()
	s.handleSessionUpload(w, req)
	return w
}

func TestSessionUploadServesOwnImage(t *testing.T) {
	s, id := replayTestServer(t, 0)
	writeFile(t, filepath.Join(s.cfg().DataDir, "uploads", id, "image.png"), pngBytes)

	w := getUpload(t, s, id, "image.png")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff header")
	}
}

func TestSessionUploadRejectsOtherSessionAndTraversal(t *testing.T) {
	s, _ := replayTestServer(t, 0)
	// 另一个会话目录里的同名文件不可达
	writeFile(t, filepath.Join(s.cfg().DataDir, "uploads", "s2", "x.png"), pngBytes)
	if w := getUpload(t, s, "s1", "x.png"); w.Code != http.StatusNotFound {
		t.Errorf("other session file: status = %d, want 404", w.Code)
	}
	// 目录穿越：name 里的分隔符被 sanitizeUploadName 抹掉，仍落在本会话目录
	writeFile(t, filepath.Join(s.cfg().DataDir, "uploads", "secret.png"), pngBytes)
	if w := getUpload(t, s, "s1", "../secret.png"); w.Code != http.StatusNotFound {
		t.Errorf("traversal: status = %d, want 404", w.Code)
	}
}

func TestSessionUploadRejectsNonImage(t *testing.T) {
	s, id := replayTestServer(t, 0)
	writeFile(t, filepath.Join(s.cfg().DataDir, "uploads", id, "x.svg"), []byte(`<svg/>`))
	if w := getUpload(t, s, id, "x.svg"); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("svg: status = %d, want 415", w.Code)
	}
}

func TestSessionUploadRejectsSymlinkEscape(t *testing.T) {
	s, id := replayTestServer(t, 0)
	outside := filepath.Join(s.cfg().DataDir, "outside.png")
	writeFile(t, outside, pngBytes)
	dir := filepath.Join(s.cfg().DataDir, "uploads", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.png")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if w := getUpload(t, s, id, "link.png"); w.Code != http.StatusForbidden {
		t.Errorf("symlink escape: status = %d, want 403", w.Code)
	}
}

func TestSessionUploadUnknownSession(t *testing.T) {
	s, _ := replayTestServer(t, 0)
	if w := getUpload(t, s, "nope", "x.png"); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// 发消息时的落盘：图片写到会话上传目录、返回文件名、能被接口取回。
// 事件里记录文件名的那一半在 runner 侧测（Enqueue 会拉起 worker，与
// t.TempDir 清理竞态，所以不在这里经 HTTP 走完整链路）。
func TestPersistMessageImages(t *testing.T) {
	s, id := replayTestServer(t, 0)
	names, err := s.persistMessageImages(id, []runner.Image{
		{MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(pngBytes)},
	})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if len(names) != 1 || !strings.HasSuffix(names[0], ".png") {
		t.Fatalf("names = %v, want one .png", names)
	}
	p := filepath.Join(s.cfg().DataDir, "uploads", id, names[0])
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("persisted image missing at %s: %v", p, err)
	}
	if w := getUpload(t, s, id, names[0]); w.Code != http.StatusOK {
		t.Fatalf("serve persisted image: status = %d, want 200", w.Code)
	}
	// 无图片时不动磁盘、返回 nil
	if n, err := s.persistMessageImages(id, nil); err != nil || n != nil {
		t.Fatalf("nil images: names=%v err=%v, want nil/nil", n, err)
	}
}

// 同名图片不能被覆盖：同一批粘贴的图都叫 image.png，落盘要各自留一份。
func TestPersistMessageImagesNoClobber(t *testing.T) {
	s, id := replayTestServer(t, 0)
	img := runner.Image{MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(pngBytes)}
	names, err := s.persistMessageImages(id, []runner.Image{img, img})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if len(names) != 2 || names[0] == names[1] {
		t.Fatalf("names = %v, want two distinct names", names)
	}
}
