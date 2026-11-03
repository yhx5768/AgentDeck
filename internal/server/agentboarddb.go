package server

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"cc-webui/internal/boarddb"
)

// SQLite board mode (YHX-TODO-821). When agent_board_db_path is configured the
// DB is the sole write source: the legacy JSON file and routing table are
// imported once at startup and never written again. Reads and writes below all
// go through boarddb, and every successful write broadcasts the same SSE event
// the JSON watcher used to produce.

func (s *Server) agentBoardDBConfigured() bool {
	return strings.TrimSpace(s.cfg().AgentBoardDBPath) != ""
}

// agentBoardDBMode is true once the server has committed to DB mode, or when
// the live config asks for it (which requires a restart). Checking the live
// config alone would flip back to JSON mode if someone removed the DB path
// during a hot reload while the DB connection is still open.
func (s *Server) agentBoardDBMode() bool {
	return s.boardDB != nil || s.boardDBErr != nil || s.agentBoardDBConfigured()
}

func (s *Server) initAgentBoardDB() {
	path := strings.TrimSpace(s.cfg().AgentBoardDBPath)
	st, err := boarddb.Open(path)
	if err != nil {
		s.boardDBErr = fmt.Errorf("open agent-board db: %w", err)
		log.Printf("agent-board: %v; board API disabled", s.boardDBErr)
		return
	}
	result, err := st.MigrateOnce(s.agentBoardPath(), strings.TrimSpace(s.cfg().AgentBoardRoutingTable))
	if err != nil {
		_ = st.Close()
		s.boardDBErr = fmt.Errorf("migrate agent-board db: %w", err)
		log.Printf("agent-board: %v; board API disabled", s.boardDBErr)
		return
	}
	s.boardDB = st
	if result.Migrated {
		log.Printf("agent-board: sqlite %s imported %d tasks / %d events from %s",
			path, result.Tasks, result.Events, result.Source)
	} else {
		log.Printf("agent-board: sqlite %s ready (%s)", path, result.Reason)
	}
}

func (s *Server) closeAgentBoardDB() {
	s.boardDBOnce.Do(func() {
		if s.boardDB != nil {
			_ = s.boardDB.Close()
		}
	})
}

func (s *Server) boardDBStore(w http.ResponseWriter) (*boarddb.Store, bool) {
	if s.boardDB != nil {
		return s.boardDB, true
	}
	if s.boardDBErr != nil {
		writeErr(w, http.StatusInternalServerError, "看板数据库不可用: "+s.boardDBErr.Error())
		return nil, false
	}
	writeErr(w, http.StatusConflict, "看板仍在 JSON 模式；请配置 agent_board_db_path 并重启")
	return nil, false
}

func (s *Server) handleAgentBoardDB(w http.ResponseWriter, r *http.Request) {
	st, ok := s.boardDBStore(w)
	if !ok {
		return
	}
	snapshot, err := st.Snapshot()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取看板数据库失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) handleAgentBoardGetTask(w http.ResponseWriter, r *http.Request) {
	st, ok := s.boardDBStore(w)
	if !ok {
		return
	}
	task, err := st.GetTask(r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "任务不存在: "+r.PathValue("id"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取任务失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "task": task})
}

func (s *Server) handleAgentBoardCreateTask(w http.ResponseWriter, r *http.Request) {
	st, ok := s.boardDBStore(w)
	if !ok {
		return
	}
	var body map[string]any
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	task, err := st.CreateTask(body)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			writeErr(w, http.StatusConflict, "任务已存在: "+fmt.Sprint(body["id"]))
			return
		}
		writeErr(w, http.StatusBadRequest, "创建任务失败: "+err.Error())
		return
	}
	s.broadcastAgentBoard()
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "task": task})
}

func (s *Server) handleAgentBoardUpdateTask(w http.ResponseWriter, r *http.Request) {
	st, ok := s.boardDBStore(w)
	if !ok {
		return
	}
	var patch map[string]any
	if err := decode(r, &patch); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if len(patch) == 0 {
		writeErr(w, http.StatusBadRequest, "没有要更新的字段")
		return
	}
	task, err := st.UpdateTask(r.PathValue("id"), patch)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "任务不存在: "+r.PathValue("id"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "更新任务失败: "+err.Error())
		return
	}
	s.broadcastAgentBoard()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "task": task})
}

func (s *Server) handleAgentBoardBindWorker(w http.ResponseWriter, r *http.Request) {
	st, ok := s.boardDBStore(w)
	if !ok {
		return
	}
	var body struct {
		Worker  string `json:"worker"`
		Session string `json:"session"`
		Cwd     string `json:"cwd"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	taskID := r.PathValue("id")
	err := st.BindWorker(taskID, body.Worker, body.Session, body.Cwd)
	if err != nil {
		if err.Error() == "task not found" {
			writeErr(w, http.StatusNotFound, "任务不存在: "+taskID)
			return
		}
		writeErr(w, http.StatusBadRequest, "绑定 worker 失败: "+err.Error())
		return
	}
	task, err := st.GetTask(taskID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取任务失败: "+err.Error())
		return
	}
	s.broadcastAgentBoard()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "task": task})
}

func (s *Server) handleAgentBoardWorkers(w http.ResponseWriter, r *http.Request) {
	st, ok := s.boardDBStore(w)
	if !ok {
		return
	}
	workers, err := st.Workers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取 worker 失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "workers": workers})
}

func (s *Server) handleAgentBoardBackup(w http.ResponseWriter, r *http.Request) {
	st, ok := s.boardDBStore(w)
	if !ok {
		return
	}
	path := st.Path()
	name := fmt.Sprintf("%s.backup-%s.sqlite", strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		time.Now().Format("20060102-150405.000"))
	dest := filepath.Join(filepath.Dir(path), name)
	if err := st.Backup(dest); err != nil {
		writeErr(w, http.StatusInternalServerError, "备份看板数据库失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": dest})
}

func (s *Server) handleAgentBoardAppendEventDB(w http.ResponseWriter, r *http.Request) {
	st, ok := s.boardDBStore(w)
	if !ok {
		return
	}
	taskID := r.PathValue("id")
	var body struct {
		Kind   string `json:"kind"`
		Text   string `json:"text"`
		At     string `json:"at"`
		Source string `json:"source"`
	}
	if err := decode(r, &body); err != nil {
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
	created, err := st.AppendEvent(taskID, body.Kind, body.Text, body.At, body.Source)
	if err != nil {
		if err.Error() == "task not found" {
			writeErr(w, http.StatusNotFound, "任务不存在: "+taskID)
			return
		}
		writeErr(w, http.StatusBadRequest, "写入事件失败: "+err.Error())
		return
	}
	if created {
		s.broadcastAgentBoard()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "task_id": taskID, "at": body.At, "duplicate": !created,
	})
}

func (s *Server) broadcastAgentBoard() {
	if s.boardHub != nil {
		s.boardHub.broadcast()
	}
}
