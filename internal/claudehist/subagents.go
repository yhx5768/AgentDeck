package claudehist

// Subagent transcripts live next to the main session jsonl:
// ~/.claude/projects/<slug>/<sid>/subagents/agent-<agentId>.jsonl, with a
// sibling agent-<agentId>.meta.json linking back to the parent conversation.
// The CLI relays subagent messages live on the parent's stdout as full
// assistant events with parent_tool_use_id set — but those events are not
// persisted anywhere in the main transcript, so replay-side parity requires
// parsing the subagent files. ConvertSubagents emits the same event shape the
// live relay uses, which lets the frontend render both through one code path.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cc-webui/internal/store"
)

// subagentMeta mirrors agent-<agentId>.meta.json.
type subagentMeta struct {
	AgentType   string `json:"agentType"`
	Description string `json:"description"`
	ToolUseID   string `json:"toolUseId"`
	SpawnDepth  int    `json:"spawnDepth"`
}

// readSubagentMeta loads one meta.json, returning zero values on any error
// (older CLIs don't always write it).
func readSubagentMeta(path string) subagentMeta {
	var m subagentMeta
	raw, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(raw, &m)
	return m
}

// ConvertSubagents converts a session's subagent transcripts into stream
// events shaped like the CLI's live relay: type=assistant with
// parent_tool_use_id set, plus a subagent {title,type} label. Assistant lines
// forward text + tool_use blocks (thinking is dropped — the CLI strips it
// from live-relayed complete events, so keeping it would make replay richer
// than live); user lines forward tool_result blocks so the chat card can pair
// results to the tool calls. The task prompt (string content) and other
// non-tool_result user lines are skipped. When meta.json (and thus toolUseId)
// is missing, "agent:<agentId>" is used as the parent key and the task prompt
// / attribution agent become the title/type. Returns ts-ascending events; a
// missing or empty subagent dir yields a nil slice and os.ErrNotExist-style
// error from ReadDir.
func ConvertSubagents(cwd, sessionID string) ([]*store.Event, error) {
	dir := filepath.Join(HistoryDir(cwd), sessionID, "subagents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var events []*store.Event
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		agentID := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl")
		meta := readSubagentMeta(filepath.Join(dir, "agent-"+agentID+".meta.json"))
		lines := scanFile(filepath.Join(dir, name))
		parentKey := meta.ToolUseID
		if parentKey == "" {
			parentKey = "agent:" + agentID
		}
		title := meta.Description
		if title == "" {
			title = firstUserText(lines)
		}
		if len(title) > 80 {
			title = title[:80] + "…"
		}
		agentType := meta.AgentType
		if agentType == "" {
			agentType = firstAttributionAgent(lines)
		}
		for _, l := range lines {
			ts, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
			switch l.Type {
			case "assistant":
				blocks := relayBlocks(l.Message.Content)
				if len(blocks) == 0 {
					continue
				}
				msg := map[string]any{"role": "assistant", "content": blocks}
				if l.Message.ID != "" {
					msg["id"] = l.Message.ID
				}
				data, _ := json.Marshal(map[string]any{
					"type":               "assistant",
					"parent_tool_use_id": parentKey,
					"subagent":           map[string]string{"title": title, "type": agentType},
					"message":            msg,
				})
				events = append(events, &store.Event{Ts: ts, Kind: "stream", Data: data})
			case "user":
				trs := toolResultBlocks(l.Message.Content)
				if len(trs) == 0 {
					continue
				}
				data, _ := json.Marshal(map[string]any{
					"type":               "user",
					"parent_tool_use_id": parentKey,
					"message":            map[string]any{"role": "user", "content": trs},
				})
				events = append(events, &store.Event{Ts: ts, Kind: "stream", Data: data})
			}
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Ts.Before(events[j].Ts) })
	return events, nil
}

// relayBlocks filters a subagent assistant content array down to what the
// chat card renders: non-empty text and tool_use blocks. thinking is dropped
// (see ConvertSubagents doc); everything else passes through verbatim.
func relayBlocks(raw json.RawMessage) []map[string]any {
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	var out []map[string]any
	for _, b := range blocks {
		switch t, _ := b["type"].(string); t {
		case "text":
			if s, _ := b["text"].(string); strings.TrimSpace(s) != "" {
				out = append(out, b)
			}
		case "tool_use":
			if name, _ := b["name"].(string); name != "" {
				out = append(out, b)
			}
		}
	}
	return out
}

// toolResultBlocks extracts tool_result blocks from a user content payload
// (string content → nil: that's the task prompt, not an echo).
func toolResultBlocks(raw json.RawMessage) []map[string]any {
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	var out []map[string]any
	for _, b := range blocks {
		if t, _ := b["type"].(string); t == "tool_result" {
			out = append(out, b)
		}
	}
	return out
}

// firstUserText returns the text of the first user line (the task prompt the
// parent gave the subagent), used as fallback title. "" when none.
func firstUserText(lines []histLine) string {
	for _, l := range lines {
		if l.Type != "user" {
			continue
		}
		if t := strings.TrimSpace(textOf(l.Message.Content)); t != "" {
			return t
		}
	}
	return ""
}

// firstAttributionAgent returns the subagent type recorded on assistant lines
// (e.g. "Explore"), used as fallback when meta.json lacks agentType.
func firstAttributionAgent(lines []histLine) string {
	for _, l := range lines {
		if l.Type == "assistant" && l.AttributionAgent != "" {
			return l.AttributionAgent
		}
	}
	return ""
}
