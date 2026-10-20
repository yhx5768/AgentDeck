// Session discovery and transcript replay for the codex engine.
//
// Codex writes one rollout file per session:
//
//	$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<timestamp>-<uuid>.jsonl
//
// Each line is a typed envelope:
//
//	{"timestamp":"…","type":"session_meta","payload":{…}}
//	{"timestamp":"…","type":"turn_context","payload":{"model":"…","cwd":"…"}}
//	{"timestamp":"…","type":"response_item","payload":{"type":"message",…}}
//	{"timestamp":"…","type":"token_usage_record","payload":{"usage":{…}}}
//
// The replay adapter reads `response_item` (the model transcript: user and
// assistant messages, reasoning summaries, function/custom tool calls and
// their outputs) and `token_usage_record` (per-response usage). `event_msg`
// duplicates of the same content are ignored except as a fallback for tool
// items, because the response_item side carries the ids the tool pairing
// needs.
package codexhist

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cc-webui/internal/store"
)

// SessionsRel is the rollout tree relative to $CODEX_HOME.
const SessionsRel = "sessions"

// HomeEnv is the variable Codex itself uses to relocate its home directory.
const HomeEnv = "CODEX_HOME"

// SessionInfo is one codex session, normalized to the JSON shape
// claudehist.SessionInfo uses so the import/list API is engine-agnostic.
type SessionInfo struct {
	SessionID string    `json:"session_id"`
	Title     string    `json:"title"`
	Cwd       string    `json:"cwd"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PreviewMessage is one message in the import preview.
type PreviewMessage struct {
	Role string    `json:"role"`
	Text string    `json:"text"`
	TS   time.Time `json:"ts"`
}

// Session is one parsed rollout.
type Session struct {
	ID       string
	Title    string
	Cwd      string
	Provider string
	ModelID  string
	Created  time.Time
	Updated  time.Time

	Messages []SessionMessage
}

// SessionMessage is one displayable transcript step.
type SessionMessage struct {
	Kind      string // user | assistant | thinking | toolUse | toolResult | usage
	TS        time.Time
	TurnID    string
	MessageID string
	Text      string
	ToolID    string
	ToolName  string
	Input     map[string]any
	Output    string
	IsError   bool
	Usage     map[string]any
	Model     string
}

// ResolveHome picks the codex home for a cli. Precedence:
//
//  1. an explicit `home:` in the cli config (ducx, whose wrapper keeps its
//     state in ~/.baidu-cx and ignores CODEX_HOME);
//  2. $CODEX_HOME from the process environment;
//  3. ~/.baidu-cx for the Baidu wrapper binaries (ducx / baidu-codex);
//  4. ~/.codex.
func ResolveHome(cliHome, cliPath string) string {
	if h := expandHome(strings.TrimSpace(cliHome)); h != "" {
		return h
	}
	if h := strings.TrimSpace(os.Getenv(HomeEnv)); h != "" {
		return expandHome(h)
	}
	home, _ := os.UserHomeDir()
	base := strings.ToLower(filepath.Base(cliPath))
	if (base == "ducx" || base == "baidu-codex") && home != "" {
		if st, err := os.Stat(filepath.Join(home, ".baidu-cx")); err == nil && st.IsDir() {
			return filepath.Join(home, ".baidu-cx")
		}
	}
	return filepath.Join(home, ".codex")
}

func expandHome(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		if p == "~" {
			return home
		}
		return filepath.Join(home, p[2:])
	}
	return p
}

// SessionsDir returns <home>/sessions.
func SessionsDir(home string) string { return filepath.Join(home, SessionsRel) }

// List walks the rollout tree and returns every session, newest first.
// Listing parses only the head of each file (session metadata and the first
// real user prompt) plus the file mtime, so it stays cheap on a large tree.
func List(home string) ([]SessionInfo, error) {
	root := SessionsDir(home)
	entries := []SessionInfo{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), "rollout-") || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, ok := readSessionInfo(path)
		if ok {
			entries = append(entries, info)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return entries, nil
		}
		return nil, fmt.Errorf("扫描 codex 会话失败: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].UpdatedAt.After(entries[j].UpdatedAt) })
	return entries, nil
}

func readSessionInfo(path string) (SessionInfo, bool) {
	info := SessionInfo{SessionID: sessionIDFromPath(path)}
	if st, err := os.Stat(path); err == nil {
		info.UpdatedAt = st.ModTime()
	}
	f, err := os.Open(path)
	if err != nil {
		return SessionInfo{}, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for lines := 0; sc.Scan() && lines < 200; lines++ {
		var line rolloutLine
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		switch line.Type {
		case "session_meta":
			var m metaPayload
			if json.Unmarshal(line.Payload, &m) == nil {
				if m.SessionID != "" {
					info.SessionID = m.SessionID
				}
				info.Cwd = m.Cwd
				if t := parseTimestamp(m.Timestamp); !t.IsZero() {
					info.CreatedAt = t
				}
				if info.SessionID == "" {
					info.SessionID = m.ID
				}
			}
		case "response_item":
			var it responsePayload
			if json.Unmarshal(line.Payload, &it) == nil && it.Type == "message" && it.Role == "user" && isRealUser(it) {
				if t := userText(it.Content); t != "" && info.Title == "" {
					info.Title = truncate(t)
				}
			}
		}
		if info.Title != "" && !info.CreatedAt.IsZero() {
			break
		}
	}
	if info.SessionID == "" {
		return SessionInfo{}, false
	}
	if info.CreatedAt.IsZero() {
		info.CreatedAt = info.UpdatedAt
	}
	if info.Title == "" {
		info.Title = "(untitled)"
	}
	return info, true
}

// sessionIDFromPath extracts the uuid suffix from rollout-<ts>-<uuid>.jsonl.
func sessionIDFromPath(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if i := strings.LastIndex(base, "-"); i >= 0 {
		// The uuid itself contains hyphens; locate the first uuid-shaped run
		// from the right by taking the trailing 36 characters.
		if len(base) >= 36 {
			return base[len(base)-36:]
		}
	}
	return base
}

// FindSessionPath locates one rollout file by session id.
func FindSessionPath(home, sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	root := SessionsDir(home)
	suffix := "-" + sessionID + ".jsonl"
	var found string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), suffix) {
			found = path
		}
		return nil
	})
	return found, found != ""
}

// FetchSession parses one rollout from disk.
func FetchSession(home, sessionID string) (*Session, error) {
	path, ok := FindSessionPath(home, sessionID)
	if !ok {
		return nil, fmt.Errorf("codex session not found: %s", sessionID)
	}
	return ParseRollout(path)
}

// ParseRollout parses a rollout file. A truncated last line (a run still
// writing) is tolerated.
func ParseRollout(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取 codex transcript 失败（%s）: %w", path, err)
	}
	defer f.Close()

	sess := &Session{ID: sessionIDFromPath(path)}
	// seenTool records the call ids the response_item side already produced,
	// so the event_msg fallback does not add a second tool card for the same
	// call.
	seenTool := map[string]bool{}
	model := ""
	prevMsgTs := time.Time{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		var line rolloutLine
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		ts := parseTimestamp(line.Timestamp)
		if ts.IsZero() {
			ts = prevMsgTs
		}
		prevMsgTs = ts

		switch line.Type {
		case "session_meta":
			var m metaPayload
			if json.Unmarshal(line.Payload, &m) == nil {
				if m.SessionID != "" {
					sess.ID = m.SessionID
				}
				sess.Cwd = m.Cwd
				sess.Provider = m.ModelProvider
				if t := parseTimestamp(m.Timestamp); !t.IsZero() {
					sess.Created = t
				}
			}
		case "turn_context":
			var tc turnContextPayload
			if json.Unmarshal(line.Payload, &tc) == nil {
				if tc.Model != "" {
					model = tc.Model
					sess.ModelID = tc.Model
				}
				if sess.Cwd == "" && tc.Cwd != "" {
					sess.Cwd = tc.Cwd
				}
			}
		case "response_item":
			parseResponseItem(sess, line.Payload, ts, model, seenTool)
		case "token_usage_record":
			var tr usageRecordPayload
			if json.Unmarshal(line.Payload, &tr) == nil && len(tr.Usage) > 0 {
				sess.Messages = append(sess.Messages, SessionMessage{
					Kind: "usage", TS: ts, Usage: tr.Usage, Model: model,
				})
			}
		case "event_msg":
			parseEventMsg(sess, line.Payload, ts, seenTool)
		}
		if sess.Created.IsZero() && !ts.IsZero() {
			sess.Created = ts
		}
		if ts.After(sess.Updated) {
			sess.Updated = ts
		}
	}
	if sess.Created.IsZero() {
		sess.Created = sess.Updated
	}
	for _, m := range sess.Messages {
		if sess.Title == "" && m.Kind == "user" && m.Text != "" {
			sess.Title = truncate(m.Text)
			break
		}
	}
	if sess.Title == "" {
		sess.Title = "(untitled)"
	}
	return sess, nil
}

func parseResponseItem(sess *Session, raw json.RawMessage, ts time.Time, model string, seenTool map[string]bool) {
	var it responsePayload
	if json.Unmarshal(raw, &it) != nil {
		return
	}
	switch it.Type {
	case "message":
		switch it.Role {
		case "user":
			if !isRealUser(it) {
				return
			}
			if text := userText(it.Content); text != "" {
				sess.Messages = append(sess.Messages, SessionMessage{
					Kind: "user", TS: ts, Text: text, MessageID: it.ID, TurnID: it.Meta.TurnID,
				})
			}
		case "assistant":
			if text := assistantText(it.Content); text != "" {
				sess.Messages = append(sess.Messages, SessionMessage{
					Kind: "assistant", TS: ts, Text: text, MessageID: it.ID, TurnID: it.Meta.TurnID,
				})
			}
		}
	case "reasoning":
		if text := summaryText(it.Summary); text != "" {
			sess.Messages = append(sess.Messages, SessionMessage{
				Kind: "thinking", TS: ts, Text: text, MessageID: it.ID, TurnID: it.Meta.TurnID,
			})
		}
	case "function_call", "custom_tool_call":
		callID := it.CallID
		if callID == "" {
			callID = it.ID
		}
		if callID == "" {
			return
		}
		input := map[string]any{}
		if it.Type == "custom_tool_call" {
			if it.Input != "" {
				input["patch"] = it.Input
			}
		} else if it.Arguments != "" {
			if err := json.Unmarshal([]byte(it.Arguments), &input); err != nil {
				input["arguments"] = it.Arguments
			}
		}
		sess.Messages = append(sess.Messages, SessionMessage{
			Kind: "toolUse", TS: ts, TurnID: it.Meta.TurnID, ToolID: callID,
			ToolName: it.Name, Input: input,
		})
		seenTool[callID] = true
	case "function_call_output", "custom_tool_call_output":
		callID := it.CallID
		if callID == "" {
			return
		}
		var outVal any
		if len(it.Output) > 0 {
			_ = json.Unmarshal(it.Output, &outVal)
		}
		out := outputText(outVal)
		isErr := exitCodeError(out)
		sess.Messages = append(sess.Messages, SessionMessage{
			Kind: "toolResult", TS: ts, TurnID: it.Meta.TurnID, ToolID: callID,
			Output: out, IsError: isErr,
		})
	}
}

// parseEventMsg handles the event_msg fallback: only settled tool items whose
// call id was not already produced from the response_item side. Assistant
// messages, reasoning and user messages are taken from response_item, which
// carries the real ids.
func parseEventMsg(sess *Session, raw json.RawMessage, ts time.Time, seenTool map[string]bool) {
	var ev eventPayload
	if json.Unmarshal(raw, &ev) != nil || ev.Type != "item_completed" {
		return
	}
	item := ev.Item
	switch itemType(item) {
	case "CommandExecution":
		callID, _ := item["id"].(string)
		if callID == "" || seenTool[callID] {
			return
		}
		seenTool[callID] = true
		input := map[string]any{}
		if cmd := joinCommand(item["command"]); cmd != "" {
			input["command"] = cmd
		}
		sess.Messages = append(sess.Messages,
			SessionMessage{Kind: "toolUse", TS: ts, TurnID: ev.TurnID, ToolID: callID, ToolName: "exec_command", Input: input},
			SessionMessage{Kind: "toolResult", TS: ts, TurnID: ev.TurnID, ToolID: callID,
				Output: str(item["aggregated_output"]), IsError: numOf(item["exit_code"]) != 0},
		)
	case "FileChange":
		callID, _ := item["id"].(string)
		if callID == "" || seenTool[callID] {
			return
		}
		seenTool[callID] = true
		input := map[string]any{}
		if changes, ok := item["changes"]; ok && changes != nil {
			input["changes"] = changes
		}
		sess.Messages = append(sess.Messages,
			SessionMessage{Kind: "toolUse", TS: ts, TurnID: ev.TurnID, ToolID: callID, ToolName: "apply_patch", Input: input},
			SessionMessage{Kind: "toolResult", TS: ts, TurnID: ev.TurnID, ToolID: callID,
				Output: str(item["stdout"]) + str(item["stderr"]), IsError: statusOf(item) == "failed"},
		)
	}
}

// ---- replay: transcript -> canonical events ----

// ToEvents converts a parsed session into cc-webui events in conversation
// order. User prompts become kind:"user" events so the stored echo dedupes
// against them; everything else is a canonical stream event shaped exactly
// like the live adapter's output.
func (s *Session) ToEvents() []*store.Event {
	tx := NewToolTx()
	var evs []*store.Event
	for _, m := range s.Messages {
		switch m.Kind {
		case "user":
			data, _ := json.Marshal(map[string]string{"text": m.Text})
			evs = append(evs, &store.Event{Ts: m.TS, Kind: "user", Data: data})
		case "assistant":
			evs = append(evs, streamEvent(m.TS, assistantEvent(m.MessageID, m.TS, map[string]any{
				"type": "text", "id": m.MessageID, "text": m.Text,
			}, nil, modelMap(m.Model))))
		case "thinking":
			evs = append(evs, streamEvent(m.TS, assistantEvent(m.MessageID, m.TS, map[string]any{
				"type": "thinking", "id": m.MessageID, "thinking": m.Text,
			}, nil, modelMap(m.Model))))
		case "toolUse":
			if m.ToolID == "" || tx.assistantSent[m.ToolID] {
				continue
			}
			tx.assistantSent[m.ToolID] = true
			block := map[string]any{"type": "tool_use", "id": m.ToolID}
			if m.ToolName != "" {
				block["name"] = m.ToolName
			}
			if len(m.Input) > 0 {
				block["input"] = m.Input
			}
			evs = append(evs, streamEvent(m.TS, assistantEvent(m.ToolID, m.TS, block, nil, nil)))
		case "toolResult":
			if m.ToolID == "" || tx.resultSent[m.ToolID] {
				continue
			}
			tx.resultSent[m.ToolID] = true
			content := []any{}
			if m.Output != "" {
				content = append(content, map[string]any{"type": "text", "text": m.Output})
			}
			block := map[string]any{"type": "tool_result", "tool_use_id": m.ToolID, "content": content}
			if m.IsError {
				block["is_error"] = true
			}
			evs = append(evs, streamEvent(m.TS, map[string]any{
				"type": "user", "message": map[string]any{"role": "user", "content": []any{block}},
			}))
		case "usage":
			if payload := UsagePayload(m.Usage, m.Model, m.TS); payload != nil {
				evs = append(evs, streamEvent(m.TS, payload))
			}
		}
	}
	return evs
}

func streamEvent(ts time.Time, payload any) *store.Event {
	data, _ := json.Marshal(payload)
	return &store.Event{Ts: ts, Kind: "stream", Data: data}
}

func modelMap(model string) map[string]any {
	if model == "" {
		return nil
	}
	return map[string]any{"modelId": model}
}

// Preview mirrors claudehist.Preview: the most recent messages, text only,
// newest-limited, returned chronologically.
func (s *Session) Preview(maxMessages int) []PreviewMessage {
	var out []PreviewMessage
	for i := len(s.Messages) - 1; i >= 0 && len(out) < maxMessages; i-- {
		m := s.Messages[i]
		text := ""
		role := ""
		switch m.Kind {
		case "user":
			text, role = m.Text, "user"
		case "assistant":
			text, role = m.Text, "assistant"
		}
		if text == "" {
			continue
		}
		out = append(out, PreviewMessage{Role: role, Text: truncate(text), TS: m.TS})
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ---- on-disk shapes (only the keys we read) ----

type rolloutLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type metaPayload struct {
	SessionID     string `json:"session_id"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	Cwd           string `json:"cwd"`
	ModelProvider string `json:"model_provider"`
}

type turnContextPayload struct {
	Model string `json:"model"`
	Cwd   string `json:"cwd"`
}

type responsePayload struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	Output    json.RawMessage `json:"output"`
	Content   []contentBlock  `json:"content"`
	Summary   []contentBlock  `json:"summary"`
	Meta      responseMeta    `json:"internal_chat_message_metadata_passthrough"`
}

type responseMeta struct {
	TurnID           string   `json:"turn_id"`
	ContentItemKinds []string `json:"content_item_kinds"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type eventPayload struct {
	Type   string         `json:"type"`
	TurnID string         `json:"turn_id"`
	Item   map[string]any `json:"item"`
}

type usageRecordPayload struct {
	Usage map[string]any `json:"usage"`
}

func isRealUser(it responsePayload) bool {
	if it.Role != "user" {
		return false
	}
	// New rollouts tag real prompts with content_item_kinds ["user.text"];
	// synthetic lines (environment context, AGENTS.md instructions) carry a
	// different kind. Older rollouts have no metadata at all, so a plain user
	// message that is not an XML-ish injection still counts.
	for _, k := range it.Meta.ContentItemKinds {
		if k == "user.text" {
			return true
		}
	}
	if len(it.Meta.ContentItemKinds) > 0 {
		return false
	}
	text := userText(it.Content)
	if text == "" || strings.HasPrefix(text, "<") {
		return false
	}
	// Older rollouts have no content_item_kinds metadata; the AGENTS.md
	// injection is still distinguishable from a real prompt.
	if strings.HasPrefix(text, "# AGENTS.md instructions for ") || strings.Contains(text, "<INSTRUCTIONS>") {
		return false
	}
	return true
}

func userText(blocks []contentBlock) string {
	parts := []string{}
	for _, b := range blocks {
		if b.Type == "input_text" || b.Type == "text" {
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func assistantText(blocks []contentBlock) string {
	parts := []string{}
	for _, b := range blocks {
		if b.Type == "output_text" || b.Type == "text" {
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func summaryText(blocks []contentBlock) string {
	parts := []string{}
	for _, b := range blocks {
		if b.Type == "summary_text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(parts, "\n")
}

func joinCommand(v any) string {
	switch cmd := v.(type) {
	case string:
		return cmd
	case []any:
		parts := []string{}
		for _, c := range cmd {
			if s, _ := c.(string); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func exitCodeError(output string) bool {
	// function_call_output wraps shell results as
	// "…Process exited with code N…Output:\n…".
	i := strings.Index(output, "Process exited with code ")
	if i < 0 {
		return false
	}
	rest := output[i+len("Process exited with code "):]
	code := ""
	for _, r := range rest {
		if r >= '0' && r <= '9' {
			code += string(r)
			continue
		}
		break
	}
	return code != "" && code != "0"
}

func parseTimestamp(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t
	}
	return time.Time{}
}

func truncate(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len([]rune(s)) > 80 {
		return string([]rune(s)[:80])
	}
	return s
}
