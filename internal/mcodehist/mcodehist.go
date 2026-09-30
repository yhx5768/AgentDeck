// Package mcodehist adapts the MiniMax Code (`mcode`) CLI to cc-webui.
//
// mcode speaks its own protocol — it is neither Claude Code's
// `assistant`/`user`/`result` line format nor opencode's part stream — so it
// needs its own engine kind and its own converter. The live path adapts
// `mcode exec --output-format stream-json` stdout; the replay path reads the
// session's own transcript (messages.jsonl), the relationship claudehist has
// with ~/.claude/projects. The two produce the same canonical event shapes so
// a refreshed page looks like the live run did.
//
// Live stream shape (every line one JSON object, ordered by `sequence`):
//
//	{"type":"exec.started"|"session.started"|"session.resumed"|"turn.started"}
//	{"type":"item.started"|"item.updated"|"item.completed",
//	 "item":{"id":"…","type":"agent_message"|"reasoning"|"tool_call", …}}
//	{"type":"turn.completed","model":{…},"usage":{…}}
//	{"type":"exec.completed","result":{"status":…,"output":…,"usage":{…}}}
//
// Mapping to the canonical shape the frontend already renders:
//
//	item.completed agent_message -> assistant text block
//	item.completed reasoning     -> assistant thinking block (mcode streams
//	                                reasoning live, so replaying it keeps a
//	                                reloaded page identical to the live run —
//	                                unlike claude, which strips it, and
//	                                opencode, which never emits it)
//	item.started   tool_call     -> assistant tool_use block (running)
//	item.completed tool_call     -> user tool_result block (is_error on failure)
//
// Only `item.completed` lines are adapted, never the `contentDelta` increments
// of `item.started`/`item.updated`: the completed line carries the full text,
// so replay and live produce byte-identical blocks. mcode's `sequence` makes
// the stream self-ordered, and its tool_call status codes (1 input ready,
// 2 succeeded, 3 failed, 4/5 pending) collapse to the same running-then-result
// pair claude produces.
package mcodehist

import (
	"encoding/json"
	"strings"
	"time"
)

// toolCall status codes in mcode's stream. Observed on 0.5.9: 4 and 5 arrive
// first with no input, 1 once the arguments are complete, then 2 (succeeded)
// or 3 (failed) with output attached.
const (
	statusPending    = 4
	statusHasInput   = 1
	statusSucceeded  = 2
	statusFailed     = 3
	itemTypeMessage  = "agent_message"
	itemTypeReason   = "reasoning"
	itemTypeToolCall = "tool_call"
)

// ToolTx tracks tool calls seen in one run. mcode emits a tool_call item
// through several item.* events (pending, input, output), and the frontend
// must see exactly one tool_use and one tool_result per call id.
// Single-goroutine use only, like opencodehist.ToolTx.
type ToolTx struct {
	assistantSent map[string]bool
	resultSent    map[string]bool
}

func NewToolTx() *ToolTx {
	return &ToolTx{assistantSent: map[string]bool{}, resultSent: map[string]bool{}}
}

// LiveEvent is the decoded envelope of one stream-json line.
type LiveEvent struct {
	Type        string
	SessionID   string
	Sequence    int64
	TimestampMs int64
	Item        map[string]any
	ToolCall    map[string]any
	Usage       map[string]any
	Model       map[string]any
	Result      map[string]any
	TS          time.Time
}

// ParseLive decodes one stdout line into a LiveEvent. ok is false for a blank
// or unparsable line, which the caller skips (the stream may carry blank
// separators).
func ParseLive(line []byte) (LiveEvent, bool) {
	if len(strings.TrimSpace(string(line))) == 0 {
		return LiveEvent{}, false
	}
	var obj map[string]any
	if err := json.Unmarshal(line, &obj); err != nil {
		return LiveEvent{}, false
	}
	ev := LiveEvent{}
	ev.Type, _ = obj["type"].(string)
	ev.SessionID, _ = obj["sessionId"].(string)
	ev.Sequence = numOf(obj["sequence"])
	ev.TimestampMs = numOf(obj["timestampMs"])
	ev.TS = time.UnixMilli(ev.TimestampMs)
	ev.Item, _ = obj["item"].(map[string]any)
	ev.Usage, _ = obj["usage"].(map[string]any)
	ev.Model, _ = obj["model"].(map[string]any)
	ev.Result, _ = obj["result"].(map[string]any)
	if ev.Item != nil {
		ev.ToolCall, _ = ev.Item["toolCall"].(map[string]any)
	}
	return ev, true
}

// SessionIDOf returns the mcode session id carried by a line, or "". Every
// mcode line carries sessionId (unlike claude, whose id arrives in a single
// system/init event), so the runner captures it on first sight.
func (e LiveEvent) SessionIDOf() string { return e.SessionID }

// Adapt converts one live event into canonical stream payloads. Only
// item.completed yields content; a tool_use block is emitted as soon as the
// call has its input (so the card appears while the tool runs) and the
// tool_result when it finishes.
func (e LiveEvent) Adapt(tx *ToolTx) []any {
	switch e.Type {
	case "item.completed":
		switch itemType(e.Item) {
		case itemTypeMessage:
			text := itemContent(e.Item)
			if strings.TrimSpace(text) == "" {
				return nil
			}
			return []any{assistantEvent(messageIDOf(itemID(e.Item)), e.TS, map[string]any{
				"type": "text", "id": itemID(e.Item), "text": text,
			}, nil, nil)}
		case itemTypeReason:
			thinking := itemContent(e.Item)
			if strings.TrimSpace(thinking) == "" {
				return nil
			}
			// Forwarded as a claude-shaped thinking block; the frontend has
			// rendered these since the claude engine's own thinking support.
			return []any{assistantEvent(messageIDOf(itemID(e.Item)), e.TS, map[string]any{
				"type": "thinking", "id": itemID(e.Item), "thinking": thinking,
			}, nil, nil)}
		case itemTypeToolCall:
			return adaptToolCall(e.ToolCall, e.TS, tx)
		}
	case "item.started", "item.updated":
		// A tool card can appear as soon as the arguments land (status 1),
		// before the call finishes; text deltas are ignored on purpose.
		if itemType(e.Item) == itemTypeToolCall {
			return adaptToolCall(e.ToolCall, e.TS, tx)
		}
	}
	return nil
}

// adaptToolCall emits the tool_use block (once, when input is known) and the
// tool_result block (once, on completion). Mirrors opencodehist.adaptTool so
// the frontend's tool-block pairing is identical across engines.
func adaptToolCall(tc map[string]any, ts time.Time, tx *ToolTx) []any {
	if tc == nil {
		return nil
	}
	callID, _ := tc["id"].(string)
	if callID == "" {
		return nil
	}
	name, _ := tc["name"].(string)
	status := int(numOf(tc["status"]))
	input, _ := tc["input"].(map[string]any)

	var out []any
	// The card is worth showing at status 1 (arguments complete) and at
	// completion; earlier pending statuses carry no input to display.
	if (status == statusHasInput || status == statusSucceeded || status == statusFailed) &&
		!tx.assistantSent[callID] {
		tx.assistantSent[callID] = true
		block := map[string]any{"type": "tool_use", "id": callID}
		if name != "" {
			block["name"] = name
		}
		if len(input) > 0 {
			block["input"] = input
		}
		out = append(out, assistantEvent(callID, ts, block, nil, nil))
	}
	if (status == statusSucceeded || status == statusFailed) && !tx.resultSent[callID] {
		tx.resultSent[callID] = true
		content := []any{}
		if text := toolOutputText(tc["output"]); text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		block := map[string]any{
			"type":        "tool_result",
			"tool_use_id": callID,
			"content":     content,
		}
		if status == statusFailed {
			block["is_error"] = true
		}
		out = append(out, map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": []any{block}},
		})
	}
	return out
}

// UsagePayload builds the per-turn usage/model carry from turn.completed (or
// the exec result, which repeats it). mcode reports usage once per turn,
// outside any assistant message, so it rides a dedicated assistant event with
// an empty content list: the frontend renders nothing for it while the stats
// path (claudehist/opencodehist/mcodehist all read message.usage) sees the
// counts. Pass an empty messageID to keep it out of the trajectory merge.
func UsagePayload(usage, model map[string]any, ts time.Time) any {
	if len(usage) == 0 && len(model) == 0 {
		return nil
	}
	msg := map[string]any{"role": "assistant", "content": []any{}}
	if len(usage) > 0 {
		msg["usage"] = normalizeUsage(usage)
	}
	if model != nil {
		if mid, _ := model["modelId"].(string); mid != "" {
			msg["model"] = mid
		}
	}
	return map[string]any{"type": "assistant", "message": msg, "_ts": ts.UnixMilli()}
}

// normalizeUsage maps mcode's per-turn token counts onto the snake_case keys
// every engine's stream events use (claude's own naming, which is also what
// the stats aggregator and the trajectory view read). mcode reports
// {"input","output","cacheRead","cacheWrite","totalTokens","cost"}; the
// transcript is normalized through this same function on replay, so a
// reloaded page carries the same usage shape the live stream did.
func normalizeUsage(u map[string]any) map[string]any {
	if len(u) == 0 {
		return nil
	}
	get := func(k string) int64 { return numOf(u[k]) }
	return map[string]any{
		"input_tokens":                get("inputTokens"),
		"output_tokens":               get("outputTokens"),
		"cache_read_input_tokens":     get("cacheReadTokens"),
		"cache_creation_input_tokens": get("cacheWriteTokens"),
	}
}

// normalizeTranscriptUsage is normalizeUsage for the transcript's own key
// names, which differ from the stream's (input/output/cacheRead rather than
// inputTokens/…).
func normalizeTranscriptUsage(u map[string]any) map[string]any {
	if len(u) == 0 {
		return nil
	}
	get := func(k string) int64 { return numOf(u[k]) }
	return map[string]any{
		"input_tokens":                get("input"),
		"output_tokens":               get("output"),
		"cache_read_input_tokens":     get("cacheRead"),
		"cache_creation_input_tokens": get("cacheWrite"),
	}
}

// ExecError extracts a human-readable failure message from a fatal run error.
// mcode reports argument/provider failures on stderr and exits non-zero
// (e.g. "--effort bogus is not available … Available levels: …"), so the
// runner surfaces stderr; ok is false for a normal completion.
func ExecError(stderr string, waitErr error) (string, bool) {
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		return "", false
	}
	return msg, true
}

// ---- small helpers ----

func itemType(item map[string]any) string {
	if item == nil {
		return ""
	}
	t, _ := item["type"].(string)
	return t
}

func itemID(item map[string]any) string {
	if item == nil {
		return ""
	}
	id, _ := item["id"].(string)
	return id
}

// itemContent returns the completed item's full text. mcode names it
// `content` on item.completed and `contentDelta` on the incremental updates.
func itemContent(item map[string]any) string {
	if item == nil {
		return ""
	}
	s, _ := item["content"].(string)
	return s
}

// messageIDOf derives the assistant message id from an item id. Text and
// reasoning items are "<uuid>:message" / "<uuid>:reasoning" — one uuid per
// assistant step, so blocks of the same step share a message id and the
// trajectory projection merges them the way it does for claude split lines.
func messageIDOf(itemID string) string {
	if i := strings.Index(itemID, ":"); i > 0 {
		return itemID[:i]
	}
	return itemID
}

// toolOutputText flattens a toolCall.output into display text. mcode uses
// {"content":[{"type":"text","text":…}],"details":{…}}; other shapes are
// stringified defensively.
func toolOutputText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case map[string]any:
		var parts []string
		if content, ok := t["content"].([]any); ok {
			for _, c := range content {
				if cm, ok := c.(map[string]any); ok {
					if s, _ := cm["text"].(string); s != "" {
						parts = append(parts, s)
					}
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
		raw, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(raw)
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

// assistantEvent wraps one content block into a canonical assistant stream
// object — the same shape claudehist.Convert produces for split lines.
func assistantEvent(messageID string, ts time.Time, block map[string]any, usage, model map[string]any) any {
	msg := map[string]any{"role": "assistant", "content": []any{block}}
	if messageID != "" {
		msg["id"] = messageID
	}
	if len(usage) > 0 {
		msg["usage"] = usage
	}
	if model != nil {
		if mid, _ := model["modelId"].(string); mid != "" {
			msg["model"] = mid
		}
	}
	return map[string]any{"type": "assistant", "message": msg}
}

func numOf(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}
