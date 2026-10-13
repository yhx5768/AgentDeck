package server

import (
	"encoding/json"
	"net/http"

	"cc-webui/internal/config"
	"cc-webui/internal/store"
)

// handleOpenCodeRewind rolls an opencode session back to one of its messages.
//
// Unlike the claude path there is no cc-webui-side transcript to truncate:
// the replay path renders from `opencode export`, and opencode's own revert
// rolls its own store back. So this handler is a thin bridge to
// /api/session/{id}/revert/stage + /revert/commit, plus the same
// LockForRewind guard and the same response shape the frontend already parses
// ({ok, draft_text, files}).
//
// The target is addressed by the opencode message id (msg_…) rather than
// claude's checkpoint uuid — opencode sessions never carry one.
func (s *Server) handleOpenCodeRewind(w http.ResponseWriter, r *http.Request, id string, sess *store.Session) {
	var body struct {
		MessageID  string `json:"message_id"`
		Checkpoint string `json:"checkpoint"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	// The frontend addresses a user bubble by whatever id it was rendered
	// with; accept both spellings so no frontend change is needed.
	messageID := body.MessageID
	if messageID == "" {
		messageID = body.Checkpoint
	}
	if messageID == "" {
		writeErr(w, http.StatusBadRequest, "message_id is required")
		return
	}
	if !s.mgr.LockForRewind(id) {
		writeErr(w, http.StatusConflict, "session is running")
		return
	}
	defer s.mgr.UnlockFromRewind(id)

	if sess.ClaudeSessionID == "" {
		writeErr(w, http.StatusBadRequest, "会话还没有 opencode session id")
		return
	}
	cli := s.cfg().CLIByName(sess.CLI)
	prov := s.cfg().ProviderByName(sess.Provider)
	if cli == nil || prov == nil {
		writeErr(w, http.StatusBadRequest, "cli or provider not found")
		return
	}

	// Recover the rolled-back message text before reverting, so the composer
	// can be refilled with it (same contract as the claude path).
	draftText := s.openCodeUserText(id, messageID)

	res, err := s.mgr.RewindOpenCode(sess, cli, prov, messageID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "opencode 回滚失败："+err.Error())
		return
	}

	rewindEv, err := s.st.AppendEvent(id, "rewind", map[string]any{
		"checkpoint": messageID,
		"draft_text": draftText,
		"ok":         true,
		"detail":     res,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mgr.BroadcastEvent(id, rewindEv)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"draft_text": draftText,
		"files":      res,
	})
}

// openCodeUserText finds the user prompt text of one cc-webui user event,
// addressed by the checkpoint id the rewind button carries (the opencode
// message id stamped onto the event).
func (s *Server) openCodeUserText(sessionID, checkpoint string) string {
	events, err := s.st.Events(sessionID)
	if err != nil {
		return ""
	}
	for _, ev := range events {
		if ev.Kind != "user" {
			continue
		}
		var d struct {
			Text       string `json:"text"`
			Checkpoint string `json:"checkpoint"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		if d.Checkpoint == checkpoint {
			return d.Text
		}
	}
	return ""
}

// openCodeSupportsCards reports whether this engine shows approval /
// AskUserQuestion cards. opencode gained them when its engine moved to the
// `opencode serve` surface; mcode still has no headless approver.
func openCodeSupportsCards(engine string) bool {
	return engine != config.KindMcode
}
