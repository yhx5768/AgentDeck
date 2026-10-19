package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
//   ① 必须在任一会话 workspace（cwd）之内，且路径先经 EvalSymlinks 解析，
//      防 symlink 逃逸；
//   ② 扩展名必须在图片白名单里（见 imageMIMEs）；
//   ③ 体积上限 10 MiB；
//   ④ 响应带 nosniff，不让浏览器自己猜类型。
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
