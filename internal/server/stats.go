package server

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cc-webui/internal/claudehist"
	"cc-webui/internal/opencodehist"
	"cc-webui/internal/runner"
	"cc-webui/internal/statsagg"
)

var (
	statsMu            sync.Mutex
	claudeStatsCache   = claudehist.NewStatsCache()
	opencodeStatsCache = opencodehist.NewStatsCache()
)

// handleStats 返回全部会话（claude 与 opencode 两引擎）的使用统计。两侧各自
// 增量聚合后合并成一份 Stats：claude 侧只重扫 mtime/size 变了的 jsonl，
// opencode 侧只重导出 `updated` 前进的会话。?fresh=1 清空两侧缓存强制全量重扫。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	fresh := r.URL.Query().Get("fresh") == "1"
	statsMu.Lock()
	fas := make([]statsagg.FileAgg, 0, 128)
	fas = append(fas, claudeStatsCache.Refresh(claudehist.HistoryDir(""), fresh)...)
	if cliPaths, dirs := s.opencodeStatsTargets(); len(cliPaths) > 0 {
		fas = append(fas, opencodeStatsCache.Refresh(cliPaths, dirs, fresh)...)
	}
	stats := statsagg.Recompute(fas)
	statsMu.Unlock()
	writeJSON(w, http.StatusOK, stats)
}

// opencodeStatsTargets 返回统计要用的 opencode cli 路径与项目桶目录候选。
// 正常路径一条只读 `opencode db` SELECT 就能枚举全部项目（见 opencodehist.ListAll），
// 桶目录只是那条查询失败时的兜底——`session list` 只返回进程 cwd 所属的项目桶，
// 而 CLI 没有"列出全部桶"的入口。所以桶集合从 cc-webui 已知的 cwd 推导：服务器
// 自身 cwd + 全部已管理会话的 cwd（归一到 git 仓库根，顺带保留 cwd 本身——它也
// 可能是嵌套仓库的桶根）。
func (s *Server) opencodeStatsTargets() (cliPaths []string, bucketDirs []string) {
	cfg := s.cfg()
	seenPath := map[string]bool{}
	for i := range cfg.CLIs {
		cli := &cfg.CLIs[i]
		if !cli.IsOpenCode() || strings.TrimSpace(cli.Path) == "" || seenPath[cli.Path] {
			continue
		}
		seenPath[cli.Path] = true
		cliPaths = append(cliPaths, cli.Path)
	}
	if len(cliPaths) == 0 {
		return nil, nil
	}

	var candidates []string
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	for _, sess := range s.st.List() {
		candidates = append(candidates, sess.Cwd)
	}

	seenDir := map[string]bool{}
	add := func(d string) {
		if d == "" || seenDir[d] {
			return
		}
		seenDir[d] = true
		bucketDirs = append(bucketDirs, d)
	}
	for _, raw := range candidates {
		d, err := runner.ResolveCwd(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		if abs, err := filepath.Abs(d); err == nil {
			d = abs
		}
		if _, err := os.Stat(d); err != nil {
			continue
		}
		add(gitRoot(d))
		add(d)
	}
	return cliPaths, bucketDirs
}

type rootEntry struct {
	root string
	at   time.Time
}

var rootCache = struct {
	sync.Mutex
	m map[string]rootEntry
}{m: map[string]rootEntry{}}

// gitRoot 取 cwd 所属 git 仓库根（opencode 按仓库分桶）：2s 超时并缓存 10 分钟，
// 非 git 仓库/超时返回 cwd 本身。
func gitRoot(cwd string) string {
	if cwd == "" {
		return cwd
	}
	rootCache.Lock()
	if e, ok := rootCache.m[cwd]; ok && time.Since(e.at) < 10*time.Minute {
		root := e.root
		rootCache.Unlock()
		return root
	}
	rootCache.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	root := cwd
	if err == nil {
		if r := strings.TrimSpace(string(out)); r != "" {
			root = r
		}
	}
	rootCache.Lock()
	rootCache.m[cwd] = rootEntry{root: root, at: time.Now()}
	rootCache.Unlock()
	return root
}

// handleClaudeSessionPreview 返回单个会话的导入预览：元信息 + 最近几条消息。
// branch 为惰性 git 查询（非 git 仓库/超时返回空串）。
func (s *Server) handleClaudeSessionPreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := strings.TrimSpace(q.Get("id"))
	cwd := strings.TrimSpace(q.Get("cwd"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if cwd == "" {
		// 客户端没给 cwd（旧数据）时按 id 在全部项目里定位
		if list, err := claudehist.List(""); err == nil {
			for _, it := range list {
				if it.SessionID == id {
					cwd = it.Cwd
					break
				}
			}
		}
	}
	path := claudehist.HistoryPath(cwd, id)
	fi, err := os.Stat(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "claude session history not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": id,
		"cwd":        cwd,
		"branch":     gitBranch(cwd),
		"updated_at": fi.ModTime(),
		"messages":   claudehist.Preview(cwd, id, 8),
	})
}

type branchEntry struct {
	branch string
	at     time.Time
}

var branchCache = struct {
	sync.Mutex
	m map[string]branchEntry
}{m: map[string]branchEntry{}}

// gitBranch 取 cwd 当前 git 分支：2s 超时并缓存 10 分钟，非 git 仓库返回 ""。
func gitBranch(cwd string) string {
	if cwd == "" {
		return ""
	}
	branchCache.Lock()
	if e, ok := branchCache.m[cwd]; ok && time.Since(e.at) < 10*time.Minute {
		branch := e.branch
		branchCache.Unlock()
		return branch
	}
	branchCache.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD").Output()
	branch := ""
	if err == nil {
		branch = strings.TrimSpace(string(out))
	}
	branchCache.Lock()
	branchCache.m[cwd] = branchEntry{branch: branch, at: time.Now()}
	branchCache.Unlock()
	return branch
}
