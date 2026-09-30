// Session discovery and transcript replay for the mcode engine.
//
// mcode keeps each session in a dated directory under its runtime data dir:
//
//	<dataDir>/v2/sessions/YYYY/MM/DD/HH-MM-SS-mmm-session_<base64(sid)>/
//	    manifest.json     sessionId, created/updated timestamps
//	    messages.jsonl    the transcript: one {message_id, turn_id, message} per line
//
// There is no CLI surface for listing or exporting sessions (unlike
// opencode's `session list`/`export`), so — exactly like claudehist with
// ~/.claude/projects — the transcript is read from disk. Directories are
// located by reading each manifest.json rather than by decoding the
// base64 in the directory name: the naming is an internal detail that could
// change, the manifest is what the CLI itself treats as authoritative.
package mcodehist

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"cc-webui/internal/store"
)

// dataDirEnv overrides the mcode runtime data directory (tests, relocated
// profiles). MINIMAX_CONFIG_DIR only relocates the config file; sessions live
// under the data dir.
const dataDirEnv = "MINIMAX_DATA_DIR"

// defaultDataRel is the sessions root relative to the user's home.
const defaultDataRel = ".minimax"

// SessionInfo is one mcode session, normalized to the JSON shape
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
	Role string    `json:"role"` // user | assistant
	Text string    `json:"text"`
	TS   time.Time `json:"ts"`
}

// Session is one parsed mcode session.
type Session struct {
	ID        string
	Title     string
	Cwd       string
	CreatedAt time.Time
	UpdatedAt time.Time
	Messages  []SessionMessage
	// ModelID is the bare model id the session last ran on (from the
	// assistant messages' `model` field), used to preselect the model when
	// importing. ProviderID is the config provider that model belongs to.
	ModelID    string
	ProviderID string
}

// SessionMessage is one transcript line. Content blocks stay raw maps: the
// adapter reads a handful of keys and ignores the rest, so an mcode release
// adding a block type degrades to "not rendered" instead of a parse failure.
type SessionMessage struct {
	Role      string
	TS        time.Time
	Blocks    []map[string]any
	Usage     map[string]any
	Model     string
	Provider  string
	MessageID string
	// toolResult fields
	ToolCallID string
	ToolName   string
	IsError    bool
}

type fileManifest struct {
	SessionID   string `json:"sessionId"`
	CreatedAtMs int64  `json:"createdAtMs"`
	UpdatedAtMs int64  `json:"updatedAtMs"`
	Paths       struct {
		SessionDir string `json:"sessionDir"`
	} `json:"paths"`
}

type fileMessage struct {
	MessageID string `json:"message_id"`
	TurnID    string `json:"turn_id"`
	Message   struct {
		Role      string           `json:"role"`
		Content   []map[string]any `json:"content"`
		Timestamp any              `json:"timestamp"`
		Usage     map[string]any   `json:"usage"`
		Model     string           `json:"model"`
		Provider  string           `json:"provider"`
		// toolResult
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
		IsError    bool   `json:"isError"`
	} `json:"message"`
}

// sessionsRoot returns <dataDir>/v2/sessions.
func sessionsRoot() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(dataDirEnv)); dir != "" {
		return filepath.Join(dir, "v2", "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("mcode data dir: %w", err)
	}
	return filepath.Join(home, filepath.FromSlash(defaultDataRel), "v2", "sessions"), nil
}

// List returns every mcode session found on disk, newest first, by walking the
// dated tree and reading each manifest.
//
// mcode's runtime sqlite store also records cwd and title per session, but
// reading it would mean shipping a sqlite driver in the binary — this project
// deliberately keeps a single dependency (yaml) and a self-contained
// executable. The cost is that Cwd comes back empty: the dated directory
// layout records neither. Callers that need it (session import) leave the
// workspace for the user to pick, which the session editor already supports.
func List() ([]SessionInfo, error) {
	root, err := sessionsRoot()
	if err != nil {
		return nil, err
	}
	var out []SessionInfo
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, keep scanning
		}
		if d.IsDir() || d.Name() != "manifest.json" {
			return nil
		}
		if info, ok := readSessionInfo(filepath.Dir(path)); ok {
			out = append(out, info)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("扫描 mcode 会话失败: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// ListByCwd returns sessions whose recorded cwd is inside dir (or equals it).
// mcode has no project-bucketed list command, so filtering happens here.
func ListByCwd(dir string) ([]SessionInfo, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(dir) == "" {
		return all, nil
	}
	want := filepath.Clean(dir)
	var out []SessionInfo
	for _, s := range all {
		if s.Cwd == "" {
			continue
		}
		got := filepath.Clean(s.Cwd)
		if got == want || strings.HasPrefix(got, want+string(filepath.Separator)) {
			out = append(out, s)
		}
	}
	return out, nil
}

// FindSessionDir locates one session's transcript directory. The dated
// directory name ends in the base64 of the session id, but that encoding is an
// internal detail, so the match is made on the transcript's own manifest
// sessionId.
func FindSessionDir(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	root, err := sessionsRoot()
	if err != nil {
		return "", false
	}
	suffix := "-session_" + base64.RawURLEncoding.EncodeToString([]byte(sessionID))
	var found string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if !d.IsDir() || !strings.HasSuffix(d.Name(), suffix) {
			return nil
		}
		// Confirm with the manifest so a future naming change degrades to
		// "not found" rather than silently returning another session.
		raw, rerr := os.ReadFile(filepath.Join(path, "manifest.json"))
		if rerr != nil {
			return nil
		}
		var mf fileManifest
		if json.Unmarshal(raw, &mf) == nil && mf.SessionID == sessionID {
			found = path
		}
		return nil
	})
	return found, found != ""
}

func readSessionInfo(dir string) (SessionInfo, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return SessionInfo{}, false
	}
	var mf fileManifest
	if err := json.Unmarshal(raw, &mf); err != nil || mf.SessionID == "" {
		return SessionInfo{}, false
	}
	info := SessionInfo{
		SessionID: mf.SessionID,
		CreatedAt: time.UnixMilli(mf.CreatedAtMs),
		UpdatedAt: time.UnixMilli(mf.UpdatedAtMs),
	}
	// cwd and title are not in the manifest; take them from the transcript
	// (the first user message carries the session's working directory) or
	// the enclosing path.
	if sess, err := parseMessages(filepath.Join(dir, "messages.jsonl")); err == nil {
		info.Cwd = sess.Cwd
		info.Title = sess.Title
		if !info.CreatedAt.IsZero() && sess.CreatedAt.Before(info.CreatedAt) {
			info.CreatedAt = sess.CreatedAt
		}
	}
	return info, true
}

// FetchSession parses one session's transcript from disk.
func FetchSession(sessionID string) (*Session, error) {
	dir, ok := FindSessionDir(sessionID)
	if !ok {
		return nil, fmt.Errorf("mcode session not found: %s", sessionID)
	}
	sess, err := parseMessages(filepath.Join(dir, "messages.jsonl"))
	if err != nil {
		return nil, err
	}
	if mf, err := os.ReadFile(filepath.Join(dir, "manifest.json")); err == nil {
		var parsed fileManifest
		if json.Unmarshal(mf, &parsed) == nil {
			// The manifest is the only place the real session id lives —
			// messages.jsonl carries per-message ids (msg-user-v1-…), which
			// are NOT session ids and must never be handed back as one.
			if parsed.SessionID != "" {
				sess.ID = parsed.SessionID
			}
			if sess.CreatedAt.IsZero() {
				sess.CreatedAt = time.UnixMilli(parsed.CreatedAtMs)
			}
			if sess.UpdatedAt.IsZero() {
				sess.UpdatedAt = time.UnixMilli(parsed.UpdatedAtMs)
			}
		}
	}
	return sess, nil
}

// parseMessages reads a messages.jsonl transcript. A truncated last line (a
// run still writing) is tolerated: everything before it is kept.
func parseMessages(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取 mcode transcript 失败（%s）: %w", path, err)
	}
	defer f.Close()

	sess := &Session{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var fm fileMessage
		if err := json.Unmarshal(line, &fm); err != nil {
			continue
		}
		m := fm.Message
		sm := SessionMessage{
			Role:       m.Role,
			TS:         parseTimestamp(m.Timestamp),
			Blocks:     m.Content,
			Usage:      m.Usage,
			Model:      m.Model,
			Provider:   m.Provider,
			MessageID:  fm.MessageID,
			ToolCallID: m.ToolCallID,
			ToolName:   m.ToolName,
			IsError:    m.IsError,
		}
		sess.Messages = append(sess.Messages, sm)
		// The model rides on assistant messages; the last one wins.
		if m.Model != "" {
			sess.ModelID = BareModelID(m.Model)
			sess.ProviderID = m.Provider
		}
		if sess.ID == "" {
			// Fallback only: the manifest supplies the real session id (see
			// FetchSession). Never use a per-message id here — those look
			// similar but are not session ids.
			sess.ID = fm.MessageID
		}
		if sess.CreatedAt.IsZero() {
			sess.CreatedAt = sm.TS
		}
		if sm.TS.After(sess.UpdatedAt) {
			sess.UpdatedAt = sm.TS
		}
		if sess.Title == "" && m.Role == "user" {
			if t := firstUserText(sm); t != "" {
				sess.Title = truncatePreview(t)
			}
		}
	}
	if len(sess.Messages) == 0 {
		return sess, nil
	}
	return sess, nil
}

// parseTimestamp handles both numeric epoch forms mcode has used.
func parseTimestamp(v any) time.Time {
	switch n := v.(type) {
	case float64:
		// Values around 1e12 are milliseconds; smaller ones are seconds.
		if n > 1e11 {
			return time.UnixMilli(int64(n))
		}
		return time.Unix(int64(n), 0)
	case int64:
		if n > 1e11 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	case string:
		if t, err := time.Parse(time.RFC3339Nano, n); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ---- replay: transcript -> canonical events ----

// ToEvents converts a parsed session into cc-webui events in conversation
// order. User prompts become "user" events so the stored echo dedupes
// against them; everything assistant-side becomes canonical stream events
// shaped exactly like the live adapter's output.
func (s *Session) ToEvents() []*store.Event {
	tx := NewToolTx()
	var evs []*store.Event
	for _, m := range s.Messages {
		switch m.Role {
		case "user":
			if t := firstUserText(m); t != "" {
				data, _ := json.Marshal(map[string]string{"text": t})
				evs = append(evs, &store.Event{Ts: m.TS, Kind: "user", Data: data})
			}
		case "assistant":
			for i, b := range m.Blocks {
				for _, payload := range adaptBlock(m, b, i, m.TS, tx) {
					data, _ := json.Marshal(payload)
					evs = append(evs, &store.Event{Ts: m.TS, Kind: "stream", Data: data})
				}
			}
		case "toolResult":
			for _, payload := range adaptToolResult(m, tx) {
				data, _ := json.Marshal(payload)
				evs = append(evs, &store.Event{Ts: m.TS, Kind: "stream", Data: data})
			}
		}
	}
	return evs
}

// adaptBlock converts one assistant content block. Block ids are synthesized
// from the message id plus the block index so two text blocks of one step
// keep distinct fingerprints in the replay/merge dedup.
func adaptBlock(m SessionMessage, b map[string]any, idx int, ts time.Time, tx *ToolTx) []any {
	t, _ := b["type"].(string)
	blockID := fmt.Sprintf("%s#%d", m.MessageID, idx)
	switch t {
	case "text":
		text, _ := b["text"].(string)
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []any{assistantEvent(m.MessageID, ts, map[string]any{
			"type": "text", "id": blockID, "text": text,
		}, normalizeTranscriptUsage(m.Usage), modelMap(m))}
	case "thinking":
		thinking, _ := b["thinking"].(string)
		if strings.TrimSpace(thinking) == "" {
			return nil
		}
		return []any{assistantEvent(m.MessageID, ts, map[string]any{
			"type": "thinking", "id": blockID, "thinking": thinking,
		}, nil, modelMap(m))}
	case "toolCall":
		tc := map[string]any{
			"id":     b["id"],
			"name":   b["name"],
			"status": statusSucceeded, // replay only ever sees a settled call
		}
		if args, ok := b["arguments"].(map[string]any); ok && len(args) > 0 {
			tc["input"] = args
		}
		return adaptToolCall(tc, ts, tx)
	default:
		return nil
	}
}

// adaptToolResult renders the toolResult message that follows a tool call.
// The paired tool_use block is emitted from the assistant message's toolCall
// block, so this only needs the result side — but it still consults the tx
// so a result without a preceding call (truncated transcript) is not paired.
func adaptToolResult(m SessionMessage, tx *ToolTx) []any {
	if m.ToolCallID == "" || tx.resultSent[m.ToolCallID] {
		return nil
	}
	tx.resultSent[m.ToolCallID] = true
	content := []any{}
	for _, b := range m.Blocks {
		if t, _ := b["type"].(string); t == "text" {
			if s, _ := b["text"].(string); s != "" {
				content = append(content, map[string]any{"type": "text", "text": s})
			}
		}
	}
	block := map[string]any{
		"type":        "tool_result",
		"tool_use_id": m.ToolCallID,
		"content":     content,
	}
	if m.IsError {
		block["is_error"] = true
	}
	return []any{map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": []any{block}},
	}}
}

func modelMap(m SessionMessage) map[string]any {
	if m.Model == "" {
		return nil
	}
	return map[string]any{"modelId": m.Model}
}

// ---- user text extraction ----

// systemReminderRe matches the <system-reminder>…</system-reminder> block mcode
// prepends to every stored user message (it carries the session's runtime
// context). It must be stripped on replay: cc-webui stored the *original*
// prompt as its echo, and the two texts have to match for the count-based
// dedup in mergeStoredAndClaude to collapse them into one bubble.
var systemReminderRe = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// firstUserText returns the user's real prompt: the text blocks joined, with
// injected system reminders stripped. Messages that are purely injection
// (nothing left after stripping) yield "" and are dropped, mirroring
// claudehist.Convert's skip of system-injected lines.
func firstUserText(m SessionMessage) string {
	var parts []string
	for _, b := range m.Blocks {
		if t, _ := b["type"].(string); t != "text" {
			continue
		}
		text, _ := b["text"].(string)
		text = systemReminderRe.ReplaceAllString(text, "")
		if strings.TrimSpace(text) != "" {
			parts = append(parts, strings.TrimSpace(text))
		}
	}
	joined := strings.TrimSpace(strings.Join(parts, "\n"))
	// A prompt that is only an xml-ish injection is not real user input.
	if strings.HasPrefix(joined, "<") {
		return ""
	}
	return joined
}

// Preview mirrors claudehist.Preview: the most recent messages, text only,
// newest-limited, returned chronologically.
func (s *Session) Preview(maxMessages int) []PreviewMessage {
	var out []PreviewMessage
	seen := 0
	for i := len(s.Messages) - 1; i >= 0 && seen < maxMessages; i-- {
		m := s.Messages[i]
		var text string
		switch m.Role {
		case "user":
			text = firstUserText(m)
		case "assistant":
			var parts []string
			for _, b := range m.Blocks {
				if t, _ := b["type"].(string); t == "text" {
					if s, _ := b["text"].(string); strings.TrimSpace(s) != "" {
						parts = append(parts, s)
					}
				}
			}
			text = strings.TrimSpace(strings.Join(parts, "\n"))
		default:
			continue
		}
		if text == "" {
			continue
		}
		seen++
		out = append(out, PreviewMessage{Role: m.Role, Text: truncatePreview(text), TS: m.TS})
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

const previewTextMax = 600

func truncatePreview(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= previewTextMax {
		return string(r)
	}
	return string(r[:previewTextMax]) + "…"
}
