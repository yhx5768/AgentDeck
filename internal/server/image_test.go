package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cc-webui/internal/config"
	"cc-webui/internal/store"
)

// 图片接口的白名单是这个测试的全部意义：它让模型回复里的本地图片能在前端显示，
// 代价是开了��个能读二进制文件的口子。所以每一条「必须拒绝」都得钉住——
// 哪天有人为了图能显示把限制放宽了，这里就该红。
func imageTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir}
	cm := config.NewManager(filepath.Join(dir, "config.yaml"), cfg)
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s := &Server{cm: cm, st: st}
	// 会话工作区：workspace 内的文件可读，workspace 外的一律拒绝。
	ws := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := st.Upsert(&store.Session{ID: "s1", Cwd: ws}, nil); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	return s, ws
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func getImage(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/image?path="+path, nil)
	w := httptest.NewRecorder()
	s.handleImage(w, req)
	return w
}

// 一张最小的合法 PNG（1x1 透明像素）。
var pngBytes = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 'I', 'D', 'A',
	'T', 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
	0x00, 0x00, 0x00, 'I', 'E', 'N', 'D', 0xae,
	0x42, 0x60, 0x82,
}

func TestImageServesWorkspaceFile(t *testing.T) {
	s, ws := imageTestServer(t)
	p := filepath.Join(ws, "shot.png")
	writeFile(t, p, pngBytes)

	w := getImage(t, s, p)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff header")
	}
	if !strings.Contains(w.Body.String(), "PNG") {
		t.Errorf("body does not look like the png bytes: %q", w.Body.String()[:min(32, len(w.Body.String()))])
	}
}

func TestImageRejectsOutsideWorkspace(t *testing.T) {
	s, ws := imageTestServer(t)
	outside := filepath.Join(filepath.Dir(ws), "outside", "secret.png")
	writeFile(t, outside, pngBytes)

	w := getImage(t, s, outside)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestImageRejectsNonImageTypes(t *testing.T) {
	s, ws := imageTestServer(t)
	// svg 必须拒：直接访问 URL 会被当文档渲染，内嵌脚本就能跑。
	svg := filepath.Join(ws, "x.svg")
	writeFile(t, svg, []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	if w := getImage(t, s, svg); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("svg: status = %d, want 415", w.Code)
	}
	// 文本也不行
	txt := filepath.Join(ws, "x.txt")
	writeFile(t, txt, []byte("hello"))
	if w := getImage(t, s, txt); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("txt: status = %d, want 415", w.Code)
	}
}

func TestImageRejectsRelativePath(t *testing.T) {
	s, _ := imageTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/image?path=shot.png", nil)
	w := httptest.NewRecorder()
	s.handleImage(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestImageRejectsMissingFile(t *testing.T) {
	s, ws := imageTestServer(t)
	w := getImage(t, s, filepath.Join(ws, "nope.png"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestImageRejectsSymlinkEscape(t *testing.T) {
	s, ws := imageTestServer(t)
	outside := filepath.Join(filepath.Dir(ws), "outside", "secret.png")
	writeFile(t, outside, pngBytes)
	link := filepath.Join(ws, "link.png")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	w := getImage(t, s, link)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (symlink 逃逸必须被 EvalSymlinks 拦住)", w.Code)
	}
}
