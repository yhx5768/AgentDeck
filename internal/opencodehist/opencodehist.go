// Package opencodehist reads opencode CLI session data and converts it into
// cc-webui events. opencode keeps sessions in its own sqlite store; we never
// touch it directly — everything goes through the CLI's official surfaces
// (`opencode session list --format json`, `opencode export <id>`), mirroring
// how claudehist reads Claude Code's own jsonl transcripts.
//
// Conversion rules (shared by live stream adaptation and export replay):
//
//	user message text part     -> kind=user event {text}
//	assistant text part        -> stream assistant event, one text block
//	assistant tool part        -> stream assistant event (tool_use block) +
//	                              stream user event (tool_result block)
//	reasoning / patch / step-* -> skipped
//
// Parts of one assistant step share a message id; each part becomes its own
// event carrying that id, mirroring Claude Code's split-line layout which the
// frontend (trajectory byMsgId merge, per-block chat rendering) already
// handles. Reasoning is skipped on purpose: `run --format json` does not
// emit reasoning parts live (only export has them), so replaying them would
// make "refresh after reload" differ from the live view.
package opencodehist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"cc-webui/internal/store"
)

// SessionInfo is one session from `opencode session list --format json`,
// normalized to the same JSON shape claudehist.SessionInfo uses.
type SessionInfo struct {
	SessionID string    `json:"session_id"`
	Title     string    `json:"title"`
	Cwd       string    `json:"cwd"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedAt time.Time `json:"created_at"`
}

// PreviewMessage is one message in the import preview.
type PreviewMessage struct {
	Role string    `json:"role"` // user | assistant
	Text string    `json:"text"`
	TS   time.Time `json:"ts"`
}

// Session is one parsed `opencode export` document.
type Session struct {
	ID              string
	Title           string
	Directory       string
	Agent           string
	ModelProviderID string // providerID of the model the session runs on
	ModelID         string
	UpdatedAt       time.Time
	CreatedAt       time.Time
	Messages        []SessionMessage
}

// SessionMessage is one message of an exported session. Parts stay raw maps —
// the adapt logic reads a handful of keys and ignores the rest.
type SessionMessage struct {
	Role  string
	ID    string
	TS    time.Time
	Parts []map[string]any
}

// execTimeout bounds every opencode CLI invocation. Exports of very long
// sessions are the slowest call; listing is quick.
const execTimeout = 30 * time.Second

// runCLI executes the opencode binary with args and returns stdout. stderr is
// folded into the error so the user sees the CLI's own message.
func runCLI(cliPath string, args ...string) ([]byte, error) {
	return runCLIDir("", cliPath, args...)
}

// runCLIDir is runCLI with a working directory for the child process (used to
// pick a project bucket for `session list`; empty inherits the server cwd).
func runCLIDir(dir, cliPath string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cliPath, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Capture stdout to a temp file, not a pipe (bytes.Buffer). opencode's
	// `export` of a session with tool calls routinely exceeds the 64KiB pipe
	// buffer, and the CLI exits before flushing the remainder to a pipe — the
	// truncated JSON then fails to parse, replay silently drops the whole
	// assistant history (only stored user prompts survive). A regular file has
	// no such cap and is fully written before exit. (`session list` with many
	// sessions can grow large too, so the shared helper gets the fix.)
	tmp, err := os.CreateTemp("", "opencode-*.json")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	cmd.Stdout = tmp
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	tmp.Close()
	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = runErr.Error()
		}
		return nil, fmt.Errorf("%s: %s", cliPath, msg)
	}
	return os.ReadFile(tmpName)
}

// List returns every opencode session (newest first). Filtering by cwd/prefix
// happens in the caller (in-memory over this slice).
//
// opencode partitions its session store by project: `session list` only
// returns sessions of the project the process cwd belongs to (a git repo for
// repo-scoped sessions, "global" for everything outside one). Callers pass
// dir to target a bucket — empty keeps the server's own cwd.
func List(cliPath, dir string) ([]SessionInfo, error) {
	raw, err := runCLIDir(dir, cliPath, "session", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Directory string `json:"directory"`
		Created   int64  `json:"created"`
		Updated   int64  `json:"updated"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("parse opencode session list: %w", err)
	}
	out := make([]SessionInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, SessionInfo{
			SessionID: r.ID,
			Title:     r.Title,
			Cwd:       r.Directory,
			CreatedAt: msTime(r.Created),
			UpdatedAt: msTime(r.Updated),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// FetchSession exports one session (`opencode export <id>`, stdout is pure
// JSON). The CLI writes a short stderr banner line; stdout is authoritative.
func FetchSession(cliPath, sessionID string) (*Session, error) {
	raw, err := runCLI(cliPath, "export", sessionID)
	if err != nil {
		return nil, err
	}
	s, err := parseExport(raw)
	if err != nil {
		return nil, fmt.Errorf("parse opencode export %s: %w", sessionID, err)
	}
	return s, nil
}

func msTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func parseExport(raw []byte) (*Session, error) {
	var doc struct {
		Info struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			Directory string `json:"directory"`
			Agent     string `json:"agent"`
			Version   string `json:"version"`
			Model     *struct {
				ID         string `json:"id"`
				ProviderID string `json:"providerID"`
			} `json:"model"`
			Time struct {
				Created int64 `json:"created"`
				Updated int64 `json:"updated"`
			} `json:"time"`
		} `json:"info"`
		Messages []struct {
			Info struct {
				Role string `json:"role"`
				ID   string `json:"id"`
				Time struct {
					Created int64 `json:"created"`
				} `json:"time"`
			} `json:"info"`
			Parts []map[string]any `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	s := &Session{
		ID:              doc.Info.ID,
		Title:           doc.Info.Title,
		Directory:       doc.Info.Directory,
		Agent:           doc.Info.Agent,
		UpdatedAt:       msTime(doc.Info.Time.Updated),
		CreatedAt:       msTime(doc.Info.Time.Created),
		ModelProviderID: "",
		ModelID:         "",
	}
	if doc.Info.Model != nil {
		s.ModelProviderID = doc.Info.Model.ProviderID
		s.ModelID = doc.Info.Model.ID
	}
	for _, m := range doc.Messages {
		s.Messages = append(s.Messages, SessionMessage{
			Role:  m.Info.Role,
			ID:    m.Info.ID,
			TS:    msTime(m.Info.Time.Created),
			Parts: m.Parts,
		})
	}
	return s, nil
}

// ToEvents converts an exported session into cc-webui events (Kind "user" for
// user prompts, Kind "stream" for everything else), in conversation order.
func (s *Session) ToEvents() []*store.Event {
	tx := NewToolTx()
	var evs []*store.Event
	for _, m := range s.Messages {
		switch m.Role {
		case "user":
			// One event per message: join its text parts so the stored echo of
			// the same prompt (one stored event) suppresses exactly one replay
			// event in mergeStoredAndClaude's count-based dedup. opencode wraps
			// the echoed prompt in double quotes (both live echo and export);
			// strip them so the text matches the echo cc-webui stored at
			// enqueue time — otherwise the replay duplicates the user bubble.
			var texts []string
			for _, p := range m.Parts {
				if t, ok := userPromptText(p); ok {
					texts = append(texts, stripEchoQuotes(t))
				}
			}
			if len(texts) == 0 {
				continue
			}
			data, _ := json.Marshal(map[string]string{"text": strings.Join(texts, "\n")})
			evs = append(evs, &store.Event{Ts: m.TS, Kind: "user", Data: data})
		case "assistant":
			for _, p := range m.Parts {
				for _, out := range adaptPart(m.ID, p, m.TS, tx) {
					data, _ := json.Marshal(out.Data)
					evs = append(evs, &store.Event{Ts: out.Ts, Kind: out.Kind, Data: data})
				}
			}
		}
	}
	return evs
}

// PreviewMessageOf mirrors claudehist.Preview: the most recent messages, text
// only, newest-limited count, returned in chronological order.
func (s *Session) Preview(maxMessages int) []PreviewMessage {
	var out []PreviewMessage
	seen := 0
	for i := len(s.Messages) - 1; i >= 0 && seen < maxMessages; i-- {
		m := s.Messages[i]
		var texts []string
		for _, p := range m.Parts {
			t := ""
			if m.Role == "user" {
				// user 消息可能带 -f 附件产生的工具前导/文件展开文本，
				// 只取真正的 prompt echo（规则与 ToEvents 一致）
				if pt, ok := userPromptText(p); ok {
					t = stripEchoQuotes(pt)
				}
			} else if partType(p) == "text" {
				t, _ = p["text"].(string)
			}
			if t != "" && strings.TrimSpace(t) != "" {
				texts = append(texts, t)
			}
		}
		if len(texts) == 0 {
			continue
		}
		seen++
		role := m.Role
		if role != "user" && role != "assistant" {
			continue
		}
		out = append(out, PreviewMessage{
			Role: role,
			Text: truncatePreview(strings.Join(texts, "\n")),
			TS:   m.TS,
		})
	}
	// Collected newest-first; flip to chronological.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// userPromptText picks the user's actual prompt text out of a user-message
// part. opencode builds extra text parts around `-f` file attachments — a
// tool-call preamble ("Called the Read tool with the following input: ...")
// and a <path>/<type>/<content> expansion — besides the file part itself.
// Only the remaining text (the echo of the typed prompt) belongs in the
// cc-webui user bubble, or the replayed echo text won't match the stored one.
func userPromptText(p map[string]any) (string, bool) {
	if partType(p) != "text" {
		return "", false
	}
	t, _ := p["text"].(string)
	lt := strings.TrimLeft(t, " \t\r\n")
	if strings.HasPrefix(lt, "Called the Read tool with the following input: ") {
		return "", false
	}
	if strings.HasPrefix(lt, "<path>") {
		return "", false
	}
	return t, true
}

// stripEchoQuotes removes one layer of the double-quote wrapper opencode puts
// around echoed user prompt text (its run echo and the persisted export both
// carry it). Stripping once keeps genuinely quoted prompts intact.
func stripEchoQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

const previewTextMax = 600

func truncatePreview(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= previewTextMax {
		return string(r)
	}
	return string(r[:previewTextMax]) + "…"
}

func partType(p map[string]any) string {
	t, _ := p["type"].(string)
	return t
}

// ---- canonical part adaptation (shared by replay and live stream) ----

// outEvent is one cc-webui event derived from a single part.
type outEvent struct {
	Kind string // store event kind: "user" (prompt) | "stream"
	Ts   time.Time
	Data any // map[string]string{"text":...} or canonical stream object
}

// ToolTx tracks tool parts seen so far in one live turn. opencode may emit a
// tool part twice (status running, then completed), and a tool call must not
// render two tool cards or two results. Single-goroutine use only.
type ToolTx struct {
	assistantSent map[string]bool // callID -> assistant tool_use block emitted
	resultSent    map[string]bool // callID -> tool_result emitted
}

func NewToolTx() *ToolTx {
	return &ToolTx{assistantSent: map[string]bool{}, resultSent: map[string]bool{}}
}

// AdaptPart converts one assistant-side opencode part (live or export shape)
// into canonical stream event payloads. Live lines carry no role — opencode
// does not echo user prompts on stdout, so every adapted part is assistant
// side. User prompts are handled at the message level in ToEvents.
func AdaptPart(messageID string, part map[string]any, ts time.Time, tx *ToolTx) []any {
	outs := adaptPart(messageID, part, ts, tx)
	raw := make([]any, 0, len(outs))
	for _, o := range outs {
		raw = append(raw, o.Data)
	}
	return raw
}

func adaptPart(messageID string, part map[string]any, ts time.Time, tx *ToolTx) []outEvent {
	switch partType(part) {
	case "text":
		text, _ := part["text"].(string)
		if text == "" {
			return nil
		}
		// The part id rides as the block id so two text blocks of one step
		// (same message id, no other distinguishing key) keep distinct
		// fingerprints in mergeStoredAndClaude.
		block := map[string]any{"type": "text", "text": text}
		if id, _ := part["id"].(string); id != "" {
			block["id"] = id
		}
		return []outEvent{assistantEvent(messageID, ts, block)}
	case "tool":
		return adaptTool(part, messageID, ts, tx)
	default:
		// reasoning / patch / step-start / step-finish / future types:
		// nothing chat-visible in v1.
		return nil
	}
}

func adaptTool(part map[string]any, messageID string, ts time.Time, tx *ToolTx) []outEvent {
	callID, _ := part["callID"].(string)
	name, _ := part["tool"].(string)
	if callID == "" {
		return nil
	}
	state, _ := part["state"].(map[string]any)
	status, _ := state["status"].(string)

	var out []outEvent
	if !tx.assistantSent[callID] {
		tx.assistantSent[callID] = true
		block := map[string]any{"type": "tool_use", "id": callID}
		if name != "" {
			block["name"] = name
		}
		if input, ok := state["input"].(map[string]any); ok && len(input) > 0 {
			block["input"] = input
		}
		out = append(out, assistantEvent(messageID, ts, block))
	}
	if (status == "completed" || status == "error") && !tx.resultSent[callID] {
		tx.resultSent[callID] = true
		content := []any{}
		if text := toolOutputText(state["output"]); text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		block := map[string]any{
			"type":        "tool_result",
			"tool_use_id": callID,
			"content":     content,
		}
		if status == "error" {
			block["is_error"] = true
		}
		out = append(out, outEvent{
			Kind: "stream",
			Ts:   ts,
			Data: map[string]any{
				"type":    "user",
				"message": map[string]any{"role": "user", "content": []any{block}},
			},
		})
	}
	return out
}

// toolOutputText normalizes a tool part's state.output into display text.
// Output is usually a string; defensively stringify other shapes.
func toolOutputText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

// assistantEvent wraps one content block into a canonical assistant stream
// object (same shape claudehist.Convert produces for split lines).
func assistantEvent(messageID string, ts time.Time, block map[string]any) outEvent {
	msg := map[string]any{"role": "assistant", "content": []any{block}}
	if messageID != "" {
		msg["id"] = messageID
	}
	return outEvent{
		Kind: "stream",
		Ts:   ts,
		Data: map[string]any{"type": "assistant", "message": msg},
	}
}
