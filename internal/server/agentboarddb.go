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

// SQLite board store (YHX-TODO-821). The DB is always <data_dir>/agent-board.db;
// there is no board configuration and no file import: the board is DB-native,
// read and written through boarddb (the HTTP API and the agentdeck MCP board
// tools). Reads and writes all go through boarddb.

func (s *Server) initAgentBoardDB() {
	path := filepath.Join(s.cfg().DataDir, "agent-board.db")
	st, err := boarddb.Open(path)
	if err != nil {
		s.boardDBErr = fmt.Errorf("open agent-board db: %w", err)
		log.Printf("agent-board: %v; board API disabled", s.boardDBErr)
		return
	}
	s.boardDB = st
	log.Printf("agent-board: sqlite %s ready", path)
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
	message := "看板数据库未初始化"
	if s.boardDBErr != nil {
		message = "看板数据库不可用: " + s.boardDBErr.Error()
	}
	writeErr(w, http.StatusInternalServerError, message)
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
	s.decorateBoardSnapshot(r, snapshot)
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
	s.decorateTaskArtifacts(r, task)
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
	s.decorateTaskArtifacts(r, task)
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
	s.decorateTaskArtifacts(r, task)
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
	s.decorateTaskArtifacts(r, task)
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
	// A board-source turn is written back automatically. If the model also
	// posts the same final text through board_comment, keep one copy instead
	// of two. (Different text — a summary or a supplement — is still allowed.)
	// Only model-authored comments are considered: a user message from the
	// drawer (source=drawer, kind=用户消息) may legitimately repeat the text.
	if body.Source == "agentdeck" || body.Kind == "主 Agent 回复" {
		if dup, err := st.HasAutoReplyText(taskID, body.Text); err == nil && dup {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "task_id": taskID, "at": body.At, "duplicate": true,
				"deduped_by": "auto_reply",
			})
			return
		}
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
