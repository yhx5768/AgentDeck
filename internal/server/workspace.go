package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"cc-webui/internal/store"
)

// 工作区面板（Codex app 风格的右侧文件区）的后端。
//
// 与 /api/files 的关键差别：**锚在「当前这个会话」的 cwd 上**，而不是
// 「任一会话的 cwd」。面板跟着会话走，锚错会话等于把别的项目的目录树露出来。
// 路径先经 EvalSymlinks 解析真实路径，再判是否还在 cwd 之内——符号链接逃逸一律拒绝。
//
// 全部接口只读；唯一的副作用是 open（用系统默认程序打开文件），那是用户明确点的按钮。

const (
	// wsMaxFileBytes 单文件预览上限。比 /api/files 的 2 MiB 宽一档：面板里常翻
	// 源码和大点的配置，2 MiB 截断得太勤；超过就带 truncated 标记返回。
	wsMaxFileBytes = 4 << 20
	// wsMaxEntries 单目录最多返回的条目数，防止一次拉爆几千个文件。
	wsMaxEntries = 2000
	// wsMaxSearchResults / wsMaxSearchScanned：搜索的返回条数与扫描上限。
	// 搜索是递归遍历整棵树（文件树本身是懒加载的）。上限不能太小：实测
	// ~/Projects/reading 的 work/ 下就有 66 万项，5 万的上限会连目标文件
	// 所在目录都没走到就返回「没找到」。到上限/超时都带 truncated 回来。
	wsMaxSearchResults = 200
	wsMaxSearchScanned = 1000000
	// wsSearchTimeout 单次搜索的墙上时间上限。超大仓库（或网络盘）里宁可
	// 返回部分结果，也别让请求一直挂着。
	wsSearchTimeout = 6 * time.Second
	// wsGitTimeout git 子进程超时。
	wsGitTimeout = 10 * time.Second
)

// wsSkipDirs 不进文件树的目录：版本库内部、依赖目录、缓存目录。
// 展开 node_modules 能刷出上千个条目，对「看一眼项目结构」毫无帮助。
var wsSkipDirs = map[string]bool{
	".git": true, "node_modules": true, ".venv": true, "venv": true,
	"__pycache__": true, ".mypy_cache": true, ".pytest_cache": true, ".ruff_cache": true,
	".next": true, ".nuxt": true, ".turbo": true, ".gradle": true,
}

var (
	// errWsOutside：路径解析后跑出了会话 cwd。
	errWsOutside = errors.New("outside workspace")
	// errWsMissing：路径不存在（或符号链接指向不存在的目标）。
	errWsMissing = errors.New("not found")
)

// wsEntry 是文件树里的一个节点。
type wsEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"` // 相对会话 cwd 的路径（前端只认相对路径）
	Dir     bool   `json:"dir"`
	Size    int64  `json:"size"`
	Mtime   int64  `json:"mtime"` // unix 秒
	Skip    bool   `json:"skip,omitempty"`
	Ignored bool   `json:"ignored,omitempty"` // 被 .gitignore 忽略（仍然展示，只是打标）
}

// workspaceSession 取出路由上的会话和它的 cwd 根目录。
// 会话不存在 / 没设工作区 / 工作区已被删，各自给出不同错误码。
func (s *Server) workspaceSession(w http.ResponseWriter, r *http.Request) (*store.Session, string, bool) {
	sess := s.st.Get(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "会话不存在")
		return nil, "", false
	}
	if sess.Cwd == "" {
		writeErr(w, http.StatusBadRequest, "会话未设置工作区")
		return nil, "", false
	}
	root, err := filepath.Abs(sess.Cwd)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "工作区路径无效")
		return nil, "", false
	}
	return sess, root, true
}

// wsResolve 把相对路径拼到 root 之下，解析符号链接后确认仍在 root 之内。
//
// 只接受相对路径：绝对路径一律拒绝，免得有人拿 /etc/passwd 之类来试。
// 路径里出现 ".." 段直接拒绝——filepath.Clean("/../x") 会把 ".." 吃掉，
// 看起来"没越界"，但那只是 Clean 的副作用，不是授权语义。
// 拼完再解析符号链接、比对前缀，符号链接逃逸也一并挡住。
func (s *Server) wsResolve(root, rel string) (string, error) {
	if rel == "" {
		return root, nil
	}
	if filepath.IsAbs(rel) {
		return "", errWsOutside
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == ".." {
			return "", errWsOutside
		}
	}
	joined := filepath.Join(root, filepath.Clean("/"+rel))
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", errWsMissing
	}
	rootResolved := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		rootResolved = r
	}
	if resolved != rootResolved && !strings.HasPrefix(resolved, rootResolved+string(os.PathSeparator)) {
		return "", errWsOutside
	}
	return resolved, nil
}

// wsWriteResolveError 把 wsResolve 的错误翻译成 HTTP 响应。
func wsWriteResolveError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errWsOutside):
		writeErr(w, http.StatusForbidden, "路径超出会话工作区")
	case errors.Is(err, errWsMissing):
		writeErr(w, http.StatusNotFound, "文件不存在")
	default:
		writeErr(w, http.StatusBadRequest, "路径无效")
	}
}

// handleWorkspaceTree 列目录：GET /api/sessions/{id}/workspace/tree?path=<相对路径>
//
// 一层一层列（前端点开目录才请求下一层），跟 Codex app-server 的
// fs/readDirectory 一样是懒加载的，不然大项目一次拉全树会卡住。
func (s *Server) handleWorkspaceTree(w http.ResponseWriter, r *http.Request) {
	_, root, ok := s.workspaceSession(w, r)
	if !ok {
		return
	}
	rel := r.URL.Query().Get("path")
	dir := root
	if rel != "" {
		p, err := s.wsResolve(root, rel)
		if err != nil {
			wsWriteResolveError(w, err)
			return
		}
		dir = p
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		writeErr(w, http.StatusNotFound, "目录不存在")
		return
	}
	// 忽略名单里的目录只列出一行占位，不给内容——展开 node_modules 能刷出
	// 上千个条目，对「看一眼项目结构」毫无帮助，还把接口拖慢。
	// 只对「展开的子目录」生效：项目根本身叫 venv/node_modules 时照样要能列。
	if rel != "" && wsSkipDirs[filepath.Base(dir)] {
		writeJSON(w, http.StatusOK, map[string]any{
			"root":    root,
			"path":    filepath.ToSlash(rel),
			"entries": []wsEntry{},
			"skipped": true,
		})
		return
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		writeErr(w, http.StatusForbidden, "目录不可读")
		return
	}
	entries := make([]wsEntry, 0, len(items))
	for _, it := range items {
		e := wsEntry{
			Name: it.Name(),
			Path: filepath.ToSlash(filepath.Join(rel, it.Name())),
			Dir:  it.IsDir(),
		}
		if e.Dir {
			e.Skip = wsSkipDirs[it.Name()]
		}
		if info, err := it.Info(); err == nil {
			e.Size = info.Size()
			e.Mtime = info.ModTime().Unix()
		}
		entries = append(entries, e)
	}
	// 目录在前、文件在后，各自按名字排（大小写不敏感），跟系统文件管理器一致。
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	truncated := false
	if len(entries) > wsMaxEntries {
		entries = entries[:wsMaxEntries]
		truncated = true
	}
	// git 忽略的文件**照样列出来**（面板不是 git，隐藏掉反而让人以为文件没了），
	// 只是打一个标记，让用户知道它不会进版本库。
	s.markIgnored(root, entries)
	writeJSON(w, http.StatusOK, map[string]any{
		"root":      root,
		"path":      filepath.ToSlash(rel),
		"entries":   entries,
		"truncated": truncated,
	})
}

// handleWorkspaceSearch 在整个工作区里递归搜索：GET /api/sessions/{id}/workspace/search?q=
//
// 文件树是懒加载的，前端原来那种「只筛已加载节点」的做法，找没展开过的
// 深层文件必然找不到（输入完整相对路径更是一条都匹配不上，因为筛的是
// entry.name）。这里由服务端走一遍树，名字和相对路径都参与匹配，
// 被 .gitignore 忽略的文件也会返回（面板不替用户藏东西）。
func (s *Server) handleWorkspaceSearch(w http.ResponseWriter, r *http.Request) {
	_, root, ok := s.workspaceSession(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"q": "", "entries": []wsEntry{}, "truncated": false})
		return
	}
	qLower := strings.ToLower(filepath.ToSlash(q))
	results := make([]wsEntry, 0, 32)
	scanned := 0
	truncated := false

	// 查询里带路径时（典型：用户从别处复制了一整条相对路径），只走那条
	// 路径的子树。66 万项的 work/ 全量扫要 1.7s，而直接定位到目录前缀
	// 是毫秒级；裸文件名的搜索仍然走全树。
	walkRoot := root
	if i := strings.LastIndex(filepath.ToSlash(q), "/"); i > 0 {
		if p, err := s.wsResolve(root, q[:i]); err == nil {
			// 只在前缀确实落在 root 之内时缩范围（root 自己是符号链接时
			// resolved 路径和 root 前缀对不上，退回全树扫，别算错相对路径）。
			if rel, relErr := filepath.Rel(root, p); relErr == nil &&
				rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				if info, statErr := os.Stat(p); statErr == nil && info.IsDir() {
					walkRoot = p
				}
			}
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), wsSearchTimeout)
	defer cancel()
	walkErr := filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
		// 每 256 项看一次取消/超时：前端换了筛选词会把旧请求 abort 掉，
		// 旧遍历必须及时停下，否则连打几个字符就是几次全树扫描叠在一起。
		if scanned%256 == 0 && ctx.Err() != nil {
			truncated = true
			return fs.SkipAll
		}
		if err != nil {
			// 单个目录没权限不该让整次搜索失败，跳过它继续。
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == root || p == walkRoot {
			return nil
		}
		if d.IsDir() && wsSkipDirs[d.Name()] {
			return fs.SkipDir
		}
		if d.Type()&fs.ModeSymlink != 0 {
			// 符号链接不跟：跳出去就破坏了「只在这个工作区里」的边界。
			return nil
		}
		scanned++
		if scanned > wsMaxSearchScanned {
			truncated = true
			return fs.SkipAll
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if !strings.Contains(strings.ToLower(relSlash), qLower) &&
			!strings.Contains(strings.ToLower(d.Name()), qLower) {
			return nil
		}
		e := wsEntry{
			Name: d.Name(),
			Path: relSlash,
			Dir:  d.IsDir(),
		}
		if info, infoErr := d.Info(); infoErr == nil {
			e.Size = info.Size()
			e.Mtime = info.ModTime().Unix()
		}
		results = append(results, e)
		if len(results) >= wsMaxSearchResults {
			truncated = true
			return fs.SkipAll
		}
		return nil
	})
	if r.Context().Err() != nil {
		// 客户端已经断开（换了筛选词），别再往回写响应。
		return
	}
	if walkErr != nil {
		writeErr(w, http.StatusForbidden, "工作区不可读")
		return
	}
	s.markIgnored(root, results)
	writeJSON(w, http.StatusOK, map[string]any{
		"q":         q,
		"entries":   results,
		"truncated": truncated,
	})
}

// markIgnored 给「被 .gitignore 忽略」的条目打标。
//
// 用一条 `git check-ignore --stdin -z` 批量判断（一次子进程），失败就静默返回：
// 不是 git 仓库、没装 git 都只是少了标记，不影响文件本身可见。
func (s *Server) markIgnored(root string, entries []wsEntry) {
	if len(entries) == 0 {
		return
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	out, code, err := s.wsGitRawInput(root, strings.Join(paths, "\x00")+"\x00", "check-ignore", "--stdin", "-z")
	if err != nil || (code != 0 && code != 1) {
		return
	}
	if out == "" {
		return
	}
	ignored := make(map[string]bool, len(entries))
	for _, p := range strings.Split(out, "\x00") {
		if p = strings.TrimSpace(p); p != "" {
			ignored[filepath.ToSlash(p)] = true
		}
	}
	for i := range entries {
		// 已经因 wsSkipDirs 打了「忽略」的目录不再叠一个 git 忽略标记。
		if !entries[i].Skip && ignored[entries[i].Path] {
			entries[i].Ignored = true
		}
	}
}

// handleWorkspaceDownload 下载文件：GET /api/sessions/{id}/workspace/download?path=<相对路径>
//
// 与预览不同，下载不截断、不拒绝二进制，交给浏览器直接存盘。走 http.ServeContent
// 以支持 Range / If-Modified-Since，大文件不会一次性读进内存。鉴权与其它
// /api/* 一样由 mux 上的 s.auth 覆盖（cookie 或 ?token=）。
func (s *Server) handleWorkspaceDownload(w http.ResponseWriter, r *http.Request) {
	_, root, ok := s.workspaceSession(w, r)
	if !ok {
		return
	}
	rel := r.URL.Query().Get("path")
	if rel == "" {
		writeErr(w, http.StatusBadRequest, "需要文件路径")
		return
	}
	resolved, err := s.wsResolve(root, rel)
	if err != nil {
		wsWriteResolveError(w, err)
		return
	}
	info, err := os.Stat(resolved)
	if err != nil {
		writeErr(w, http.StatusNotFound, "文件不存在")
		return
	}
	if info.IsDir() {
		writeErr(w, http.StatusBadRequest, "目标是目录")
		return
	}
	if !info.Mode().IsRegular() {
		writeErr(w, http.StatusBadRequest, "非常规文件")
		return
	}
	f, err := os.Open(resolved)
	if err != nil {
		writeErr(w, http.StatusForbidden, "文件不可读")
		return
	}
	defer f.Close()
	name := filepath.Base(resolved)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// handleWorkspaceFile 读文件：GET /api/sessions/{id}/workspace/file?path=<相对路径>
//
// 二进制只回标记不回内容（面板只预览文本/代码/图片）。图片走既有的 /api/image。
func (s *Server) handleWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	_, root, ok := s.workspaceSession(w, r)
	if !ok {
		return
	}
	rel := r.URL.Query().Get("path")
	if rel == "" {
		writeErr(w, http.StatusBadRequest, "需要文件路径")
		return
	}
	resolved, err := s.wsResolve(root, rel)
	if err != nil {
		wsWriteResolveError(w, err)
		return
	}
	info, err := os.Stat(resolved)
	if err != nil {
		writeErr(w, http.StatusNotFound, "文件不存在")
		return
	}
	if info.IsDir() {
		writeErr(w, http.StatusBadRequest, "目标是目录")
		return
	}
	if !info.Mode().IsRegular() {
		writeErr(w, http.StatusBadRequest, "非常规文件")
		return
	}
	f, err := os.Open(resolved)
	if err != nil {
		writeErr(w, http.StatusForbidden, "文件不可读")
		return
	}
	defer f.Close()
	buf := make([]byte, wsMaxFileBytes+1)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		writeErr(w, http.StatusForbidden, "文件不可读")
		return
	}
	data := buf[:n]
	truncated := n > wsMaxFileBytes
	if truncated {
		data = data[:wsMaxFileBytes]
	}
	binary := false
	for _, b := range data {
		if b == 0 {
			binary = true
			break
		}
	}
	out := map[string]any{
		"path":      filepath.ToSlash(rel),
		"abs":       resolved,
		"name":      filepath.Base(resolved),
		"size":      info.Size(),
		"mtime":     info.ModTime().Unix(),
		"truncated": truncated,
		"binary":    binary,
	}
	if !binary {
		out["content"] = string(data)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleWorkspaceStatus 列 git 改动：GET /api/sessions/{id}/workspace/status
//
// 给文件树打「这个文件被改过」的标记用。解析 `git status --porcelain` 的
// "XY path" 行；重命名行是 "R  old -> new"，取新名字。
func (s *Server) handleWorkspaceStatus(w http.ResponseWriter, r *http.Request) {
	_, root, ok := s.workspaceSession(w, r)
	if !ok {
		return
	}
	changes, isRepo, err := s.wsGitStatus(root)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"repo": isRepo, "changes": map[string]string{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": isRepo, "changes": changes})
}

// wsGitStatus 解析 git status --porcelain，返回 path -> 状态字母。
// 不在 git 仓库里时 isRepo=false、changes 为空（不是错误）。
func (s *Server) wsGitStatus(root string) (map[string]string, bool, error) {
	out, err := s.wsGit(root, "status", "--porcelain")
	if err != nil {
		return map[string]string{}, false, err
	}
	changes := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		code := strings.TrimSpace(line[:2])
		path := strings.TrimSpace(line[3:])
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		path = strings.Trim(path, `"`)
		if path == "" {
			continue
		}
		changes[filepath.ToSlash(path)] = code
	}
	return changes, true, nil
}

// handleWorkspaceDiff 看某个文件的改动：GET /api/sessions/{id}/workspace/diff?path=
//
// 未暂存的改动 + 已暂存的改动都返回；未跟踪文件（??）用 diff --no-index
// 把内容当新增整段显示，否则新文件在面板里「没有 diff」，看着像没改过。
func (s *Server) handleWorkspaceDiff(w http.ResponseWriter, r *http.Request) {
	_, root, ok := s.workspaceSession(w, r)
	if !ok {
		return
	}
	rel := r.URL.Query().Get("path")
	if rel == "" {
		writeErr(w, http.StatusBadRequest, "需要文件路径")
		return
	}
	resolved, err := s.wsResolve(root, rel)
	if err != nil {
		wsWriteResolveError(w, err)
		return
	}
	if _, err := os.Stat(resolved); err != nil {
		writeErr(w, http.StatusNotFound, "文件不存在")
		return
	}
	relSlash := filepath.ToSlash(rel)

	unstaged, err := s.wsGit(root, "diff", "--no-color", "--", relSlash)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"repo": false, "diff": "", "unstaged": "", "staged": ""})
		return
	}
	staged, _ := s.wsGit(root, "diff", "--cached", "--no-color", "--", relSlash)

	untracked := false
	if unstaged == "" && staged == "" {
		changes, isRepo, _ := s.wsGitStatus(root)
		if isRepo {
			if code, hit := changes[relSlash]; hit && strings.HasPrefix(code, "?") {
				untracked = true
				// --no-index 发现差异时退出码是 1，不是失败：这里要的就是那份输出。
				if d, _, err := s.wsGitRaw(root, "diff", "--no-color", "--no-index", "/dev/null", resolved); err == nil {
					unstaged = d
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"repo":      true,
		"path":      relSlash,
		"unstaged":  unstaged,
		"staged":    staged,
		"diff":      unstaged,
		"untracked": untracked,
	})
}

// handleWorkspaceOpen 用系统默认程序打开文件：POST /api/sessions/{id}/workspace/open
//
// 面板里的「打开」按钮：把文件丢给系统的默认处理器（macOS 的 open /
// Linux 的 xdg-open），用户在编辑器里接着改。
func (s *Server) handleWorkspaceOpen(w http.ResponseWriter, r *http.Request) {
	_, root, ok := s.workspaceSession(w, r)
	if !ok {
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效")
		return
	}
	if body.Path == "" {
		writeErr(w, http.StatusBadRequest, "需要文件路径")
		return
	}
	resolved, err := s.wsResolve(root, body.Path)
	if err != nil {
		wsWriteResolveError(w, err)
		return
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		writeErr(w, http.StatusNotFound, "文件不存在")
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", resolved)
	case "linux":
		cmd = exec.Command("xdg-open", resolved)
	default:
		writeErr(w, http.StatusNotImplemented, "当前系统不支持打开文件")
		return
	}
	cmd.Dir = root
	if err := cmd.Start(); err != nil {
		writeErr(w, http.StatusInternalServerError, "打开失败："+err.Error())
		return
	}
	go func() { _ = cmd.Wait() }()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// wsGit 在 root 里跑一条 git 命令，返回 stdout。
// 不在仓库里时返回错误（调用方据此把 repo 置 false，而不是当成失败）。
func (s *Server) wsGit(root string, args ...string) (string, error) {
	out, code, err := s.wsGitRaw(root, args...)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("git %s: exit status %d", strings.Join(args, " "), code)
	}
	return out, nil
}

// wsGitRaw 跑 git 并把退出码也带回来（--no-index 找到差异时退出码是 1，不是失败）。
func (s *Server) wsGitRaw(root string, args ...string) (string, int, error) {
	return s.wsGitRawInput(root, "", args...)
}

// wsGitRawInput 与 wsGitRaw 相同，但把 stdin 喂给 git（check-ignore --stdin 用）。
func (s *Server) wsGitRawInput(root, stdin string, args ...string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wsGitTimeout)
	defer cancel()
	full := append([]string{"-C", root}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_OPTIONAL_LOCKS=0", // 只读命令，别跟用户的 git 操作抢索引锁
		"LC_ALL=C",             // 输出稳定，别跟系统语言走
	)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return stdout.String(), exitErr.ExitCode(), nil
		}
		return "", 0, fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), 0, nil
}
