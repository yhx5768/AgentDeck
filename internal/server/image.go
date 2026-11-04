package server

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"cc-webui/internal/runner"
)

// 图片上限 10 MiB：比文本预览的 2 MiB 宽，但仍是有界的。
const maxImageBytes = 10 << 20

// 受支持的图片类型。**不含 svg**——svg 直接访问 URL 会被浏览器当文档渲染，
// 内嵌脚本就跟着跑；<img> 上下文里 svg 不执行脚本，但用户点开链接时是文档。
var imageMIMEs = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
}

// handleImage 按二进制原样返回会话工作区内的图片文件，供前端 <img> 直接引用。
//
// 为什么需要：模型在回复里写 ![](/abs/path/x.png) 时，浏览器会把它当站点相对
// URL 去请求 http://host/abs/path/x.png —— cc-webui 的静态服务只提供内嵌的 web/，
// 一律 404，图裂在消息里。文本预览接口 /api/files 也用不上：它限 2 MiB 且含
// NUL 字节就判「二进制文件不支持预览」，而图片正是二进制。
//
// 安全边界与 /api/files 同源（都在会话工作区内、只读），且类型更严：
//
//	① 必须在任一会话 workspace（cwd）之内，且路径先经 EvalSymlinks 解析，
//	   防 symlink 逃逸；
//	② 扩展名必须在图片白名单里（见 imageMIMEs）；
//	③ 体积上限 10 MiB；
//	④ 响应带 nosniff，不让浏览器自己猜类型。
func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
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
	ext := strings.ToLower(filepath.Ext(resolved))
	ct, ok := imageMIMEs[ext]
	if !ok {
		writeErr(w, http.StatusUnsupportedMediaType, "不是受支持的图片类型")
		return
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() {
		writeErr(w, http.StatusNotFound, "文件不存在")
		return
	}
	if info.Size() > maxImageBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "图片过大")
		return
	}
	f, err := os.Open(resolved)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取失败")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	io.Copy(w, f)
}

// handleSessionUpload serves an image from a session's own upload directory
// (data/uploads/<sid>/), for the <img> tags in user message bubbles. Pasted
// images are persisted there at send time and only their file names go into
// the stored user event, so a reload/replay can still show them.
//
// Why not /api/image: that one is scoped to the session workspace (cwd), and
// the uploads dir lives under data/, outside every workspace. The safety
// boundary here is tighter and self-contained: the name is a bare file name
// (sanitizeUploadName rejects separators), the resolved path must stay inside
// this session's uploads dir (symlink-escape check), the extension must be in
// the image allowlist, and the response carries nosniff.
func (s *Server) handleSessionUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.st.Get(id) == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	name, err := sanitizeUploadName(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid file name")
		return
	}
	dir := filepath.Join(s.cfg().DataDir, "uploads", id)
	resolved, err := filepath.EvalSymlinks(filepath.Join(dir, name))
	if err != nil {
		writeErr(w, http.StatusNotFound, "文件不存在")
		return
	}
	if rel, err := filepath.Rel(dir, resolved); err != nil ||
		rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		writeErr(w, http.StatusForbidden, "越权访问")
		return
	}
	ext := strings.ToLower(filepath.Ext(resolved))
	ct, ok := imageMIMEs[ext]
	if !ok {
		writeErr(w, http.StatusUnsupportedMediaType, "不是受支持的图片类型")
		return
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() {
		writeErr(w, http.StatusNotFound, "文件不存在")
		return
	}
	if info.Size() > maxImageBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "图片过大")
		return
	}
	f, err := os.Open(resolved)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取失败")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	io.Copy(w, f)
}

// persistMessageImages writes each inline image of an outgoing turn to the
// session's uploads dir and returns the stored file names, so the stored user
// event can reference them (see runner.Message.ImageNames) instead of carrying
// base64. Best-effort in the sense that a decode/size failure aborts the turn
// before anything is enqueued; files already written are harmless leftovers.
func (s *Server) persistMessageImages(sid string, images []runner.Image) ([]string, error) {
	if len(images) == 0 {
		return nil, nil
	}
	dir := filepath.Join(s.cfg().DataDir, "uploads", sid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(images))
	for _, im := range images {
		data, err := base64.StdEncoding.DecodeString(im.Data)
		if err != nil {
			return nil, fmt.Errorf("decode image: %w", err)
		}
		if len(data) > maxImageBytes {
			return nil, fmt.Errorf("image exceeds %dMB", maxImageBytes>>20)
		}
		stored, _, err := writeUploadExclusive(dir, "image"+imageExtForMediaType(im.MediaType), data)
		if err != nil {
			return nil, err
		}
		names = append(names, stored)
	}
	return names, nil
}

// imageExtForMediaType maps a browser media type to the file extension the
// upload is stored under. The frontend only sends the allowlisted types, so an
// unknown value falls back to .png (the stored file is served back with the
// extension's MIME, so a mismatch only ever means a slightly wrong suffix).
func imageExtForMediaType(mt string) string {
	switch strings.ToLower(strings.TrimSpace(mt)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	}
	return ".png"
}
