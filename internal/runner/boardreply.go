package runner

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Board reply capture.
//
// A message sent from the board drawer carries a BoardTaskID. The runner
// persists that binding on the user event, then watches the turn's canonical
// assistant stream events. When the turn ends cleanly, the last assistant
// message that contains text is the user-visible final reply; it is handed to
// the server, which appends it to the card timeline verbatim (Markdown
// included).
//
// The capture is engine-neutral: it only reads the canonical assistant shape
// every engine is normalized onto (type=assistant, message.content[] with
// type=text blocks), and it de-duplicates repeated snapshots by message block
// id — opencode re-sends a growing text part, claude sends each complete
// assistant message once.

// TurnFinal is a cleanly finished board-bound turn. TurnID is stable across
// retries/restarts (session id + persisted user event seq, plus its timestamp
// so a rewind that reuses a seq cannot collide), so the server can use it as an
// idempotency key.
type TurnFinal struct {
	SessionID   string
	BoardTaskID string
	TurnID      string
	UserSeq     int64
	Text        string
}

// SetTurnObserver installs the final-turn callback. Safe to call while turns
// are in flight; the server calls it once after construction. A nil callback
// disables board writeback.
func (m *Manager) SetTurnObserver(fn func(TurnFinal)) {
	m.notifyMu.Lock()
	m.turnFinal = fn
	m.notifyMu.Unlock()
}

func (m *Manager) turnObserverFor() func(TurnFinal) {
	m.notifyMu.Lock()
	defer m.notifyMu.Unlock()
	return m.turnFinal
}

type boardTurnCapture struct {
	taskID  string
	userSeq int64
	turnID  string
	// messages keeps assistant messages in arrival order; each message keeps
	// its text blocks in arrival order. Iterating backwards finds the last
	// assistant message that actually carries text, which is the final reply
	// (a trailing tool-use-only message must not hide it).
	messages map[string]*boardTurnMessage
	order    []string
	// fallback is the claude-style `result` event's final text, used only when
	// no canonical assistant message carried text (defensive: some compatible
	// CLIs emit result alone).
	fallback string
}

type boardTurnMessage struct {
	blocks map[string]string
	keys   []string
}

func newBoardTurnCapture(msg Message) *boardTurnCapture {
	return &boardTurnCapture{
		taskID:   msg.BoardTaskID,
		userSeq:  msg.userSeq,
		messages: map[string]*boardTurnMessage{},
	}
}

// beginBoardTurn installs (or clears) the capture for the turn about to run.
func (m *Manager) beginBoardTurn(sessionID string, msg Message) {
	var capture *boardTurnCapture
	if msg.BoardTaskID != "" {
		capture = newBoardTurnCapture(msg)
		capture.turnID = fmt.Sprintf("%s#%d", sessionID, msg.userSeq)
		if !msg.userTs.IsZero() {
			// Rewind can reuse a Seq; the user event's timestamp keeps the
			// idempotency key unique across two distinct turns.
			capture.turnID += fmt.Sprintf("@%d", msg.userTs.UnixNano())
		}
	}
	r := m.rt(sessionID)
	r.mu.Lock()
	r.boardTurn = capture
	r.mu.Unlock()
}

// noteBoardOutput folds one live stream event into the active board capture.
// Called from emit for every transient stream event; a no-op unless the turn
// was board-bound.
func (m *Manager) noteBoardOutput(sessionID string, data any) {
	r := m.rt(sessionID)
	r.mu.Lock()
	capture := r.boardTurn
	if capture != nil {
		capture.add(data)
	}
	r.mu.Unlock()
}

// finishBoardTurn snapshots and clears the capture. A clean turn emits the
// callback on its own goroutine so the runner never blocks on board I/O; error
// and interrupt turns are dropped (partial output is not a final reply).
func (m *Manager) finishBoardTurn(sessionID, turnErr string) {
	r := m.rt(sessionID)
	r.mu.Lock()
	capture := r.boardTurn
	r.boardTurn = nil
	r.mu.Unlock()
	if capture == nil || turnErr != "" {
		return
	}
	text := capture.reply()
	if strings.TrimSpace(text) == "" {
		return
	}
	fn := m.turnObserverFor()
	if fn == nil {
		return
	}
	final := TurnFinal{
		SessionID:   sessionID,
		BoardTaskID: capture.taskID,
		TurnID:      capture.turnID,
		UserSeq:     capture.userSeq,
		Text:        text,
	}
	go fn(final)
}

func (c *boardTurnCapture) add(data any) {
	obj := streamObject(data)
	if obj == nil {
		return
	}
	typ, _ := obj["type"].(string)
	if typ == "result" {
		if text, _ := obj["result"].(string); strings.TrimSpace(text) != "" {
			c.fallback = text
		}
		return
	}
	if typ != "assistant" {
		// oc_text_delta / stream_event / user(tool_result) events are not a
		// final assistant message snapshot.
		return
	}
	msg, _ := obj["message"].(map[string]any)
	if msg == nil {
		return
	}
	id, _ := msg["id"].(string)
	if id == "" {
		// No stable id: treat it as its own message. The next event can only
		// supersede it by being later in arrival order.
		id = fmt.Sprintf("\x00%d", len(c.order))
	}
	entry := c.messages[id]
	if entry == nil {
		entry = &boardTurnMessage{blocks: map[string]string{}}
		c.messages[id] = entry
		c.order = append(c.order, id)
	}
	for i, raw := range contentBlocks(msg["content"]) {
		block, _ := raw.(map[string]any)
		if block == nil || block["type"] != "text" {
			continue
		}
		text, _ := block["text"].(string)
		key, _ := block["id"].(string)
		if key == "" {
			// Claude messages carry no block id; the ordinal is stable for a
			// repeated snapshot of the same message.
			key = fmt.Sprintf("#%d", i)
		}
		if _, seen := entry.blocks[key]; !seen {
			entry.keys = append(entry.keys, key)
		}
		entry.blocks[key] = text
	}
}

// reply returns the final assistant text: the last message with non-whitespace
// content, blocks concatenated in arrival order. This preserves Markdown
// verbatim (no trimming inside the text).
func (c *boardTurnCapture) reply() string {
	for i := len(c.order) - 1; i >= 0; i-- {
		msg := c.messages[c.order[i]]
		if msg == nil {
			continue
		}
		var b strings.Builder
		for _, key := range msg.keys {
			b.WriteString(msg.blocks[key])
		}
		if strings.TrimSpace(b.String()) != "" {
			return b.String()
		}
	}
	return c.fallback
}

func streamObject(data any) map[string]any {
	switch v := data.(type) {
	case map[string]any:
		return v
	case json.RawMessage:
		var obj map[string]any
		if json.Unmarshal(v, &obj) == nil {
			return obj
		}
	}
	return nil
}

func contentBlocks(v any) []any {
	switch content := v.(type) {
	case []any:
		return content
	case []map[string]any:
		out := make([]any, 0, len(content))
		for _, block := range content {
			out = append(out, block)
		}
		return out
	}
	return nil
}
