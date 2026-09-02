package server

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"cc-webui/internal/claudehist"
)

var (
	statsMu    sync.Mutex
	statsCache = claudehist.NewStatsCache()
)

// handleStats 返回全部 on-disk 会话的使用统计。增量聚合：仅重新解析自上次
// 扫描以来 mtime/size 变化的会话文件，其余复用缓存的逐文件聚合再合并——
// 打开弹窗通常只重扫当前活跃会话一个文件。?fresh=1 清空逐文件缓存强制全量重扫。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	fresh := r.URL.Query().Get("fresh") == "1"
	statsMu.Lock()
	stats := statsCache.Refresh(claudehist.HistoryDir(""), fresh)
	statsMu.Unlock()
	writeJSON(w, http.StatusOK, stats)
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
