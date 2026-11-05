package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"cc-webui/internal/a2a"
	"cc-webui/internal/store"
)

// Transcript reading for the agentdeck MCP (read_session_messages /
// search_messages).
//
// The main agent dispatches work to a worker and then has no way to see what
// the worker actually produced until the worker reports back. These tools give
// it bounded read access to any session's transcript.
//
// Engine coverage comes for free: s.replayEvents already normalizes every
// engine (claude / qoder / opencode / mcode / codex) into the same canonical
// event shape, so there is no per-engine reader here. The projection keeps text
// only — assistant/user/peer text plus tool *names* — and drops thinking, tool
// inputs/outputs and images, so the tool cannot become a way to exfiltrate a
// whole transcript or a screenshot.

const (
	readDefaultLimit = 50
	readMaxLimit     = 200
	searchDefaultCap = 50
	searchMaxCap     = 100
	// searchBudget bounds a cross-session search; each opencode session costs a
	// CLI export (~1s), so an unbounded scan could run for minutes.
	searchBudget = 20 * time.Second
	// perMessageRunes caps one projected message so a single huge turn cannot
	// blow up the response.
	perMessageRunes = 4000
)

// handleReadSessionMessages is the read_session_messages MCP tool: a bounded
// page of a session's transcript, oldest→newest, projected to text.
func (s *Server) handleReadSessionMessages(session string, before, limit int) (string, error) {
	sess := s.resolveAddress(session)
	if sess == nil {
		return "", fmt.Errorf("no session with address or id: %s", session)
	}
	merged, err := s.replayEvents(sess)
	if err != nil {
		return "", err
	}
	total := len(merged)
	if limit <= 0 {
		limit = readDefaultLimit
	}
	if limit > readMaxLimit {
		limit = readMaxLimit
	}
	if before <= 0 || before > total {
		before = total
	}
	from := before - limit
	if from < 0 {
		from = 0
	}
	messages := make([]map[string]any, 0, before-from)
	for i := from; i < before; i++ {
		role, text, ok := projectEvent(merged[i])
		if !ok {
			continue
		}
		messages = append(messages, map[string]any{
			"index": i, "role": role, "text": truncateRunes(text, perMessageRunes),
		})
	}
	out := map[string]any{
		"session":  sessionAddress(sess),
		"id":       sess.ID,
		"engine":   s.engineOf(sess),
		"total":    total,
		"from":     from,
		"to":       before,
		"has_more": from > 0,
		"messages": messages,
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// handleSearchMessages is the search_messages MCP tool. With a session it scans
// just that session; without one it scans recent sessions until the hit cap or
// the time budget, so a broad search stays bounded.
func (s *Server) handleSearchMessages(query, session string, limit int) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", fmt.Errorf("search_messages requires 'query'")
	}
	if limit <= 0 {
		limit = searchDefaultCap
	}
	if limit > searchMaxCap {
		limit = searchMaxCap
	}
	needle := strings.ToLower(query)

	var sessions []*store.Session
	if strings.TrimSpace(session) != "" {
		target := s.resolveAddress(session)
		if target == nil {
			return "", fmt.Errorf("no session with address or id: %s", session)
		}
		sessions = []*store.Session{target}
	} else {
		sessions = s.searchableSessions()
	}

	deadline := time.Now().Add(searchBudget)
	hits := []map[string]any{}
	truncated := false
	for _, sess := range sessions {
		if len(hits) >= limit {
			truncated = true
			break
		}
		if time.Now().After(deadline) {
			truncated = true
			break
		}
		merged, err := s.replayEvents(sess)
		if err != nil {
			continue
		}
		for i, ev := range merged {
			if len(hits) >= limit {
				truncated = true
				break
			}
			role, text, ok := projectEvent(ev)
			if !ok {
				continue
			}
			if idx := strings.Index(strings.ToLower(text), needle); idx >= 0 {
				hits = append(hits, map[string]any{
					"session": sessionAddress(sess),
					"id":      sess.ID,
					"index":   i,
					"role":    role,
					"snippet": snippetAround(text, idx, len(query)),
				})
			}
		}
	}
	out := map[string]any{
		"query":     query,
		"hits":      hits,
		"truncated": truncated,
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// searchableSessions lists the sessions a cross-session search scans: ordinary,
// non-archived sessions, most recently updated first.
func (s *Server) searchableSessions() []*store.Session {
	var out []*store.Session
	for _, sess := range s.st.List() {
		if sess.GroupID != "" || sess.Archived {
			continue
		}
		out = append(out, sess)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// projectEvent reduces one canonical replay event to a {role, text} pair. ok is
// false for events that carry no user-visible text (turn boundaries, thinking,
// tool results, the engine's own echo of the user prompt).
func projectEvent(ev *store.Event) (role, text string, ok bool) {
	if ev == nil {
		return "", "", false
	}
	switch ev.Kind {
	case "user":
		var d struct {
			Text   string `json:"text"`
			Source string `json:"source"`
			From   string `json:"from"`
			Body   string `json:"body"`
		}
		if json.Unmarshal(ev.Data, &d) != nil {
			return "", "", false
		}
		if d.Source == a2a.Source || d.Body != "" {
			body := d.Body
			if body == "" {
				body = d.Text
			}
			if strings.TrimSpace(body) == "" {
				return "", "", false
			}
			if d.From != "" {
				return "peer", "[" + d.From + "] " + body, true
			}
			return "peer", body, true
		}
		if strings.TrimSpace(d.Text) == "" {
			return "", "", false
		}
		return "user", d.Text, true
	case "status":
		var d map[string]any
		if json.Unmarshal(ev.Data, &d) != nil {
			return "", "", false
		}
		if state, _ := d["state"].(string); state == "error" {
			msg, _ := d["error"].(string)
			if strings.TrimSpace(msg) == "" {
				return "", "", false
			}
			return "error", msg, true
		}
		return "", "", false
	case "stream":
		var d map[string]any
		if json.Unmarshal(ev.Data, &d) != nil {
			return "", "", false
		}
		// A peer message folded into a running turn carries an origin.
		if origin, _ := d["origin"].(map[string]any); origin != nil {
			name, _ := origin["name"].(string)
			body, _ := origin["body"].(string)
			if body == "" {
				body = textOfContent(d["message"])
			}
			if strings.TrimSpace(body) == "" {
				return "", "", false
			}
			if name != "" {
				return "peer", "[" + name + "] " + body, true
			}
			return "peer", body, true
		}
		typ, _ := d["type"].(string)
		if typ != "assistant" {
			// "user" is the engine echo of the prompt or a tool_result; both are
			// already represented, and tool output is deliberately dropped.
			return "", "", false
		}
		msg, _ := d["message"].(map[string]any)
		if msg == nil {
			return "", "", false
		}
		content, _ := msg["content"].([]any)
		text := projectContent(content)
		if strings.TrimSpace(text) == "" {
			return "", "", false
		}
		return "assistant", text, true
	}
	return "", "", false
}

// projectContent renders assistant content blocks to text: text blocks verbatim,
// tool_use as a name-only marker, thinking dropped.
func projectContent(content []any) string {
	var parts []string
	for _, c := range content {
		blk, ok := c.(map[string]any)
		if !ok {
			continue
		}
		switch blk["type"] {
		case "text":
			if s, _ := blk["text"].(string); strings.TrimSpace(s) != "" {
				parts = append(parts, s)
			}
		case "tool_use":
			name, _ := blk["name"].(string)
			if name == "" {
				name = "tool"
			}
			parts = append(parts, "[tool: "+name+"]")
		}
	}
	return strings.Join(parts, "\n")
}

// textOfContent pulls plain text out of a {content:[{type:text}]} message value.
func textOfContent(message any) string {
	m, _ := message.(map[string]any)
	if m == nil {
		return ""
	}
	content, _ := m["content"].([]any)
	return projectContent(content)
}

// snippetAround returns a window of text centered on the match at byte index
// idx (case-folded match length matchLen), capped so a hit stays readable.
func snippetAround(text string, idx, matchLen int) string {
	const window = 160
	start := idx - window
	if start < 0 {
		start = 0
	}
	end := idx + matchLen + window
	if end > len(text) {
		end = len(text)
	}
	// Do not cut mid-rune: back off to a boundary.
	for start > 0 && !utf8Start(text[start]) {
		start--
	}
	for end < len(text) && !utf8Start(text[end]) {
		end++
	}
	s := text[start:end]
	if start > 0 {
		s = "…" + s
	}
	if end < len(text) {
		s = s + "…"
	}
	return strings.TrimSpace(s)
}

// utf8Start reports whether b begins a UTF-8 rune (not a continuation byte).
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
