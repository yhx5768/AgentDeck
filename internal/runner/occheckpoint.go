package runner

import (
	"encoding/json"
	"log"

	"cc-webui/internal/store"
)

// captureOpenCodeCheckpoint records the opencode message id of the user turn
// that just ran, so the frontend's rewind button has something to address.
//
// This is the opencode counterpart of captureCheckpoint: claude identifies a
// rewind point by the uuid it wrote for the user prompt, and opencode
// identifies it by the msg_… id of the user message. Both land in the same
// `checkpoint` field of the user event, so the frontend needs no engine branch
// — it already renders a rewind button whenever that field is present.
//
// The user message id is known as soon as the SSE stream reports it
// (message.part.updated on a user-role part), which the turn records while
// consuming events; this runs after the prompt returns.
func (m *Manager) captureOpenCodeCheckpoint(sessionID string, t *openCodeTurn) {
	if t == nil || t.lastUserMessageID == "" {
		return
	}
	m.writeCheckpoint(sessionID, t.lastUserMessageID)
}

// writeCheckpoint stamps a checkpoint id onto the most recent user event and
// tells live clients to attach a rewind button to it.
func (m *Manager) writeCheckpoint(sessionID, checkpoint string) {
	if checkpoint == "" {
		return
	}
	events, err := m.st.Events(sessionID)
	if err != nil {
		return
	}
	var userSeq int64 = -1
	for _, ev := range events {
		if ev.Kind == "user" {
			userSeq = ev.Seq
		}
	}
	if userSeq < 0 {
		return
	}
	if _, err := m.st.UpdateEvent(sessionID, userSeq, func(ev *store.Event) {
		var d map[string]string
		if json.Unmarshal(ev.Data, &d) != nil {
			d = map[string]string{}
		}
		d["checkpoint"] = checkpoint
		raw, _ := json.Marshal(d)
		ev.Data = raw
	}); err != nil {
		log.Printf("capture opencode checkpoint: %v", err)
		return
	}
	m.broadcast(sessionID, "checkpoint", map[string]string{"checkpoint": checkpoint})
}
