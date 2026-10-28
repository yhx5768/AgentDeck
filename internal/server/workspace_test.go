package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"cc-webui/internal/config"
	"cc-webui/internal/store"
)

// 工作区面板的测试重点只有一条：**面板只能看见当前会话工作区里的东西**。
// 越界（..、绝对路径、符号链接）必须被挡住；列目录、读文件、git 改动要正确。

func wsTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir}
	cm := config.NewManager(filepath.Join(dir, "config.yaml"), cfg)
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s := &Server{cm: cm, st: st}
	ws := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "outside"), 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := st.Upsert(&store.Session{ID: "s1", Cwd: ws}, nil); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	return s, ws
}

// wsCall 直接打 handler（不经过 mux：测试里的 Server 没注册路由）。
func wsCall(t *testing.T, h http.HandlerFunc, url, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

func wsGet(t *testing.T, s *Server, url string, pathValue string) *httptest.ResponseRecorder {
	t.Helper()
	return wsCall(t, s.handleWorkspaceTree, url, pathValue)
}

func wsJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return m
}

func TestWorkspaceTreeListsDirsAndFiles(t *testing.T) {
	s, ws := wsTestServer(t)
	writeFile(t, filepath.Join(ws, "README.md"), []byte("# hi"))
	writeFile(t, filepath.Join(ws, "src", "main.go"), []byte("package main"))
	writeFile(t, filepath.Join(ws, "node_modules", "x", "y.js"), []byte("x"))

	w := wsGet(t, s, "/api/sessions/s1/workspace/tree", "s1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := wsJSON(t, w)
	entries, _ := body["entries"].([]any)
	if len(entries) != 3 {
		t.Fatalf("entries = %v, want src + README.md + node_modules(占位)", entries)
	}
	byName := map[string]map[string]any{}
	for _, e := range entries {
		m, _ := e.(map[string]any)
		byName[m["name"].(string)] = m
	}
	if src := byName["src"]; src == nil || src["dir"] != true {
		t.Fatalf("src entry = %v, want dir", src)
	}
	if rm := byName["README.md"]; rm == nil || rm["dir"] != false {
		t.Fatalf("README.md entry = %v, want file", rm)
	}
	if nm := byName["node_modules"]; nm == nil || nm["skip"] != true {
		t.Fatalf("node_modules entry = %v, want skip", nm)
	}
	// 目录排在文件前面
	first, _ := entries[0].(map[string]any)
	last, _ := entries[len(entries)-1].(map[string]any)
	if first["dir"] != true || last["dir"] != false {
		t.Fatalf("排序不是「目录在前」：first=%v last=%v", first, last)
	}
}

// 忽略名单里的目录只给占位，不给内容（展开 node_modules 会刷出上千条）。
func TestWorkspaceTreeSkipsHeavyDirs(t *testing.T) {
	s, ws := wsTestServer(t)
	writeFile(t, filepath.Join(ws, "node_modules", "x", "y.js"), []byte("x"))

	w := wsGet(t, s, "/api/sessions/s1/workspace/tree?path=node_modules", "s1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := wsJSON(t, w)
	if body["skipped"] != true {
		t.Fatalf("skipped = %v, want true", body["skipped"])
	}
	entries, _ := body["entries"].([]any)
	if len(entries) != 0 {
		t.Fatalf("entries = %v, want empty", entries)
	}
}

func TestWorkspaceTreeSubdirectory(t *testing.T) {
	s, ws := wsTestServer(t)
	writeFile(t, filepath.Join(ws, "src", "main.go"), []byte("package main"))
	writeFile(t, filepath.Join(ws, "src", "util.go"), []byte("package main"))

	w := wsGet(t, s, "/api/sessions/s1/workspace/tree?path=src", "s1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	entries, _ := wsJSON(t, w)["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries = %v, want 2 files under src/", entries)
	}
	e, _ := entries[0].(map[string]any)
	if e["path"] != "src/main.go" {
		t.Fatalf("path = %v, want src/main.go（相对路径）", e["path"])
	}
}

func TestWorkspaceTreeRejectsEscape(t *testing.T) {
	s, ws := wsTestServer(t)
	writeFile(t, filepath.Join(filepath.Dir(ws), "outside", "secret.txt"), []byte("secret"))

	for _, q := range []string{"path=..", "path=../outside", "path=%2Fetc"} {
		w := wsGet(t, s, "/api/sessions/s1/workspace/tree?"+q, "s1")
		if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 403/404 (越界必须被挡)", q, w.Code)
		}
	}
}

func TestWorkspaceFileReturnsContent(t *testing.T) {
	s, ws := wsTestServer(t)
	writeFile(t, filepath.Join(ws, "notes.md"), []byte("# notes\nbody"))

	w := wsCall(t, s.handleWorkspaceFile, "/api/sessions/s1/workspace/file?path=notes.md", "s1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := wsJSON(t, w)
	if body["content"] != "# notes\nbody" {
		t.Fatalf("content = %v", body["content"])
	}
	if body["name"] != "notes.md" {
		t.Fatalf("name = %v", body["name"])
	}
}

func TestWorkspaceFileRejectsOutsideAndBinary(t *testing.T) {
	s, ws := wsTestServer(t)
	outside := filepath.Join(filepath.Dir(ws), "outside", "secret.txt")
	writeFile(t, outside, []byte("secret"))

	w := wsGet(t, s, "/api/sessions/s1/workspace/file?path=../outside/secret.txt", "s1")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}

	bin := filepath.Join(ws, "a.bin")
	writeFile(t, bin, []byte{0x00, 0x01, 0x02})
	w = wsCall(t, s.handleWorkspaceFile, "/api/sessions/s1/workspace/file?path=a.bin", "s1")
	if w.Code != http.StatusOK {
		t.Fatalf("binary status = %d, want 200", w.Code)
	}
	if body := wsJSON(t, w); body["binary"] != true {
		t.Fatalf("binary = %v, want true", body["binary"])
	} else if _, ok := body["content"]; ok {
		t.Fatalf("binary file must not carry content: %v", body)
	}
}

func TestWorkspaceFileRejectsSymlinkEscape(t *testing.T) {
	s, ws := wsTestServer(t)
	outside := filepath.Join(filepath.Dir(ws), "outside", "secret.txt")
	writeFile(t, outside, []byte("secret"))
	link := filepath.Join(ws, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	w := wsCall(t, s.handleWorkspaceFile, "/api/sessions/s1/workspace/file?path=link.txt", "s1")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (符号链接逃逸必须被 EvalSymlinks 拦住)", w.Code)
	}
}

func TestWorkspaceUnknownSession(t *testing.T) {
	s, _ := wsTestServer(t)
	w := wsGet(t, s, "/api/sessions/nope/workspace/tree", "nope")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// git 仓库里的改动：status 要标出被改过的文件，diff 要给出内容。
func TestWorkspaceGitStatusAndDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	s, ws := wsTestServer(t)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", ws}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init")
	writeFile(t, filepath.Join(ws, "a.txt"), []byte("one\n"))
	run("add", "a.txt")
	run("commit", "-m", "init")
	writeFile(t, filepath.Join(ws, "a.txt"), []byte("one\ntwo\n"))

	w := wsCall(t, s.handleWorkspaceStatus, "/api/sessions/s1/workspace/status", "s1")
	if w.Code != http.StatusOK {
		t.Fatalf("status endpoint = %d", w.Code)
	}
	body := wsJSON(t, w)
	if body["repo"] != true {
		t.Fatalf("repo = %v, want true", body["repo"])
	}
	changes, _ := body["changes"].(map[string]any)
	if _, ok := changes["a.txt"]; !ok {
		t.Fatalf("changes = %v, want a.txt marked", changes)
	}

	w = wsCall(t, s.handleWorkspaceDiff, "/api/sessions/s1/workspace/diff?path=a.txt", "s1")
	if w.Code != http.StatusOK {
		t.Fatalf("diff endpoint = %d", w.Code)
	}
	diff, _ := wsJSON(t, w)["diff"].(string)
	if !strings.Contains(diff, "+two") {
		t.Fatalf("diff = %q, want it to contain +two", diff)
	}
}

// 未跟踪的新文件也要能在面板里看到「新增了什么」，否则新文件看着像没改过。
func TestWorkspaceDiffUntrackedFile(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	s, ws := wsTestServer(t)
	cmd := exec.Command("git", "-C", ws, "init")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	writeFile(t, filepath.Join(ws, "new.txt"), []byte("fresh\n"))

	w := wsCall(t, s.handleWorkspaceDiff, "/api/sessions/s1/workspace/diff?path=new.txt", "s1")
	if w.Code != http.StatusOK {
		t.Fatalf("diff endpoint = %d, body=%s", w.Code, w.Body.String())
	}
	body := wsJSON(t, w)
	if body["untracked"] != true {
		t.Fatalf("untracked = %v, want true", body["untracked"])
	}
	diff, _ := body["diff"].(string)
	if !strings.Contains(diff, "+fresh") {
		t.Fatalf("diff = %q, want it to contain +fresh", diff)
	}
}

// 不是 git 仓库时不算错误：repo=false、diff 为空。
func TestWorkspaceDiffOutsideGitRepo(t *testing.T) {
	s, ws := wsTestServer(t)
	writeFile(t, filepath.Join(ws, "a.txt"), []byte("x"))
	w := wsCall(t, s.handleWorkspaceDiff, "/api/sessions/s1/workspace/diff?path=a.txt", "s1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if body := wsJSON(t, w); body["repo"] != false {
		t.Fatalf("repo = %v, want false", body["repo"])
	}
}

// 「打开」接口只测拒绝路径：成功路径会真的把文件丢给系统默认程序，
// 在测试机上跑一次等于替用户弹一个编辑器。
func TestWorkspaceOpenRejectsBadPaths(t *testing.T) {
	s, ws := wsTestServer(t)
	outside := filepath.Join(filepath.Dir(ws), "outside", "secret.txt")
	writeFile(t, outside, []byte("secret"))

	post := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		body := strings.NewReader(`{"path":` + strconv.Quote(path) + `}`)
		req := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/workspace/open", body)
		req.SetPathValue("id", "s1")
		w := httptest.NewRecorder()
		s.handleWorkspaceOpen(w, req)
		return w
	}

	if w := post("../outside/secret.txt"); w.Code != http.StatusForbidden {
		t.Errorf("越界路径: status = %d, want 403", w.Code)
	}
	if w := post("nope.txt"); w.Code != http.StatusNotFound {
		t.Errorf("不存在的文件: status = %d, want 404", w.Code)
	}
	if w := post(""); w.Code != http.StatusBadRequest {
		t.Errorf("空路径: status = %d, want 400", w.Code)
	}
}

// 搜索必须递归整棵树：文件树是懒加载的，没展开过的深层文件也得能找到，
// 而且输入完整相对路径时不能一条都匹配不上。
func TestWorkspaceSearchFindsDeepFilesByPath(t *testing.T) {
	s, ws := wsTestServer(t)
	deep := filepath.Join(ws, "work", "projects", "risk-identification", "deliverables",
		"riskminer-skills", "riskminer-tool-20261026.zip")
	writeFile(t, deep, []byte("zip"))

	want := "work/projects/risk-identification/deliverables/riskminer-skills/riskminer-tool-20261026.zip"
	for _, q := range []string{"riskminer-tool-20261026", "work/projects/risk-identification"} {
		w := wsCall(t, s.handleWorkspaceSearch, "/api/sessions/s1/workspace/search?q="+q, "s1")
		if w.Code != http.StatusOK {
			t.Fatalf("q=%s: status = %d, body=%s", q, w.Code, w.Body.String())
		}
		entries, _ := wsJSON(t, w)["entries"].([]any)
		found := false
		for _, raw := range entries {
			e, _ := raw.(map[string]any)
			if e["path"] == want && e["dir"] == false {
				found = true
			}
		}
		if !found {
			t.Fatalf("q=%s: entries = %v, want %s", q, entries, want)
		}
	}
}

// .gitignore 忽略的文件：树和搜索都要显示（只是打标），不能替用户藏起来。
func TestWorkspaceShowsGitIgnoredFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	s, ws := wsTestServer(t)
	if out, err := exec.Command("git", "-C", ws, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	writeFile(t, filepath.Join(ws, ".gitignore"), []byte("secret*\n"))
	writeFile(t, filepath.Join(ws, "secret.env"), []byte("TOKEN=1"))
	writeFile(t, filepath.Join(ws, "visible.txt"), []byte("hi"))

	w := wsGet(t, s, "/api/sessions/s1/workspace/tree", "s1")
	if w.Code != http.StatusOK {
		t.Fatalf("tree status = %d, body=%s", w.Code, w.Body.String())
	}
	byName := map[string]map[string]any{}
	for _, raw := range wsJSON(t, w)["entries"].([]any) {
		e, _ := raw.(map[string]any)
		byName[e["name"].(string)] = e
	}
	if e := byName["secret.env"]; e == nil || e["ignored"] != true {
		t.Fatalf("secret.env = %v, want it listed with ignored=true", e)
	}
	if e := byName["visible.txt"]; e == nil || e["ignored"] != nil {
		t.Fatalf("visible.txt = %v, want it listed without ignored", e)
	}

	w = wsCall(t, s.handleWorkspaceSearch, "/api/sessions/s1/workspace/search?q=secret", "s1")
	entries, _ := wsJSON(t, w)["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("search entries = %v, want ignored file included", entries)
	}
	if e, _ := entries[0].(map[string]any); e["ignored"] != true {
		t.Fatalf("search result = %v, want ignored=true", e)
	}
}
