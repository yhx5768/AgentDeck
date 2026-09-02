package runner

import (
	"bufio"
	"bytes"
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

	"cc-webui/internal/claudehist"
	"cc-webui/internal/config"
	"cc-webui/internal/store"
)

// Status is the runtime state of a session.
type Status struct {
	Running   bool   `json:"running"`
	Queued    int    `json:"queued"`
	LastError string `json:"last_error,omitempty"`
	// Resident is true while a long-lived streaming-input process is alive
	// (peer-messaging reachable). RegisteredName is the name the CLI actually
	// registered under (~/.claude/sessions/<pid>.json) — it may differ from
	// the requested peer name when another live session holds it.
	Resident       bool   `json:"resident,omitempty"`
	RegisteredName string `json:"registered_name,omitempty"`
}

// Image is an inline base64 image attached to a user message.
type Image struct {
	MediaType string `json:"media_type"` // e.g. "image/png"
	Data      string `json:"data"`       // base64-encoded (no data: prefix)
}

// UploadedFile is a user-uploaded file that the server has persisted to disk
// (data/uploads/<sid>/). The agent is told the absolute path and reads or
// parses it with its own tools — the model API relays here reject inline
// document/file content blocks, so files cannot ride the multimodal channel.
type UploadedFile struct {
	Name string `json:"name"` // original file name (display only)
	Path string `json:"path"` // absolute on-disk path
}

// Message is a user turn: text plus optional images and files. Images trigger
// the --input-format stream-json path; text-only messages (with or without
// files, which are mentioned in the text) use the simpler argv path.
type Message struct {
	Text   string         `json:"text"`
	Images []Image        `json:"images,omitempty"`
	Files  []UploadedFile `json:"files,omitempty"`
}

// turnText composes the text actually sent to the CLI for a user turn: the
// user's text plus, for uploaded files, a mention block listing each file's
// on-disk path so the agent can Read or parse them. The composed string must
// be the single source of truth for every place that carries the turn's text:
// the persisted user event (merged with the CLI's own transcript echo by
// trimmed-text match in mergeStoredAndClaude), the stdin payload, and the
// captureCheckpoint match text — they all have to agree or replay dedup /
// rewind lookup break.
func turnText(msg Message) string {
	if len(msg.Files) == 0 {
		return msg.Text
	}
	var b strings.Builder
	b.WriteString(msg.Text)
	if msg.Text != "" {
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "（用户上传了 %d 个文件，已保存到磁盘：）", len(msg.Files))
	for _, f := range msg.Files {
		b.WriteString("\n- ")
		b.WriteString(f.Path)
	}
	return b.String()
}

type sessionRuntime struct {
	mu            sync.Mutex
	queue         []Message
	working       bool
	cancel        context.CancelFunc
	lastErr       string
	subs          map[chan *store.Event]struct{}
	statusCh      chan struct{} // closed/replaced on every status change
	streamSeq     int64         // counter for synthetic (non-persisted) seqs
	streamSeqBase int64         // base: max(persisted seq) + 1, set lazily
	resident      *residentProc // live resident process (nil in one-shot mode)
}

// Manager spawns claude processes and fans out stream events.
type Manager struct {
	cm    *config.Manager
	st    *store.Store
	mu    sync.Mutex
	procs map[string]*sessionRuntime
}

func NewManager(cm *config.Manager, st *store.Store) *Manager {
	return &Manager{cm: cm, st: st, procs: map[string]*sessionRuntime{}}
}

// cfg returns the live config (hot-reloadable via config.Manager.Watch).
func (m *Manager) cfg() *config.Config { return m.cm.Get() }

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
	// Fetch before r.mu: store lock must never be taken while holding r.mu.
	sess := m.st.Get(sessionID)
	r := m.rt(sessionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	st := Status{Running: r.cancel != nil, Queued: len(r.queue), LastError: r.lastErr}
	if r.resident != nil {
		select {
		case <-r.resident.dead:
		default:
			st.Resident = true
			name, _ := r.resident.regName()
			if name == "" && r.resident.registryRefreshDue() {
				// Registration may lag spawn (slow warmup) or be written by a
				// child process (launcher CLIs like ducc) — refresh on demand
				// so the frontend doesn't sit at "注册中" forever.
				if e := m.lookupRegistry(r.resident, sess); e != nil {
					r.resident.setRegName(e.Name, e.NameSource)
					name = e.Name
				}
			}
			st.RegisteredName = name
		}
	}
	return st
}

// LockForRewind atomically marks the session busy (so no turn can start) if it
// is currently idle. Returns false if a turn is running/queued (caller should
// 409). The rewind holds this lock so a message sent mid-rewind queues but
// doesn't start a worker that would race the truncation. Pair with
// UnlockFromRewind.
func (m *Manager) LockForRewind(sessionID string) bool {
	r := m.rt(sessionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil || r.working || len(r.queue) > 0 {
		return false
	}
	r.working = true
	r.cancel = func() {} // sentinel: GetStatus reports running while rewinding
	r.notifyStatus()
	return true
}

// UnlockFromRewind releases the rewind lock and, if a message was queued during
// the rewind, starts the worker to drain it.
func (m *Manager) UnlockFromRewind(sessionID string) {
	r := m.rt(sessionID)
	r.mu.Lock()
	r.cancel = nil
	r.working = false
	r.notifyStatus()
	start := len(r.queue) > 0
	if start {
		r.working = true
	}
	r.mu.Unlock()
	if start {
		go m.work(sessionID)
	}
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
	m.BroadcastEvent(sessionID, ev)
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

// BroadcastEvent sends an already-persisted event to all subscribers of a
// session (used by rewind to fan out the marker after truncating the log).
func (m *Manager) BroadcastEvent(sessionID string, ev *store.Event) {
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

// BroadcastConfigChanged notifies every connected SSE client that the config
// was hot-reloaded, so the UI can refresh its cli/provider/model lists. The
// event is transient (never persisted); the SSE handler renumbers seqs anyway.
func (m *Manager) BroadcastConfigChanged() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.procs))
	for id := range m.procs {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	ev := &store.Event{Kind: "config_changed", Ts: time.Now(), Data: json.RawMessage(`{}`)}
	for _, id := range ids {
		m.BroadcastEvent(id, ev)
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
	// current draft on the client and aren't durable state; uploaded files
	// are mentioned in the composed text so replay shows where they live).
	if _, err := m.st.AppendEvent(sessionID, "user", map[string]string{"text": turnText(msg)}); err != nil {
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
		// Resident sessions run turns on their long-lived streaming-input
		// process; everyone else spawns a one-shot process per turn.
		if sess := m.st.Get(sessionID); sess != nil && sess.Resident {
			m.runResidentTurn(sessionID, msg)
		} else {
			m.runTurn(sessionID, msg)
		}
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
//
// crossSessionInbound:"accept" is included unconditionally: cc-webui is a
// single-user console where inbound peer messages come from the user's own
// other sessions, and "hold" would park them forever (there is no interactive
// approver in headless mode). Repo/managed settings can still tighten it.
func (m *Manager) settingsFile(cliName, providerName, modelName string, env map[string]string) (string, error) {
	dir := filepath.Join(m.cfg().DataDir, "settings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	raw, err := json.Marshal(map[string]any{"env": env, "crossSessionInbound": "accept"})
	if err != nil {
		return "", err
	}
	path := SettingsPath(m.cfg().DataDir, cliName, providerName, modelName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// buildEnvMap builds the process environment for a claude spawn: the parent
// env, the CLI/provider/model env, and file checkpointing unconditionally
// enabled. Checkpointing is on for every turn so any user message can later be
// rewound (files restored by the CLI, conversation truncated by cc-webui).
func buildEnvMap(env map[string]string) map[string]string {
	envMap := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			envMap[kv[:i]] = kv[i+1:]
		}
	}
	for k, v := range env {
		envMap[k] = v
	}
	envMap["CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING"] = "true"
	return envMap
}

func envMapToSlice(envMap map[string]string) []string {
	out := make([]string, 0, len(envMap))
	for k, v := range envMap {
		out = append(out, k+"="+v)
	}
	return out
}

// ResolveCwd expands a leading "~" to the user's home directory and verifies
// the path exists and is a directory, returning the resolved path.
//
// os/exec does not expand "~", and a missing cmd.Dir makes cmd.Start fail
// with the baffling "fork/exec <binary>: no such file or directory" — which
// blames the binary, not the directory — when Setpgid is set. Callers must
// resolve and validate the cwd here so the real problem surfaces.
func ResolveCwd(dir string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("工作目录不能为空")
	}
	if strings.HasPrefix(dir, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("无法解析 ~（主目录不可用）：%w", err)
		}
		switch {
		case dir == "~":
			dir = home
		case strings.HasPrefix(dir, "~/"):
			dir = filepath.Join(home, dir[2:])
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("工作目录不存在：%s", dir)
		}
		return "", fmt.Errorf("无法访问工作目录 %s：%w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("工作目录不是目录：%s", dir)
	}
	return dir, nil
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
	cli := m.cfg().CLIByName(sess.CLI)
	prov := m.cfg().ProviderByName(sess.Provider)
	if cli == nil {
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": "cli not found: " + sess.CLI})
		return
	}
	if prov == nil {
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": "provider not found: " + sess.Provider})
		return
	}
	resolvedCwd, err := ResolveCwd(sess.Cwd)
	if err != nil {
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": err.Error()})
		return
	}

	m.emit(sessionID, "status", map[string]string{"state": "turn_start"})

	text := turnText(msg)
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
	cmd.Dir = resolvedCwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	envMap := buildEnvMap(env)
	cmd.Env = envMapToSlice(envMap)

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
		if text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
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
		stdin.Write([]byte(text))
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
		// Capture the claude session id and slash commands from the init event.
		if obj["type"] == "system" && obj["subtype"] == "init" {
			if sid, _ := obj["session_id"].(string); sid != "" && sid != sess.ClaudeSessionID {
				m.st.Upsert(sess, func(s *store.Session) { s.ClaudeSessionID = sid })
				sess.ClaudeSessionID = sid
			}
			if cmds, ok := obj["slash_commands"].([]any); ok && len(cmds) > 0 {
				names := make([]string, 0, len(cmds))
				for _, c := range cmds {
					if name, ok := c.(string); ok && name != "" {
						names = append(names, name)
					}
				}
				if len(names) > 0 {
					m.st.Upsert(sess, func(s *store.Session) { s.SlashCommands = names })
					sess.SlashCommands = names
				}
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
		// Surface structured claude-code errors in a friendlier shape and flag
		// errors that are known to occur after usable output has already been
		// streamed (e.g. model validation warnings from compatible CLIs).
		errMeta := parseClaudeCodeError(errMsg)
		if errMeta.Kind == "unrecognized_model" {
			m.emit(sessionID, "status", map[string]any{
				"state":          "error",
				"error":          errMeta.Message,
				"recoverable":    true,
				"original_error": errMsg,
			})
		} else {
			m.emit(sessionID, "status", map[string]string{"state": "error", "error": errMsg})
		}
		m.setIdle(sessionID, errMsg)
	default:
		m.emit(sessionID, "status", map[string]string{"state": "turn_end"})
		m.setIdle(sessionID, "")
		// Attach the file-checkpoint uuid to this turn's user event so the UI
		// can offer a rewind. Best-effort: silent on failure (no button).
		m.captureCheckpoint(sessionID, msg, sess)
	}
}

// claudeCodeError is a parsed [claude-code:<kind>] line from CLI stderr.
type claudeCodeError struct {
	Kind    string
	Message string
	Raw     string
}

// parseClaudeCodeError recognizes stderr lines like:
//
//	[claude-code:unrecognized_model] {"model":"glm-5.3-flash[1m]","query_source":"sdk"}
//
// and returns a human-readable message plus the structured kind. Unknown
// shapes return an empty Kind.
func parseClaudeCodeError(stderr string) claudeCodeError {
	for _, rawLine := range strings.Split(stderr, "\n") {
		line := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(line, "[claude-code:") {
			continue
		}
		end := strings.IndexByte(line, ']')
		if end < 0 {
			continue
		}
		kind := line[len("[claude-code:"):end]
		jsonPart := strings.TrimSpace(line[end+1:])
		switch kind {
		case "unrecognized_model":
			var payload struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal([]byte(jsonPart), &payload); err == nil && payload.Model != "" {
				return claudeCodeError{
					Kind:    kind,
					Message: fmt.Sprintf("CLI 报告模型未被识别：%s（但本轮输出可能已返回）", payload.Model),
					Raw:     stderr,
				}
			}
			return claudeCodeError{Kind: kind, Message: "CLI 报告模型未被识别（但本轮输出可能已返回）", Raw: stderr}
		}
	}
	return claudeCodeError{}
}

// captureCheckpoint finds the uuid claude logged for this turn's user prompt,
// writes it into the corresponding user event (data.checkpoint), and notifies
// connected clients to attach a rewind button to the last rendered user msg.
// No-op for multimodal/image-only messages (no matchable text) or if the uuid
// can't be found.
func (m *Manager) captureCheckpoint(sessionID string, msg Message, sess *store.Session) {
	text := turnText(msg)
	if sess.ClaudeSessionID == "" || strings.TrimSpace(text) == "" {
		return
	}
	uuid := claudehist.LastUserUUID(sess.Cwd, sess.ClaudeSessionID, text)
	if uuid == "" {
		return
	}
	events, err := m.st.Events(sessionID)
	if err != nil {
		return
	}
	var userSeq int64 = -1
	for _, ev := range events {
		if ev.Kind == "user" {
			userSeq = ev.Seq // the most recent user event is this turn's
		}
	}
	if userSeq < 0 {
		return
	}
	if _, err := m.st.UpdateEvent(sessionID, userSeq, func(ev *store.Event) {
		var d map[string]string
		if json.Unmarshal(ev.Data, &d) != nil {
			d = map[string]string{}
		}
		d["checkpoint"] = uuid
		raw, _ := json.Marshal(d)
		ev.Data = raw
	}); err != nil {
		log.Printf("capture checkpoint: %v", err)
		return
	}
	// Transient live notification: connected clients attach a rewind button to
	// their last rendered user message. On reconnect the button comes from the
	// persisted data.checkpoint in the replayed user event.
	m.broadcast(sessionID, "checkpoint", map[string]string{"checkpoint": uuid})
}

// RewindFiles spawns a one-shot claude process to rewind the session's files to
// the state at the given checkpoint uuid (a user-message uuid). It returns a
// short human-readable detail string and an error if the rewind failed (CLI
// stderr is surfaced). Does not touch the conversation.
func (m *Manager) RewindFiles(sess *store.Session, checkpoint string) (string, error) {
	if sess.ClaudeSessionID == "" {
		return "", fmt.Errorf("session has no claude session id")
	}
	cli := m.cfg().CLIByName(sess.CLI)
	if cli == nil {
		return "", fmt.Errorf("cli not found: %s", sess.CLI)
	}
	prov := m.cfg().ProviderByName(sess.Provider)
	model := ""
	var env map[string]string
	if prov != nil {
		model = prov.EffectiveModel(sess.ModelOverride)
		env = cli.EffectiveEnv(prov, model)
	}
	args := []string{
		"-p", "--resume", sess.ClaudeSessionID,
		"--rewind-files", checkpoint,
		"--output-format", "json",
	}
	args = append(args, cli.Args...)
	if len(env) > 0 && !hasArg(args, "--settings") {
		if p, err := m.settingsFile(cli.Name, prov.Name, model, env); err == nil {
			args = append(args, "--settings", p)
		} else {
			log.Printf("settings file for rewind %s/%s/%s: %v", cli.Name, prov.Name, model, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resolvedCwd, err := ResolveCwd(sess.Cwd)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, cli.Path, args...)
	cmd.Dir = resolvedCwd
	cmd.Env = envMapToSlice(buildEnvMap(env))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// No prompt: --rewind-files performs the restore and exits. Close stdin
	// immediately (mirrors the SDK's empty-prompt rewind pattern).
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start claude: %w", err)
	}
	stdin.Close()
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return "", fmt.Errorf("rewind timed out")
	}
	if waitErr != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = waitErr.Error()
		}
		return "", fmt.Errorf("%s", errMsg)
	}
	return parseRewindDetail(stdout.String()), nil
}

// parseRewindDetail extracts a short summary from the --rewind-files CLI output.
// The flag is undocumented, so parse defensively: scan JSON lines for a result
// or rewind-result object; fall back to a generic success message.
func parseRewindDetail(stdout string) string {
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			continue
		}
		if r, ok := obj["rewind_files_result"].(map[string]any); ok {
			if s := summarizeRewindResult(r); s != "" {
				return s
			}
		}
		if t, _ := obj["type"].(string); t == "result" {
			if isErr, _ := obj["is_error"].(bool); isErr {
				if sub, _ := obj["subtype"].(string); sub != "" {
					return sub
				}
			}
		}
	}
	return "文件已回滚"
}

// summarizeRewindResult turns a rewind result object into a zh summary. Field
// names are speculative (the schema is undocumented); unknown shapes fall back
// to the empty string so the caller uses the generic message.
func summarizeRewindResult(r map[string]any) string {
	count := func(keys ...string) int {
		for _, k := range keys {
			if v, ok := r[k]; ok {
				switch n := v.(type) {
				case int:
					return n
				case float64:
					return int(n)
				case []any:
					return len(n)
				}
			}
		}
		return 0
	}
	var parts []string
	if n := count("restored_files", "restored", "modified"); n > 0 {
		parts = append(parts, fmt.Sprintf("%d 个文件恢复", n))
	}
	if n := count("deleted_files", "deleted", "created"); n > 0 {
		parts = append(parts, fmt.Sprintf("%d 个新建文件删除", n))
	}
	if n := count("skipped_links", "skippedLinks", "skipped"); n > 0 {
		parts = append(parts, fmt.Sprintf("%d 个跳过", n))
	}
	return strings.Join(parts, "，")
}
