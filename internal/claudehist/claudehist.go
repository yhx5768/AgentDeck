// Package claudehist scans and converts Claude Code's own on-disk session
// history (~/.claude/projects/<slug>/<session-id>.jsonl).
package claudehist

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
	UUID      string `json:"uuid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Usage   json.RawMessage `json:"usage"`
	} `json:"message"`
	Summary     string `json:"summary"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	// Origin carries the CLI's attribution for injected turns. Peer messages
	// arrive as isMeta user lines with origin.kind=="peer" (name= sender's
	// display name, body= decoded message text).
	Origin *struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		From string `json:"from"`
		Body string `json:"body"`
	} `json:"origin"`
}

// isRealUserPrompt reports whether a jsonl line is a genuine user-typed prompt
// (not a tool_result echo, sidechain, meta, or system-injected control text).
func isRealUserPrompt(l histLine) (string, bool) {
	if l.Type != "user" || l.IsSidechain || l.IsMeta {
		return "", false
	}
	t := strings.TrimSpace(textOf(l.Message.Content))
	if t == "" || strings.HasPrefix(t, "<") || strings.HasPrefix(t, "[Request interrupted") {
		return "", false
	}
	return t, true
}

// HasUserPrompt reports whether the session jsonl still contains at least one
// real user-prompt line. After truncating before the first user message, the
// transcript has no user prompt left and can't be --resume'd ("No conversation
// found"); the caller should clear the claude session id so the next turn
// starts fresh instead.
func HasUserPrompt(cwd, sessionID string) bool {
	for _, l := range scanFile(HistoryPath(cwd, sessionID)) {
		if _, ok := isRealUserPrompt(l); ok {
			return true
		}
	}
	return false
}

// LastUserUUID returns the uuid of the most recent real user-prompt line in the
// session jsonl whose text matches matchText (trimmed). After a turn ends, the
// last matching line is that turn's prompt, so its uuid is the file-checkpoint
// id. Returns "" if no match (e.g. multimodal/image-only messages, or the
// prompt wasn't logged).
func LastUserUUID(cwd, sessionID, matchText string) string {
	matchText = strings.TrimSpace(matchText)
	if matchText == "" {
		return ""
	}
	var lastUUID string
	for _, l := range scanFile(HistoryPath(cwd, sessionID)) {
		if t, ok := isRealUserPrompt(l); ok && t == matchText {
			lastUUID = l.UUID
		}
	}
	return lastUUID
}

// TruncateBeforeUUID rewrites the session jsonl to keep only the lines BEFORE
// the line whose uuid == uuid (that line and everything after it is removed).
// It returns the number of lines removed. If the uuid is not found, the file is
// left untouched and an error is returned. The caller must back up the file
// first (truncation is irreversible).
//
// Lines are matched by raw byte index (not via scanFile, which skips
// unparseable lines) so the kept prefix is an exact byte-for-byte prefix.
func TruncateBeforeUUID(cwd, sessionID, uuid string) (int, error) {
	path := HistoryPath(cwd, sessionID)
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	// Preserve exact bytes: split on '\n'. A trailing newline yields no extra
	// empty entry after TrimRight.
	body := strings.TrimRight(string(raw), "\n")
	if body == "" {
		return 0, fmt.Errorf("checkpoint uuid %s not found in history", uuid)
	}
	rawLines := strings.Split(body, "\n")
	idx := -1
	for i, rl := range rawLines {
		var l histLine
		if json.Unmarshal([]byte(rl), &l) == nil && l.UUID == uuid {
			idx = i
			break
		}
	}
	if idx < 0 {
		return 0, fmt.Errorf("checkpoint uuid %s not found in history", uuid)
	}
	kept := rawLines[:idx]
	out := strings.Join(kept, "\n")
	if len(kept) > 0 {
		out += "\n"
	}
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return 0, err
	}
	return len(rawLines) - idx, nil
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
		if l.IsSidechain {
			continue
		}
		if l.IsMeta && !(l.Type == "user" && l.Origin != nil && l.Origin.Kind == "peer") {
			// 跳过元数据行，但放行入站 peer 消息——否则别的会话发来的消息
			// 在回放里完全消失（transcript 是其唯一持久层）。
			continue
		}
		ts, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
		switch l.Type {
		case "user":
			if l.Origin != nil && l.Origin.Kind == "peer" {
				// 入站 peer 消息：作为 stream 事件透传 origin，前端用
				// origin.body 渲染专用气泡（不落盘成 cc-webui 的 user 事件）。
				data, _ := json.Marshal(map[string]any{
					"type":   "user",
					"uuid":   l.UUID,
					"origin": l.Origin,
					"message": map[string]any{
						"role":    "user",
						"content": l.Message.Content,
					},
				})
				events = append(events, &store.Event{Ts: ts, Kind: "stream", Data: data})
				continue
			}
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
			// Forward id/model/usage so history replay carries per-step token
			// counts and split lines (thinking/tool_use blocks of one API step
			// share a message id) can be merged by the trajectory projection.
			msg := map[string]any{"role": "assistant", "content": l.Message.Content}
			if l.Message.ID != "" {
				msg["id"] = l.Message.ID
			}
			if l.Message.Model != "" {
				msg["model"] = l.Message.Model
			}
			if len(l.Message.Usage) > 0 {
				var usage any
				if json.Unmarshal(l.Message.Usage, &usage) == nil {
					msg["usage"] = usage
				}
			}
			data, _ := json.Marshal(map[string]any{
				"type":    "assistant",
				"message": msg,
			})
			events = append(events, &store.Event{Ts: ts, Kind: "stream", Data: data})
		}
	}
	return events, nil
}

// RenameSession appends a custom-title entry to the session's transcript —
// the Go port of the Agent SDK's rename_session(). The CLI reads the LAST
// custom-title from the file tail, so repeated calls are safe and the most
// recent wins. The title doubles as the session's peer-messaging address:
// a live process registers it in ~/.claude/sessions/<pid>.json at startup.
//
// Note: a running process absorbs the appended title into its cached metadata
// (the CLI tail-scans before re-appending), but its registry name is written
// at process start — callers renaming a live session should restart it for
// the new address to take effect.
func RenameSession(cwd, sessionID, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("title must be non-empty")
	}
	line, err := json.Marshal(map[string]string{
		"type":        "custom-title",
		"customTitle": title,
		"sessionId":   sessionID,
	})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(HistoryPath(cwd, sessionID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// RegistryEntry is one live-session record from ~/.claude/sessions/<pid>.json.
// The CLI writes it at process start (and refreshes status); Name is the
// peer-messaging address other sessions use in SendMessage.
type RegistryEntry struct {
	Pid        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	Name       string `json:"name"`
	NameSource string `json:"nameSource"` // "derived" = auto from cwd; absent = custom title
	Status     string `json:"status"`     // idle | busy | waiting
}

// ReadRegistryEntry returns the live-session registry entry for pid, or nil
// when the file is absent/unparseable (process not registered — e.g. old CLI
// without peerProtocol, or not fully started yet). Tries multiple home roots
// in case $HOME differs between cc-webui and the CLI process (e.g. systemd
// daemon, container with a clean env).
func ReadRegistryEntry(pid int) *RegistryEntry {
	home, herr := os.UserHomeDir()
	// 诊断：每个 PID 首次查询时把候选路径打到日志（高频路径会节流）。
	logOnceForPid(pid, home, herr)
	for _, root := range registryRoots(home) {
		raw, err := os.ReadFile(filepath.Join(root, ".claude", "sessions", fmt.Sprintf("%d.json", pid)))
		if err != nil {
			continue
		}
		var e RegistryEntry
		if json.Unmarshal(raw, &e) != nil || e.Pid == 0 {
			continue
		}
		return &e
	}
	return nil
}

// ReadRegistryForSession returns the live-session registry entry whose
// sessionId matches, or nil. Launcher-style CLIs (e.g. baidu ducc) fork the
// real claude as a child process, so the registry file is named after the
// child's pid, not the pid the host spawned — a sessionId scan finds it where
// ReadRegistryEntry(pid) can't.
func ReadRegistryForSession(sessionID string) *RegistryEntry {
	if sessionID == "" {
		return nil
	}
	home, _ := os.UserHomeDir()
	for _, root := range registryRoots(home) {
		dir := filepath.Join(root, ".claude", "sessions")
		names, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, de := range names {
			if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, de.Name()))
			if err != nil {
				continue
			}
			var e RegistryEntry
			if json.Unmarshal(raw, &e) != nil || e.Pid == 0 {
				continue
			}
			if e.SessionID == sessionID {
				return &e
			}
		}
	}
	return nil
}

// registryRoots returns candidate HOME directories in priority order:
//   - $HOME if set (CLI usually inherits this)
//   - os.UserHomeDir() (Go's fallback via /etc/passwd)
//   - common hardcoded paths for daemon/container setups
func registryRoots(primary string) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	if v := os.Getenv("HOME"); v != "" {
		add(v)
	}
	add(primary)
	if u, err := user.Current(); err == nil {
		add(u.HomeDir)
	}
	add("/home")
	// /home 下真正的 username 还得遍历；这里只在 /home/<user>/.claude 直接存在时尝试。
	if entries, err := os.ReadDir("/home"); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			add(filepath.Join("/home", e.Name()))
		}
	}
	return out
}

var (
	regLogMu      sync.Mutex
	regLogSeenPid = map[int]bool{}
)

func logOnceForPid(pid int, home string, herr error) {
	regLogMu.Lock()
	defer regLogMu.Unlock()
	if regLogSeenPid[pid] {
		return
	}
	regLogSeenPid[pid] = true
	if herr != nil {
		log.Printf("registry: pid=%d os.UserHomeDir error: %v; HOME=%q", pid, herr, os.Getenv("HOME"))
		return
	}
	log.Printf("registry: pid=%d scanning roots: HOME=%q UserHomeDir=%q", pid, os.Getenv("HOME"), home)
}
