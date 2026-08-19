package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"cc-webui/internal/config"
	"cc-webui/internal/store"
)

// Status is the runtime state of a session.
type Status struct {
	Running   bool   `json:"running"`
	Queued    int    `json:"queued"`
	LastError string `json:"last_error,omitempty"`
}

// Image is an inline base64 image attached to a user message.
type Image struct {
	MediaType string `json:"media_type"` // e.g. "image/png"
	Data      string `json:"data"`       // base64-encoded (no data: prefix)
}

// Message is a user turn: text plus optional images. Images trigger the
// --input-format stream-json path; text-only messages use the simpler
// argv path.
type Message struct {
	Text   string  `json:"text"`
	Images []Image `json:"images,omitempty"`
}

type sessionRuntime struct {
	mu            sync.Mutex
	queue         []Message
	working       bool
	cancel        context.CancelFunc
	lastErr       string
	subs          map[chan *store.Event]struct{}
	statusCh      chan struct{} // closed/replaced on every status change
	streamSeq     int64        // counter for synthetic (non-persisted) seqs
	streamSeqBase int64        // base: max(persisted seq) + 1, set lazily
}

// Manager spawns claude processes and fans out stream events.
type Manager struct {
	cfg   *config.Config
	st    *store.Store
	mu    sync.Mutex
	procs map[string]*sessionRuntime
}

func NewManager(cfg *config.Config, st *store.Store) *Manager {
	return &Manager{cfg: cfg, st: st, procs: map[string]*sessionRuntime{}}
}

func (m *Manager) rt(sessionID string) *sessionRuntime {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.procs[sessionID]
	if !ok {
		r = &sessionRuntime{subs: map[chan *store.Event]struct{}{}, statusCh: make(chan struct{})}
		m.procs[sessionID] = r
	}
	return r
}

// GetStatus returns runtime status; unknown sessions are idle.
func (m *Manager) GetStatus(sessionID string) Status {
	r := m.rt(sessionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	return Status{Running: r.cancel != nil, Queued: len(r.queue), LastError: r.lastErr}
}

// StatusChanges returns a channel closed on the next status change (for polling/SSE).
func (m *Manager) StatusChanges(sessionID string) <-chan struct{} {
	r := m.rt(sessionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statusCh
}

func (r *sessionRuntime) notifyStatus() {
	close(r.statusCh)
	r.statusCh = make(chan struct{})
}

// Subscribe registers a listener; returned func unregisters.
func (m *Manager) Subscribe(sessionID string) (<-chan *store.Event, func()) {
	r := m.rt(sessionID)
	ch := make(chan *store.Event, 256)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		delete(r.subs, ch)
		r.mu.Unlock()
	}
}

// emit persists user/status events to the events file AND broadcasts.
// For kind=stream the event is broadcast only — the authoritative source
// for stream events is Claude's own on-disk jsonl, parsed live by the SSE
// handler. This avoids double storage and lets terminal-side updates show
// up in cc-webui after a reconnect.
func (m *Manager) emit(sessionID, kind string, data any) {
	if kind == "stream" {
		m.broadcast(sessionID, kind, data)
		return
	}
	ev, err := m.st.AppendEvent(sessionID, kind, data)
	if err != nil {
		log.Printf("append event: %v", err)
		return
	}
	m.broadcastEvent(sessionID, ev)
}

// broadcast sends a transient event (not persisted) to subscribers with a
// synthetic seq so the SSE handler can dedup on reconnect.
func (m *Manager) broadcast(sessionID, kind string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	r := m.rt(sessionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	seq := r.nextSyntheticSeq(m.st, sessionID)
	ev := &store.Event{Seq: seq, Ts: time.Now(), Kind: kind, Data: raw}
	for ch := range r.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (m *Manager) broadcastEvent(sessionID string, ev *store.Event) {
	r := m.rt(sessionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	for ch := range r.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// nextSyntheticSeq returns the next seq for transient (non-persisted)
// events on this session. It seeds itself from the highest seq already
// persisted to the events file, then increments. Values are unique within
// the lifetime of this process; SSE clients dedup by seq within a
// connection.
func (r *sessionRuntime) nextSyntheticSeq(st *store.Store, sessionID string) int64 {
	if r.streamSeqBase == 0 {
		max, _ := st.MaxSeq(sessionID)
		r.streamSeqBase = max + 1
	}
	r.streamSeq++
	return r.streamSeqBase - 1 + r.streamSeq
}

func (m *Manager) setRunning(sessionID string, cancel context.CancelFunc) {
	r := m.rt(sessionID)
	r.mu.Lock()
	r.cancel = cancel
	r.notifyStatus()
	r.mu.Unlock()
}

func (m *Manager) setIdle(sessionID string, lastErr string) {
	r := m.rt(sessionID)
	r.mu.Lock()
	r.cancel = nil
	r.lastErr = lastErr
	r.notifyStatus()
	r.mu.Unlock()
}

// Enqueue queues a user message and starts the worker if needed.
func (m *Manager) Enqueue(sessionID string, msg Message) error {
	sess := m.st.Get(sessionID)
	if sess == nil {
		return fmt.Errorf("session not found")
	}
	// Persist the user turn (text only — images are reconstructed from the
	// current draft on the client and aren't durable state).
	if _, err := m.st.AppendEvent(sessionID, "user", map[string]string{"text": msg.Text}); err != nil {
		return err
	}
	events, _ := m.st.Events(sessionID)
	if n := len(events); n > 0 {
		r0 := m.rt(sessionID)
		r0.mu.Lock()
		for ch := range r0.subs {
			select {
			case ch <- events[n-1]:
			default:
			}
		}
		r0.mu.Unlock()
	}

	r := m.rt(sessionID)
	r.mu.Lock()
	r.queue = append(r.queue, msg)
	r.notifyStatus()
	if r.working {
		r.mu.Unlock()
		return nil
	}
	r.working = true
	r.mu.Unlock()
	go m.work(sessionID)
	return nil
}

// Stop interrupts the currently running turn (kills the claude process tree).
func (m *Manager) Stop(sessionID string) {
	r := m.rt(sessionID)
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Manager) work(sessionID string) {
	r := m.rt(sessionID)
	for {
		r.mu.Lock()
		if len(r.queue) == 0 {
			r.working = false
			r.mu.Unlock()
			return
		}
		msg := r.queue[0]
		r.queue = r.queue[1:]
		r.notifyStatus()
		r.mu.Unlock()
		m.runTurn(sessionID, msg)
	}
}

func hasArg(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

func sanitizeName(name string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, name)
}

// SettingsPath returns the materialized --settings file path for a
// (cli, provider, model) triple. dataDir is absolute (guaranteed at config load).
func SettingsPath(dataDir, cliName, providerName, modelName string) string {
	return filepath.Join(dataDir, "settings", sanitizeName(cliName+"__"+providerName+"__"+modelName)+".json")
}

// settingsFile materializes env into a claude --settings JSON file under
// <data_dir>/settings/. Rewritten on every turn so config edits only need a
// process restart (not a session rebuild).
func (m *Manager) settingsFile(cliName, providerName, modelName string, env map[string]string) (string, error) {
	dir := filepath.Join(m.cfg.DataDir, "settings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	raw, err := json.Marshal(map[string]any{"env": env})
	if err != nil {
		return "", err
	}
	path := SettingsPath(m.cfg.DataDir, cliName, providerName, modelName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// runTurn spawns one claude process for a single user message. If the
// message contains images, claude is invoked with --input-format
// stream-json and the message is encoded as a single JSON user message
// on stdin; otherwise the text is piped via stdin as before.
func (m *Manager) runTurn(sessionID string, msg Message) {
	sess := m.st.Get(sessionID)
	if sess == nil {
		return
	}
	cli := m.cfg.CLIByName(sess.CLI)
	prov := m.cfg.ProviderByName(sess.Provider)
	if cli == nil {
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": "cli not found: " + sess.CLI})
		return
	}
	if prov == nil {
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": "provider not found: " + sess.Provider})
		return
	}

	m.emit(sessionID, "status", map[string]string{"state": "turn_start"})

	multimodal := len(msg.Images) > 0
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if multimodal {
		args = append(args, "--input-format", "stream-json")
	}
	if cli.PartialMessages && !hasArg(cli.Args, "--include-partial-messages") {
		args = append(args, "--include-partial-messages")
	}
	permMode := sess.PermissionMode
	if permMode == "" {
		permMode = cli.PermissionMode
	}
	if permMode != "" {
		args = append(args, "--permission-mode", permMode)
	}
	model := prov.EffectiveModel(sess.ModelOverride)
	if model != "" {
		args = append(args, "--model", model)
	}
	if sess.EffortOverride != "" {
		args = append(args, "--effort", sess.EffortOverride)
	}
	env := cli.EffectiveEnv(prov, model)
	if sess.ClaudeSessionID != "" {
		args = append(args, "--resume", sess.ClaudeSessionID)
	}
	args = append(args, cli.Args...)

	if len(env) > 0 && !hasArg(args, "--settings") {
		if p, err := m.settingsFile(cli.Name, prov.Name, model, env); err == nil {
			args = append(args, "--settings", p)
		} else {
			log.Printf("settings file for %s/%s/%s: %v", cli.Name, prov.Name, model, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.setRunning(sessionID, cancel)

	cmd := exec.CommandContext(ctx, cli.Path, args...)
	cmd.Dir = sess.Cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	envMap := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			envMap[kv[:i]] = kv[i+1:]
		}
	}
	for k, v := range env {
		envMap[k] = v
	}
	cmd.Env = make([]string, 0, len(envMap))
	for k, v := range envMap {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": err.Error()})
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": err.Error()})
		return
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": "start claude: " + err.Error()})
		return
	}
	if multimodal {
		content := []map[string]any{}
		if msg.Text != "" {
			content = append(content, map[string]any{"type": "text", "text": msg.Text})
		}
		for _, img := range msg.Images {
			content = append(content, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": img.MediaType,
					"data":       img.Data,
				},
			})
		}
		payload, _ := json.Marshal(map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": content},
		})
		stdin.Write(append(payload, '\n'))
	} else {
		stdin.Write([]byte(msg.Text))
	}
	stdin.Close()

	// Parse stream-json line by line and forward every event.
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(line, &obj); err != nil {
			continue // not JSON; ignore
		}
		m.emit(sessionID, "stream", obj)
		// Capture the claude session id so later turns can --resume it.
		if obj["type"] == "system" && obj["subtype"] == "init" {
			if sid, _ := obj["session_id"].(string); sid != "" && sid != sess.ClaudeSessionID {
				m.st.Upsert(sess, func(s *store.Session) { s.ClaudeSessionID = sid })
				sess.ClaudeSessionID = sid
			}
		}
	}

	waitErr := cmd.Wait()
	m.st.Upsert(sess, func(s *store.Session) { s.UpdatedAt = time.Now() })

	switch {
	case ctx.Err() != nil:
		m.emit(sessionID, "status", map[string]string{"state": "interrupted"})
		m.setIdle(sessionID, "interrupted")
	case waitErr != nil:
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = waitErr.Error()
		}
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": errMsg})
		m.setIdle(sessionID, errMsg)
	default:
		m.emit(sessionID, "status", map[string]string{"state": "turn_end"})
		m.setIdle(sessionID, "")
	}
}
