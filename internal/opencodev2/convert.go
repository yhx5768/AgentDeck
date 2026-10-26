package opencodev2

import (
	"encoding/json"
	"fmt"
	"time"

	"cc-webui/internal/store"
)

// ToEvents converts an exported v2 session into cc-webui events (Kind "user"
// for user prompts, Kind "stream" for everything else), in conversation order.
func (s *Session) ToEvents() []*store.Event {
	tx := newToolTx()
	var evs []*store.Event
	for _, m := range s.Messages {
		switch m.Role {
		case "user":
			if m.Text == "" {
				continue
			}
			data, _ := json.Marshal(map[string]string{"text": m.Text})
			evs = append(evs, &store.Event{Ts: m.TS, Kind: "user", Data: data})
		case "assistant":
			usage := m.usageMap()
			for i, p := range m.Content {
				for _, out := range adaptContent(m, p, i, usage, tx) {
					data, _ := json.Marshal(out.Data)
					evs = append(evs, &store.Event{Ts: out.Ts, Kind: out.Kind, Data: data})
				}
			}
		}
	}
	return evs
}

// usageMap renders the step usage in the snake_case shape every engine's
// stream events use (also what statsagg reads).
func (m SessionMessage) usageMap() map[string]any {
	if m.Tokens == (MessageTokens{}) {
		return nil
	}
	return map[string]any{
		"input_tokens":                m.Tokens.Input,
		"output_tokens":               m.Tokens.Output,
		"reasoning_tokens":            m.Tokens.Reasoning,
		"cache_read_input_tokens":     m.Tokens.CacheRead,
		"cache_creation_input_tokens": m.Tokens.CacheWrite,
	}
}

// toolTx tracks tool parts seen so far in one session, so a tool call renders
// exactly one card and one result. Single-goroutine use only.
type toolTx struct {
	useSent    map[string]bool
	resultSent map[string]bool
}

func newToolTx() *toolTx {
	return &toolTx{useSent: map[string]bool{}, resultSent: map[string]bool{}}
}

// outEvent is one cc-webui event derived from a single content part.
type outEvent struct {
	Kind string // "user" (tool_result) | "stream"
	Ts   time.Time
	Data any
}

func adaptContent(m SessionMessage, p ContentPart, ordinal int, usage map[string]any, tx *toolTx) []outEvent {
	switch p.Type {
	case "text":
		if p.Text == "" {
			return nil
		}
		block := map[string]any{"type": "text", "text": p.Text, "id": fmt.Sprintf("%s:text-%d", m.ID, ordinal)}
		return []outEvent{assistantEvent(m, block, usage)}
	case "reasoning":
		if p.Text == "" {
			return nil
		}
		block := map[string]any{"type": "thinking", "thinking": p.Text, "id": fmt.Sprintf("%s:reasoning-%d", m.ID, ordinal)}
		return []outEvent{assistantEvent(m, block, usage)}
	case "tool":
		return adaptTool(m, p, usage, tx)
	default:
		return nil
	}
}

// adaptTool emits the tool_use block once (when its arguments are known) and
// the tool_result once (on completion/error).
func adaptTool(m SessionMessage, p ContentPart, usage map[string]any, tx *toolTx) []outEvent {
	if p.CallID == "" {
		return nil
	}
	var out []outEvent
	if !tx.useSent[p.CallID] && len(p.Input) > 0 {
		tx.useSent[p.CallID] = true
		block := map[string]any{"type": "tool_use", "id": p.CallID}
		if p.ToolName != "" {
			block["name"] = p.ToolName
		}
		block["input"] = p.Input
		out = append(out, assistantEvent(m, block, usage))
	}
	if (p.Status == "completed" || p.Status == "error") && !tx.resultSent[p.CallID] {
		tx.resultSent[p.CallID] = true
		content := []any{}
		if p.Output != "" {
			content = append(content, map[string]any{"type": "text", "text": p.Output})
		}
		block := map[string]any{"type": "tool_result", "tool_use_id": p.CallID, "content": content}
		if p.IsError {
			block["is_error"] = true
		}
		out = append(out, outEvent{
			Kind: "stream",
			Ts:   m.TS,
			Data: map[string]any{
				"type":    "user",
				"message": map[string]any{"role": "user", "content": []any{block}},
			},
		})
	}
	return out
}

// assistantEvent wraps one content block into the canonical assistant stream
// object (same shape claudehist.Convert and opencodehist produce).
func assistantEvent(m SessionMessage, block map[string]any, usage map[string]any) outEvent {
	msg := map[string]any{"role": "assistant", "content": []any{block}}
	if m.ID != "" {
		msg["id"] = m.ID
	}
	if m.ModelID != "" {
		msg["model"] = m.ModelID
	}
	if usage != nil {
		msg["usage"] = usage
	}
	return outEvent{
		Kind: "stream",
		Ts:   m.TS,
		Data: map[string]any{"type": "assistant", "message": msg},
	}
}
