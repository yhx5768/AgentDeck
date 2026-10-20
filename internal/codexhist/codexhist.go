// Package codexhist adapts the Codex CLI (`codex`, and wrappers such as
// Baidu's `ducx`) to cc-webui.
//
// Codex is its own protocol. The live path decodes the JSONL that
// `codex exec --json` prints (thread.started / item.started / item.completed /
// turn.completed); the replay path reads the rollout file the CLI writes under
// $CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl. Both are mapped onto the
// canonical cc-webui event shapes, so a reloaded page looks like the live run.
//
// Live stream shape (one JSON object per line):
//
//	{"type":"thread.started","thread_id":"<uuid>"}
//	{"type":"turn.started"|"turn.completed","usage":{…}}
//	{"type":"turn.failed","error":{"message":"…"}}
//	{"type":"item.started"|"item.completed","item":{
//	    "id":"item_0","type":"agent_message|reasoning|command_execution|
//	     file_change|mcp_tool_call|web_search|todo_list|error", …}}
//
// Mapping:
//
//	item.completed agent_message      -> assistant text
//	item.completed reasoning          -> assistant thinking
//	item.started|completed command_execution -> tool_use + tool_result
//	item.started|completed file_change       -> tool_use + tool_result
//	turn.completed usage              -> assistant event with empty content
//
// `codex exec` has no token-level streaming: item.completed carries the full
// text, and no item.updated lines are produced for assistant text, so the live
// and replay views line up byte for byte.
package codexhist

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Item types emitted by codex exec --json.
const (
	itemAgentMessage     = "agent_message"
	itemReasoning        = "reasoning"
	itemCommandExecution = "command_execution"
	itemFileChange       = "file_change"
	itemMCPToolCall      = "mcp_tool_call"
	itemWebSearch        = "web_search"
	itemTodoList         = "todo_list"
	itemError            = "error"
)

// ToolTx tracks one run's tool calls so the frontend sees exactly one
// tool_use and one tool_result per call id. Single-goroutine use only, like
// mcodehist.ToolTx.
type ToolTx struct {
	assistantSent map[string]bool
	resultSent    map[string]bool
	turn          int
	// runID distinguishes two exec processes of the same session: codex
	// restarts item numbering (item_0, item_1, …) on every run, and a session
	// spawns one process per user turn.
	runID string
}

// NewToolTx returns an empty tracker.
func NewToolTx() *ToolTx {
	return &ToolTx{
		assistantSent: map[string]bool{},
		resultSent:    map[string]bool{},
		runID:         fmt.Sprintf("%x", time.Now().UnixNano()),
	}
}

// LiveEvent is one decoded stdout line.
type LiveEvent struct {
	Type     string
	ThreadID string
	Item     map[string]any
	Usage    map[string]any
	Error    map[string]any
	TS       time.Time
}

// ParseLive decodes one `codex exec --json` line. ok is false for a blank or
// unparsable line (stderr noise never arrives here, but wrappers may print
// banners on stdout).
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
	if ev.Type == "" {
		return LiveEvent{}, false
	}
	ev.ThreadID, _ = obj["thread_id"].(string)
	ev.Item, _ = obj["item"].(map[string]any)
	ev.Usage, _ = obj["usage"].(map[string]any)
	ev.Error, _ = obj["error"].(map[string]any)
	ev.TS = time.Now()
	if ts, _ := obj["timestamp"].(string); ts != "" {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			ev.TS = t
		}
	}
	return ev, true
}

// SessionIDOf returns the codex session id carried by the line. Only
// thread.started carries it; the runner captures it there.
func (e LiveEvent) SessionIDOf() string {
	if e.Type == "thread.started" {
		return e.ThreadID
	}
	return ""
}

// ErrorMessage extracts a fatal error message from turn.failed (or a bare
// error line), or "" for a normal line.
func (e LiveEvent) ErrorMessage() string {
	switch e.Type {
	case "turn.failed":
		if msg, _ := e.Error["message"].(string); msg != "" {
			return msg
		}
	case "error":
		if msg, _ := e.Error["message"].(string); msg != "" {
			return msg
		}
	}
	return ""
}

// Adapt converts one live event into canonical stream payloads. Every content
// item is emitted once, at completion; tool cards appear as soon as the call
// starts so the UI shows them while the command runs.
func (e LiveEvent) Adapt(tx *ToolTx) []any {
	switch e.Type {
	case "turn.started":
		tx.turn++
		return nil
	case "item.started", "item.completed":
		item := e.Item
		if item == nil {
			return nil
		}
		done := e.Type == "item.completed"
		itemID := tx.uid(item["id"])
		switch itemType(item) {
		case itemAgentMessage:
			if !done {
				return nil
			}
			text, _ := item["text"].(string)
			if strings.TrimSpace(text) == "" {
				return nil
			}
			return []any{assistantEvent(tx.messageID(item["id"]), e.TS, map[string]any{
				"type": "text", "id": itemID, "text": text,
			}, nil, nil)}
		case itemReasoning:
			if !done {
				return nil
			}
			text, _ := item["text"].(string)
			if strings.TrimSpace(text) == "" {
				return nil
			}
			return []any{assistantEvent(tx.messageID(item["id"]), e.TS, map[string]any{
				"type": "thinking", "id": itemID, "thinking": text,
			}, nil, nil)}
		case itemCommandExecution, itemFileChange, itemMCPToolCall, itemWebSearch, itemTodoList:
			return adaptItemTool(itemID, item, done, e.TS, tx)
		}
	}
	return nil
}

// adaptItemTool maps one tool-shaped item onto the tool_use/tool_result pair.
func adaptItemTool(callID string, item map[string]any, done bool, ts time.Time, tx *ToolTx) []any {
	name, input := toolNameInput(item)
	if name == "" {
		return nil
	}
	var out []any
	if !tx.assistantSent[callID] {
		tx.assistantSent[callID] = true
		block := map[string]any{"type": "tool_use", "id": callID, "name": name}
		if len(input) > 0 {
			block["input"] = input
		}
		out = append(out, assistantEvent(callID, ts, block, nil, nil))
	}
	if done && !tx.resultSent[callID] {
		tx.resultSent[callID] = true
		output, isErr := toolOutput(item)
		content := []any{}
		if output != "" {
			content = append(content, map[string]any{"type": "text", "text": output})
		}
		block := map[string]any{
			"type": "tool_result", "tool_use_id": callID, "content": content,
		}
		if isErr {
			block["is_error"] = true
		}
		out = append(out, map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": []any{block}},
		})
	}
	return out
}

// toolNameInput derives the display name and arguments from a codex item.
func toolNameInput(item map[string]any) (string, map[string]any) {
	switch itemType(item) {
	case itemCommandExecution:
		input := map[string]any{}
		switch cmd := item["command"].(type) {
		case string:
			if cmd != "" {
				input["command"] = cmd
			}
		case []any:
			parts := make([]string, 0, len(cmd))
			for _, c := range cmd {
				if s, _ := c.(string); s != "" {
					parts = append(parts, s)
				}
			}
			if len(parts) > 0 {
				input["command"] = strings.Join(parts, " ")
			}
		}
		if cwd, _ := item["cwd"].(string); cwd != "" {
			input["cwd"] = cwd
		}
		return "exec_command", input
	case itemFileChange:
		input := map[string]any{}
		if changes, ok := item["changes"]; ok && changes != nil {
			input["changes"] = changes
		}
		return "apply_patch", input
	case itemMCPToolCall:
		name, _ := item["tool"].(string)
		if name == "" {
			name, _ = item["name"].(string)
		}
		if name == "" {
			name = "mcp_tool_call"
		}
		input, _ := item["arguments"].(map[string]any)
		if input == nil {
			input, _ = item["input"].(map[string]any)
		}
		return name, input
	case itemWebSearch:
		input := map[string]any{}
		if q, _ := item["query"].(string); q != "" {
			input["query"] = q
		}
		return "web_search", input
	case itemTodoList:
		input := map[string]any{}
		if items, ok := item["items"]; ok && items != nil {
			input["items"] = items
		}
		return "todo_list", input
	}
	return "", nil
}

// toolOutput extracts the display text and error flag from a finished item.
func toolOutput(item map[string]any) (string, bool) {
	switch itemType(item) {
	case itemCommandExecution:
		text, _ := item["aggregated_output"].(string)
		code := numOf(item["exit_code"])
		failed := statusOf(item) == "failed" || (item["exit_code"] != nil && code != 0)
		return text, failed
	case itemFileChange:
		parts := []string{}
		for _, k := range []string{"stdout", "stderr"} {
			if s, _ := item[k].(string); strings.TrimSpace(s) != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n"), statusOf(item) == "failed"
	case itemMCPToolCall:
		return outputText(item["result"]), statusOf(item) == "failed"
	case itemWebSearch:
		return outputText(item["results"]), statusOf(item) == "failed"
	case itemTodoList:
		return "", false
	}
	return "", false
}

// UsagePayload builds the per-turn usage carry from turn.completed. Codex
// reports usage outside any assistant message, so it rides a dedicated
// assistant event with an empty content list: the UI renders nothing while
// the stats path sees the counts. model is the name the turn ran on.
func UsagePayload(usage map[string]any, model string, ts time.Time) any {
	if len(usage) == 0 {
		return nil
	}
	msg := map[string]any{"role": "assistant", "content": []any{}}
	msg["usage"] = normalizeUsage(usage)
	if model != "" {
		msg["model"] = model
	}
	return map[string]any{"type": "assistant", "message": msg, "_ts": ts.UnixMilli()}
}

// normalizeUsage maps codex's token keys onto the snake_case naming every
// engine's stream events use. Codex reports cached_input_tokens for cache
// reads and reasoning_output_tokens for the reasoning split.
func normalizeUsage(u map[string]any) map[string]any {
	if len(u) == 0 {
		return nil
	}
	get := func(k string) int64 { return numOf(u[k]) }
	return map[string]any{
		"input_tokens":                get("input_tokens"),
		"output_tokens":               get("output_tokens"),
		"reasoning_tokens":            get("reasoning_output_tokens"),
		"cache_read_input_tokens":     get("cached_input_tokens"),
		"cache_creation_input_tokens": get("cache_write_input_tokens"),
	}
}

// ---- helpers ----

func itemType(item map[string]any) string {
	if item == nil {
		return ""
	}
	t, _ := item["type"].(string)
	return t
}

func statusOf(item map[string]any) string {
	s, _ := item["status"].(string)
	return s
}

// uid makes an item id unique across turns. Codex restarts item numbering at
// item_0 for every exec, so the raw id would collide in the trajectory view
// when a session has several turns. The thread id plus the turn counter is
// stable within one live run and never repeats across turns.
func (tx *ToolTx) uid(v any) string {
	id, _ := v.(string)
	if id == "" {
		// file_change uses the call id as item id; mcp calls always have one.
		id = "item_unknown"
	}
	return fmt.Sprintf("%s-t%d-%s", tx.runID, tx.turn, id)
}

func (tx *ToolTx) messageID(v any) string { return tx.uid(v) }

// assistantEvent wraps one content block into a canonical assistant stream
// object.
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
	return map[string]any{"type": "assistant", "message": msg, "_ts": ts.UnixMilli()}
}

// outputText flattens an arbitrary tool result value into display text.
func outputText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		parts := []string{}
		for _, c := range t {
			if s := outputText(c); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		for _, k := range []string{"text", "output", "content", "stdout", "message"} {
			if s, ok := t[k].(string); ok && s != "" {
				return s
			}
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
