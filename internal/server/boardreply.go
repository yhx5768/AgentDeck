package server

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"cc-webui/internal/runner"
	"cc-webui/internal/store"
)

// boardTaskForMessage validates a structured board_task_id sent with a user
// message. The board drawer only exists on main-agent sessions, and the card
// must exist, otherwise a typo would silently bind the turn to nothing while
// the user waits for a reply that can never be written back.
func (s *Server) boardTaskForMessage(sess *store.Session, raw string) (string, int, error) {
	taskID := strings.TrimSpace(raw)
	if taskID == "" {
		return "", 0, nil
	}
	if sess == nil || !sess.MainAgent {
		return "", http.StatusBadRequest, fmt.Errorf("board_task_id 仅主 Agent 会话可用")
	}
	if s.boardDB == nil {
		return "", http.StatusInternalServerError, fmt.Errorf("看板数据库未初始化")
	}
	if _, err := s.boardDB.GetTask(taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", http.StatusNotFound, fmt.Errorf("任务不存在: %s", taskID)
		}
		return "", http.StatusInternalServerError, fmt.Errorf("读取看板任务失败: %w", err)
	}
	return taskID, 0, nil
}

// onBoardTurnFinal writes a board-bound turn's final assistant reply to its
// card timeline. It is called from a runner goroutine after a clean turn end.
//
// Idempotency:
//   - the append key is derived from the persisted user event seq
//     (session#seq), so duplicate callbacks, reconnects and process restarts
//     cannot append twice;
//   - if the model already posted the identical text through board_comment,
//     the automatic append is skipped (the manual handler performs the mirror
//     check against our source="turn" events).
func (s *Server) onBoardTurnFinal(t runner.TurnFinal) {
	taskID := strings.TrimSpace(t.BoardTaskID)
	if taskID == "" || strings.TrimSpace(t.Text) == "" {
		return
	}
	st := s.boardDB
	if st == nil {
		return
	}
	if dup, err := st.HasManualReplyText(taskID, t.Text); err == nil && dup {
		log.Printf("agent-board: turn %s reply already posted manually on %s; skipping auto writeback", t.TurnID, taskID)
		return
	}
	data := map[string]any{
		"auto":     true,
		"source":   "turn",
		"turn_id":  t.TurnID,
		"session":  t.SessionID,
		"user_seq": t.UserSeq,
	}
	created, err := st.AppendEventOnce(taskID, "主 Agent 回复", t.Text, "", "turn",
		"turn-reply:"+t.TurnID, data)
	if err != nil {
		log.Printf("agent-board: write reply for turn %s to %s: %v", t.TurnID, taskID, err)
		return
	}
	if created {
		s.broadcastAgentBoard()
	}
}
