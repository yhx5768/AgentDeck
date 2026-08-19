package server

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// 预览上限 2 MiB，超出截断返回
const maxFileBytes = 2 << 20

// handleReadFile 只读返回本地文件内容。
// 安全：必须在任一会话 workspace（cwd）之内，且解析符号链接后仍不越界。
func (s *Server) handleReadFile(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" || !filepath.IsAbs(p) {
		writeErr(w, http.StatusBadRequest, "需要绝对路径")
		return
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(p))
	if err != nil {
		writeErr(w, http.StatusNotFound, "文件不存在")
		return
	}
	if !s.inWorkspace(resolved) {
		writeErr(w, http.StatusForbidden, "仅允许读取会话工作区内的文件")
		return
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() {
		writeErr(w, http.StatusNotFound, "文件不存在")
		return
	}
	if !info.Mode().IsRegular() {
		writeErr(w, http.StatusBadRequest, "非常规文件")
		return
	}
	f, err := os.Open(resolved)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取失败")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取失败")
		return
	}
	truncated := len(data) > maxFileBytes
	if truncated {
		data = data[:maxFileBytes]
	}
	if bytes.IndexByte(data, 0) >= 0 {
		writeErr(w, http.StatusUnsupportedMediaType, "二进制文件不支持预览")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":      resolved,
		"content":   string(data),
		"truncated": truncated,
	})
}

// inWorkspace 判断 p 是否位于任一会话（含归档）的 cwd 之下。
// root 同样做符号链接解析，防止通过 symlink 逃逸白名单。
func (s *Server) inWorkspace(p string) bool {
	sep := string(os.PathSeparator)
	for _, sess := range s.st.List() {
		root := sess.Cwd
		if root == "" {
			continue
		}
		if r, err := filepath.EvalSymlinks(filepath.Clean(root)); err == nil {
			root = r
		}
		if p == root || strings.HasPrefix(p, root+sep) {
			return true
		}
	}
	return false
}
