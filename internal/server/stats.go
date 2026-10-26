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
	"cc-webui/internal/codexhist"
	"cc-webui/internal/config"
	"cc-webui/internal/mcodehist"
	"cc-webui/internal/opencodehist"
	"cc-webui/internal/opencodev2"
	"cc-webui/internal/opencodever"
	"cc-webui/internal/runner"
	"cc-webui/internal/statsagg"
)

var (
	statsMu              sync.Mutex
	claudeStatsCache     = claudehist.NewStatsCache()
	opencodeStatsCache   = opencodehist.NewStatsCache()
	opencodeStatsCacheV2 = opencodev2.NewStatsCache()
	mcodeStatsCache      = mcodehist.NewStatsCache()
	// qoder reads the same transcript format as claude, from its own home
	// directory, and reports under its own engine kind.
	qoderStatsCache = claudehist.NewStatsCacheFor(config.KindQoder)
)

// handleStats 返回全部会话（claude、opencode、mcode、qoder 四引擎）的使用统计。
// 各引擎各自增量聚合后合并成一份 Stats：claude/qoder 侧只重扫 mtime/size 变了
// 的 jsonl（同一个 reader，两个 home 目录、两个 engine kind），opencode 侧只重
// 导出 `updated` 前进的会话，mcode 侧只重解析指纹变了的 messages.jsonl。
// ?fresh=1 清空缓存强制全量重扫。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	fresh := r.URL.Query().Get("fresh") == "1"
	statsMu.Lock()
	fas := make([]statsagg.FileAgg, 0, 128)
	fas = append(fas, claudeStatsCache.Refresh(claudehist.HistoryDir(""), fresh)...)
	if v1Paths, v2Paths, dirs := s.opencodeStatsTargets(); len(v1Paths)+len(v2Paths) > 0 {
		if len(v1Paths) > 0 {
			fas = append(fas, opencodeStatsCache.Refresh(v1Paths, dirs, fresh)...)
		}
		if len(v2Paths) > 0 {
			fas = append(fas, opencodeStatsCacheV2.Refresh(v2Paths, dirs, fresh)...)
		}
	}
	// mcode keeps one store under its runtime data dir, shared by every
	// configured mcode cli, so a single configured cli is enough to include
	// the engine in the stats at all.
	if s.hasMcodeCLI() {
		fas = append(fas, mcodeStatsCache.Refresh(fresh)...)
	}
	// codex 的 home 可以按 cli 不同（ducx 在 ~/.baidu-cx，原生 codex 在
	// ~/.codex），所以按 cli 声明的 home 去重后各扫一份。
	for _, home := range s.codexStatsHomes() {
		fas = append(fas, s.codexStatsCache(home).Refresh(fresh)...)
	}
	// qoder 的 home dir 是 per-installation（~/.qoder-cn），所以和 mcode 一样
	// 按「有没有配 qoder cli」决定是否扫；模型名另有一层修正，见 qoderModelHints。
	if s.hasQoderCLI() {
		fas = append(fas, qoderStatsCache.RefreshWithModels(
			claudehist.HistoryDirIn(claudehist.QoderProfile, ""), s.qoderModelHints(), fresh)...)
	}
	// Shadow group-member sessions run the group's turns under their own
	// engine session ids — drop those aggregates so group activity isn't
	// double-counted as individual sessions (the group log itself is not
	// scanned, so the group as such stays out of stats for now).
	skip := map[string]bool{}
	for _, sess := range s.st.List() {
		if sess.GroupID == "" {
			continue
		}
		if sess.ClaudeSessionID != "" {
			skip[sess.ClaudeSessionID] = true
		}
		for _, prev := range sess.PreviousClaudeSessionIDs {
			skip[prev] = true
		}
	}
	if len(skip) > 0 {
		kept := fas[:0]
		for _, fa := range fas {
			if !skip[fa.FileID] {
				kept = append(kept, fa)
			}
		}
		fas = kept
	}
	stats := statsagg.Recompute(fas)
	statsMu.Unlock()
	writeJSON(w, http.StatusOK, stats)
}

// qoderModelHints 返回 session id -> 模型名，用来盖掉 qoder transcript 里记的
// 内部 key（"qfmodel"，CLI 归一化后的值，磁盘上从头到尾没有显示名）。cc-webui
// 知道每个托管会话是以哪个模型起的（spawn 的 --model 就来自这里），所以按模型
// 分桶时用真名。没被 cc-webui 托管、也没显式选过模型的会话无从得知，退回
// transcript 自己的值。
func (s *Server) qoderModelHints() map[string]string {
	cfg := s.cfg()
	hints := map[string]string{}
	for _, sess := range s.st.List() {
		if sess.ClaudeSessionID == "" || s.engineOf(sess) != config.KindQoder {
			continue
		}
		model := sess.ModelOverride
		if model == "" {
			if p := cfg.ProviderByName(sess.Provider); p != nil {
				model = p.DefaultModelName()
			}
		}
		if model != "" {
			hints[sess.ClaudeSessionID] = model
		}
	}
	return hints
}

// codexStatsHomes returns the distinct codex homes of the configured clis.
func (s *Server) codexStatsHomes() []string {
	seen := map[string]bool{}
	var homes []string
	for i := range s.cfg().CLIs {
		cli := &s.cfg().CLIs[i]
		if !cli.IsCodex() {
			continue
		}
		h := codexHome(cli)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		homes = append(homes, h)
	}
	return homes
}

// codexStatsCache returns the incremental cache for one codex home, creating
// it on first use. Callers hold statsMu.
func (s *Server) codexStatsCache(home string) *codexhist.StatsCache {
	if s.codexCaches == nil {
		s.codexCaches = map[string]*codexhist.StatsCache{}
	}
	if c, ok := s.codexCaches[home]; ok {
		return c
	}
	c := codexhist.NewStatsCache(home)
	s.codexCaches[home] = c
	return c
}

// hasMcodeCLI reports whether any configured cli uses the mcode engine.
func (s *Server) hasMcodeCLI() bool {
	cfg := s.cfg()
	for i := range cfg.CLIs {
		if cfg.CLIs[i].IsMcode() {
			return true
		}
	}
	return false
}

// hasQoderCLI reports whether any configured cli uses the qoder engine.
func (s *Server) hasQoderCLI() bool {
	cfg := s.cfg()
	for i := range cfg.CLIs {
		if cfg.CLIs[i].IsQoder() {
			return true
		}
	}
	return false
}

// opencodeStatsTargets 返回统计要用的 opencode cli 路径与项目桶目录候选。
// 正常路径一条只读 `opencode db` SELECT 就能枚举全部项目（见 opencodehist.ListAll），
// 桶目录只是那条查询失败时的兜底——`session list` 只返回进程 cwd 所属的项目桶，
// 而 CLI 没有"列出全部桶"的入口。所以桶集合从 cc-webui 已知的 cwd 推导：服务器
// 自身 cwd + 全部已管理会话的 cwd（归一到 git 仓库根，顺带保留 cwd 本身——它也
// 可能是嵌套仓库的桶根）。
func (s *Server) opencodeStatsTargets() (v1Paths, v2Paths []string, bucketDirs []string) {
	cfg := s.cfg()
	seenPath := map[string]bool{}
	for i := range cfg.CLIs {
		cli := &cfg.CLIs[i]
		if !cli.IsOpenCode() || strings.TrimSpace(cli.Path) == "" || seenPath[cli.Path] {
			continue
		}
		seenPath[cli.Path] = true
		if opencodever.IsV2(cli.Path, cli.OpenCodeAPI) {
			v2Paths = append(v2Paths, cli.Path)
		} else {
			v1Paths = append(v1Paths, cli.Path)
		}
	}
	if len(v1Paths)+len(v2Paths) == 0 {
		return nil, nil, nil
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
	return v1Paths, v2Paths, bucketDirs
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
