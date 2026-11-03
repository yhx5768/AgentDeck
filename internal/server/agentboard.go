package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// boardHub fans out "the local board file changed" notifications to SSE
// clients. The payload is intentionally tiny: the frontend re-fetches
// /api/agent-board, so a large board is not copied through every connection.
type boardHub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func newBoardHub() *boardHub {
	return &boardHub{subs: map[chan struct{}]struct{}{}}
}

func (h *boardHub) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *boardHub) unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *boardHub) broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// agentBoardPath is the local JSON store read by the board. It never points at
// an iCafe API; an optional importer may have written the file earlier.
func (s *Server) agentBoardPath() string {
	path := s.cfg().AgentBoardPath
	if path == "" {
		path = filepath.Join(s.cfg().DataDir, "agent-board.local.json")
	}
	return path
}

// watchAgentBoard polls the local store's metadata once per second and
// broadcasts when it changes. It deliberately does not parse or poll iCafe:
// "real time" here means local task file/event changes, not a 10/30 minute
// snapshot job. One second is the latency bound; the frontend gets an SSE
// refresher and re-reads the file immediately.
func (s *Server) watchAgentBoard() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		path := s.agentBoardPath()
		info, err := os.Stat(path)
		var mod time.Time
		var size int64
		if err == nil {
			mod = info.ModTime()
			size = info.Size()
		}
		s.boardMu.Lock()
		changed := path != s.boardLastPath || !mod.Equal(s.boardLastMod) || size != s.boardLastSize
		if changed {
			s.boardLastPath = path
			s.boardLastMod = mod
			s.boardLastSize = size
		}
		s.boardMu.Unlock()
		if changed {
			s.boardHub.broadcast()
		}
	}
}

func (s *Server) handleAgentBoard(w http.ResponseWriter, r *http.Request) {
	path := s.agentBoardPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		writeJSON(w, http.StatusOK, map[string]any{
			"generated_at": "",
			"source":       "missing",
			"tasks":        []any{},
			"projects":     []any{},
			"counts":       map[string]int{},
			"error":        "本地看板 store 不存在：" + path,
		})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取看板 store 失败: "+err.Error())
		return
	}
	// Validate before serving: a truncated snapshot should surface as an error
	// in the board, not as a JSON parse failure in the browser.
	var payload any
	if err := json.Unmarshal(data, &payload); err != nil {
		writeErr(w, http.StatusInternalServerError, "看板 store JSON 无效: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleAgentBoardAppendEvent appends one event to a task timeline. The board
// is otherwise read-only; keeping this write in the server avoids races
// between browser tabs and gives external writers one documented path. The
// JSON file stays the source of truth until the task store moves to a DB.
func (s *Server) handleAgentBoardAppendEvent(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	var body struct {
		Kind   string `json:"kind"`
		Text   string `json:"text"`
		At     string `json:"at"`
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	body.Text = strings.TrimSpace(body.Text)
	if body.Text == "" {
		writeErr(w, http.StatusBadRequest, "事件正文不能为空")
		return
	}
	if body.Kind == "" {
		body.Kind = "用户消息"
	}
	if body.Source == "" {
		body.Source = "drawer"
	}
	if body.At == "" {
		body.At = time.Now().Format("2006-01-02 15:04:05")
	}

	s.boardWriteMu.Lock()
	defer s.boardWriteMu.Unlock()

	path := s.agentBoardPath()
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		writeErr(w, http.StatusNotFound, "看板 store 不存在："+path)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取看板 store 失败: "+err.Error())
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		writeErr(w, http.StatusInternalServerError, "看板 store JSON 无效: "+err.Error())
		return
	}
	tasks, _ := payload["tasks"].([]any)
	var task map[string]any
	for _, item := range tasks {
		candidate, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if fmt.Sprint(candidate["id"]) == taskID {
			task = candidate
			break
		}
	}
	if task == nil {
		writeErr(w, http.StatusNotFound, "任务不存在: "+taskID)
		return
	}
	timeline, _ := task["timeline"].([]any)
	for _, item := range timeline {
		existing, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if fmt.Sprint(existing["at"]) == body.At &&
			fmt.Sprint(existing["kind"]) == body.Kind &&
			fmt.Sprint(existing["text"]) == body.Text {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "task_id": taskID, "at": body.At, "duplicate": true,
			})
			return
		}
	}
	event := map[string]any{
		"at":     body.At,
		"kind":   body.Kind,
		"text":   body.Text,
		"source": body.Source,
	}
	updated := make([]any, 0, len(timeline)+1)
	updated = append(updated, event)
	updated = append(updated, timeline...)
	if len(updated) > 100 {
		updated = updated[:100]
	}
	task["timeline"] = updated
	task["updated_at"] = body.At

	out, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "编码看板 store 失败: "+err.Error())
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, "写入看板 store 失败: "+err.Error())
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		writeErr(w, http.StatusInternalServerError, "替换看板 store 失败: "+err.Error())
		return
	}
	if s.boardHub != nil {
		s.boardHub.broadcast()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "task_id": taskID, "at": body.At,
	})
}

// handleAgentBoardEvents streams a refresh trigger whenever the local store
// file changes. The browser re-fetches /api/agent-board on each event; this
// keeps the SSE payload constant-sized and avoids stale full-board copies.
func (s *Server) handleAgentBoardEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "SSE not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := s.boardHub.subscribe()
	defer s.boardHub.unsubscribe(ch)
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			fmt.Fprint(w, "event: board\ndata: {\"changed\":true}\n\n")
			flusher.Flush()
		}
	}
}
