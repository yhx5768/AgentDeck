// Package claudehist scans and converts Claude Code's own on-disk session
// history (~/.claude/projects/<slug>/<session-id>.jsonl).
package claudehist

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cc-webui/internal/store"
)

// SessionInfo summarizes one on-disk claude session.
type SessionInfo struct {
	SessionID string    `json:"session_id"`
	Title     string    `json:"title"`
	Cwd       string    `json:"cwd"`
	UpdatedAt time.Time `json:"updated_at"`
	Lines     int       `json:"lines"`
}

// slugify mirrors claude's project directory naming: non-alphanumerics -> '-'.
func slugify(path string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, path)
}

func projectsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

// HistoryDir returns the history directory for a cwd ("" -> the projects root).
func HistoryDir(cwd string) string {
	if cwd == "" {
		return projectsDir()
	}
	return filepath.Join(projectsDir(), slugify(cwd))
}

// HistoryPath returns the full path to a specific claude session's jsonl.
func HistoryPath(cwd, sessionID string) string {
	return filepath.Join(HistoryDir(cwd), sessionID+".jsonl")
}

type histLine struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Summary     string `json:"summary"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
}

func scanFile(path string) []histLine {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []histLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		var l histLine
		if err := json.Unmarshal(sc.Bytes(), &l); err == nil {
			lines = append(lines, l)
		}
	}
	return lines
}

// textOf extracts plain user text from a message.content payload.
// content may be a string or a list of typed blocks.
func textOf(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// List returns on-disk claude sessions for cwd ("" lists every project),
// most recently modified first.
func List(cwd string) ([]*SessionInfo, error) {
	dirs := []string{}
	if cwd != "" {
		dirs = append(dirs, HistoryDir(cwd))
	} else {
		entries, err := os.ReadDir(projectsDir())
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(projectsDir(), e.Name()))
			}
		}
	}

	var out []*SessionInfo
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			full := filepath.Join(dir, e.Name())
			fi, err := e.Info()
			if err != nil {
				continue
			}
			info := &SessionInfo{
				SessionID: strings.TrimSuffix(e.Name(), ".jsonl"),
				UpdatedAt: fi.ModTime(),
			}
			for _, l := range scanFile(full) {
				info.Lines++
				if l.Cwd != "" {
					info.Cwd = l.Cwd
				}
				if info.Title == "" && l.Type == "user" && !l.IsSidechain && !l.IsMeta {
					if t := strings.TrimSpace(textOf(l.Message.Content)); t != "" &&
						!strings.HasPrefix(t, "<") { // skip system-injected xml-ish messages
						info.Title = t
						if len(info.Title) > 80 {
							info.Title = info.Title[:80] + "…"
						}
					}
				}
			}
			if info.Title == "" {
				info.Title = "(empty)"
			}
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// Convert transforms an on-disk claude history file into cc-webui events.
func Convert(cwd, sessionID string) ([]*store.Event, error) {
	path := filepath.Join(HistoryDir(cwd), sessionID+".jsonl")
	lines := scanFile(path)
	if len(lines) == 0 {
		return nil, os.ErrNotExist
	}
	var events []*store.Event
	for _, l := range lines {
		if l.IsSidechain || l.IsMeta {
			continue
		}
		ts, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
		switch l.Type {
		case "user":
			if t := strings.TrimSpace(textOf(l.Message.Content)); t != "" {
				// 跳过系统注入/控制类消息，它们不是真实用户输入：
				// xml-ish 注入（<task-notification> 等）和中断标记。
				if strings.HasPrefix(t, "<") || strings.HasPrefix(t, "[Request interrupted by user") {
					continue
				}
				data, _ := json.Marshal(map[string]string{"text": t})
				events = append(events, &store.Event{Ts: ts, Kind: "user", Data: data})
			} else if len(l.Message.Content) > 0 {
				// tool_result message: forward as a stream event for rendering.
				data, _ := json.Marshal(map[string]any{
					"type":    "user",
					"message": map[string]any{"role": "user", "content": l.Message.Content},
				})
				events = append(events, &store.Event{Ts: ts, Kind: "stream", Data: data})
			}
		case "assistant":
			data, _ := json.Marshal(map[string]any{
				"type":    "assistant",
				"message": map[string]any{"role": "assistant", "content": l.Message.Content},
			})
			events = append(events, &store.Event{Ts: ts, Kind: "stream", Data: data})
		}
	}
	return events, nil
}
