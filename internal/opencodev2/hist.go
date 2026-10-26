// Package opencodev2 reads OpenCode 2 session data and converts it into
// cc-webui events. It is the v2 counterpart of opencodehist: OpenCode 2 is a
// breaking rewrite with a different CLI surface and a different projected
// transcript shape, so it gets its own reader instead of a pile of version
// branches inside opencodehist.
//
// Everything here goes through the CLI's official surfaces, always with
// `--standalone` so a read never attaches to (or starts) the user's shared
// background service:
//
//	opencode session list --format json --standalone     (project bucket)
//	opencode api session.list --standalone               (all projects)
//	opencode session export <id> --standalone            (one session)
//	opencode api model.list --standalone                 (model catalogue)
//
// Conversion rules (same canonical shapes opencodehist produces, so the
// frontend and the stats aggregator need no changes):
//
//	user message text            -> kind=user event {text}
//	assistant content text       -> stream assistant event, one text block
//	assistant content reasoning  -> stream assistant event, one thinking block
//	assistant content tool       -> stream assistant event (tool_use) +
//	                                stream user event (tool_result)
//	agent/model/location/idle    -> skipped
package opencodev2

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
)

// SessionInfo mirrors opencodehist.SessionInfo (same JSON shape).
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

// Session is one parsed `session export` document.
type Session struct {
	ID              string
	Title           string
	Directory       string
	Agent           string
	ModelProviderID string
	ModelID         string
	UpdatedAt       time.Time
	CreatedAt       time.Time
	Messages        []SessionMessage
}

// SessionMessage is one message of an exported v2 session.
type SessionMessage struct {
	Role string
	ID   string
	TS   time.Time
	Text string // user messages
	// Content is the assistant message's ordered parts.
	Content []ContentPart
	// Tokens is the assistant step's usage.
	Tokens MessageTokens
	// Model the step ran on.
	ModelProviderID string
	ModelID         string
}

// ContentPart is one assistant content entry.
type ContentPart struct {
	Type string // "text" | "reasoning" | "tool"
	Text string
	// Tool fields.
	CallID   string
	ToolName string
	Status   string // streaming | running | completed | error
	Input    map[string]any
	Output   string
	IsError  bool
}

// MessageTokens is one assistant step's usage.
type MessageTokens struct {
	Input      int64
	Output     int64
	Reasoning  int64
	CacheRead  int64
	CacheWrite int64
}

// Total returns the step's full token volume.
func (t MessageTokens) Total() int64 {
	return t.Input + t.Output + t.Reasoning + t.CacheRead + t.CacheWrite
}

// execTimeout bounds every opencode CLI invocation. A `--standalone` command
// boots a private server before answering, so the budget is generous.
const execTimeout = 45 * time.Second

// runCLI executes the opencode binary with args (already including
// --standalone) and returns stdout. stderr is folded into the error.
func runCLI(cliPath string, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cliPath, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Capture stdout to a temp file: a large export exceeds the 64KiB pipe
	// buffer and the CLI exits before flushing the rest (same fix as
	// opencodehist).
	tmp, err := os.CreateTemp("", "opencode-v2-*.json")
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

// List returns the sessions of one project bucket (the process cwd's project),
// newest first. dir selects the bucket; empty keeps cc-webui's own cwd.
func List(cliPath, dir string) ([]SessionInfo, error) {
	raw, err := runCLI(cliPath, dir, "session", "list", "--format", "json", "--standalone")
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

// ListAll returns every top-level session across all projects in one call.
// v2 has no project-bucket restriction on the HTTP session list, so this is a
// single `api session.list` — unlike v1, which had to fall back to a raw
// sqlite SELECT because `session list` was bucket-scoped.
//
// `parentID=null` hides subagent (child) sessions, matching `session list`.
func ListAll(cliPath string) ([]SessionInfo, error) {
	raw, err := runCLI(cliPath, "", "api", "session.list", "--param", "parentID=null", "--standalone")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Data []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Time  struct {
				Created int64 `json:"created"`
				Updated int64 `json:"updated"`
			} `json:"time"`
			Location struct {
				Directory string `json:"directory"`
			} `json:"location"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse opencode session list: %w", err)
	}
	out := make([]SessionInfo, 0, len(doc.Data))
	for _, r := range doc.Data {
		out = append(out, SessionInfo{
			SessionID: r.ID,
			Title:     r.Title,
			Cwd:       r.Location.Directory,
			CreatedAt: msTime(r.Time.Created),
			UpdatedAt: msTime(r.Time.Updated),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// FetchSession exports one session (`session export <id> --standalone`).
func FetchSession(cliPath, sessionID string) (*Session, error) {
	raw, err := runCLI(cliPath, "", "session", "export", sessionID, "--standalone")
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
			ID    string `json:"id"`
			Title string `json:"title"`
			Agent string `json:"agent"`
			Model *struct {
				ID         string `json:"id"`
				ProviderID string `json:"providerID"`
			} `json:"model"`
			Time struct {
				Created int64 `json:"created"`
				Updated int64 `json:"updated"`
			} `json:"time"`
			Location struct {
				Directory string `json:"directory"`
			} `json:"location"`
		} `json:"info"`
		Messages []rawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	s := &Session{
		ID:        doc.Info.ID,
		Title:     doc.Info.Title,
		Directory: doc.Info.Location.Directory,
		Agent:     doc.Info.Agent,
		CreatedAt: msTime(doc.Info.Time.Created),
		UpdatedAt: msTime(doc.Info.Time.Updated),
	}
	if doc.Info.Model != nil {
		s.ModelProviderID = doc.Info.Model.ProviderID
		s.ModelID = doc.Info.Model.ID
	}
	for _, m := range doc.Messages {
		msg := SessionMessage{
			Role:    m.Type,
			ID:      m.ID,
			TS:      msTime(m.Time.Created),
			Text:    m.Text,
			Content: m.contentParts(),
		}
		if m.Model != nil {
			msg.ModelProviderID = m.Model.ProviderID
			msg.ModelID = m.Model.ID
		}
		if m.Tokens != nil {
			msg.Tokens = MessageTokens{
				Input:      m.Tokens.Input,
				Output:     m.Tokens.Output,
				Reasoning:  m.Tokens.Reasoning,
				CacheRead:  m.Tokens.Cache.Read,
				CacheWrite: m.Tokens.Cache.Write,
			}
		}
		s.Messages = append(s.Messages, msg)
	}
	return s, nil
}

// rawMessage is the wire shape of one v2 exported message.
type rawMessage struct {
	ID   string `json:"id"`
	Type string `json:"type"` // user | assistant | idle | system | …
	Text string `json:"text"`
	Time struct {
		Created int64 `json:"created"`
	} `json:"time"`
	Model *struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
	} `json:"model"`
	Tokens *struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Content []struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		ID    string `json:"id"`
		Name  string `json:"name"`
		State *struct {
			Status  string         `json:"status"`
			Input   map[string]any `json:"input"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"state"`
	} `json:"content"`
}

func (m rawMessage) contentParts() []ContentPart {
	out := make([]ContentPart, 0, len(m.Content))
	for _, c := range m.Content {
		p := ContentPart{Type: c.Type, Text: c.Text}
		switch c.Type {
		case "tool":
			p.CallID = c.ID
			p.ToolName = c.Name
			if c.State != nil {
				p.Status = c.State.Status
				p.Input = c.State.Input
				var texts []string
				for _, cc := range c.State.Content {
					if cc.Type == "text" && cc.Text != "" {
						texts = append(texts, cc.Text)
					}
				}
				p.Output = strings.Join(texts, "\n")
				p.IsError = c.State.Status == "error"
			}
		}
		out = append(out, p)
	}
	return out
}

// Preview returns the most recent messages, text only, newest-limited count,
// in chronological order.
func (s *Session) Preview(maxMessages int) []PreviewMessage {
	var out []PreviewMessage
	seen := 0
	for i := len(s.Messages) - 1; i >= 0 && seen < maxMessages; i-- {
		m := s.Messages[i]
		var texts []string
		switch m.Role {
		case "user":
			if strings.TrimSpace(m.Text) != "" {
				texts = append(texts, m.Text)
			}
		case "assistant":
			for _, p := range m.Content {
				if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
					texts = append(texts, p.Text)
				}
			}
		default:
			continue
		}
		if len(texts) == 0 {
			continue
		}
		seen++
		out = append(out, PreviewMessage{
			Role: m.Role,
			Text: truncatePreview(strings.Join(texts, "\n")),
			TS:   m.TS,
		})
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
