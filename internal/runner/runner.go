package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"cc-webui/internal/claudehist"
	"cc-webui/internal/codexhist"
	"cc-webui/internal/config"
	"cc-webui/internal/mcodehist"
	"cc-webui/internal/opencodeapi"
	"cc-webui/internal/opencodehist"
	"cc-webui/internal/opencodever"
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

	// Session Goal bookkeeping (see goal.go). goalAwait marks the injected
	// evaluation turn; goalVerdict/goalTail hold what the model said about it.
	// Guarded by mu like everything else here.
	goalAwait   bool
	goalVerdict string
	goalTail    string
}

// Manager spawns claude processes and fans out stream events.
type Manager struct {
	cm    *config.Manager
	st    *store.Store
	mu    sync.Mutex
	procs map[string]*sessionRuntime
	perm  *permissionState
	grp   *groupState
	// ocPool owns the `opencode serve` child processes shared by every
	// opencode-kind session (see ocserve.go).
	ocPool *opencodeServePool
	// ocTurns tracks each session's live opencode turn so Stop() can
	// interrupt it over HTTP.
	ocTurns *ocTurnRegistry
}

func NewManager(cm *config.Manager, st *store.Store) *Manager {
	m := &Manager{cm: cm, st: st, procs: map[string]*sessionRuntime{}, perm: newPermissionState(), grp: newGroupState()}
	m.ocPool = newOpencodeServePool(m)
	m.ocTurns = newOcTurnRegistry()
	return m
}

// Close shuts down every child process the manager owns, including the shared
// opencode serve processes (they hold provider credentials in memory).
func (m *Manager) Close() {
	if m.ocPool != nil {
		m.ocPool.Close()
	}
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

// busyLocked reports whether the session has any turn activity: a turn in
// flight (cancel set — includes resident turns and the rewind sentinel), a
// worker draining the queue, or queued messages. Callers must hold r.mu.
// GetStatus only exposes running + queue length; busy additionally covers the
// brief window where the worker already popped a message but the turn's cancel
// handle is not installed yet.
func (r *sessionRuntime) busyLocked() bool {
	return r.cancel != nil || r.working || len(r.queue) > 0
}

// Busy reports whether the session has any in-flight or queued turn activity.
// The SSE handler uses it to tell a genuinely running turn apart from a stale
// persisted turn_start left behind by a killed backend process.
func (m *Manager) Busy(sessionID string) bool {
	r := m.rt(sessionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.busyLocked()
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
	if r.busyLocked() {
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
		// Stream events are never persisted, so the goal loop reads its verdict
		// off this path instead of the event log.
		m.noteGoalOutput(sessionID, assistantTextOf(data))
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
	// A turn that ended (clean or error) produced output the user hasn't seen
	// yet — mark it unread so the sidebar shows a blue dot on every client
	// until one of them opens it. An interrupt produced no fresh result, so
	// skip it (mirrors the client's err === 'interrupted' guard). The runner
	// is the single writer for the set; clients only ever clear it. st is nil
	// only in tests that exercise Busy semantics with a store-less manager.
	if m.st != nil && lastErr != "interrupted" {
		if err := m.st.SetUnread(sessionID, true); err != nil {
			log.Printf("set unread for %s: %v", sessionID, err)
		}
	}
	// A Session Goal feeds itself another turn here. Only on a clean end: an
	// error or an interrupt means the round produced nothing to judge, and
	// auto-continuing would paper over a broken turn (or race the user's own
	// stop). advanceGoal is a no-op without an active goal.
	if lastErr == "" {
		go m.advanceGoal(sessionID)
	}
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
	// opencode turns echo only the raw prompt text: files ride as `-f`
	// attachments outside the text, and opencode's export re-states the
	// prompt verbatim (quoted) — mergeStoredAndClaude's text-match dedup
	// needs stored echo == replayed echo.
	echoText := turnText(msg)
	if cli := m.cm.Get().CLIByName(sess.CLI); cli != nil && cli.IsOpenCode() {
		echoText = msg.Text
	}
	if _, err := m.st.AppendEvent(sessionID, "user", map[string]string{"text": echoText}); err != nil {
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

// Stop interrupts the currently running turn.
//
// For the opencode engine the turn lives in a long-lived `opencode serve`
// process, so cancelling the request context is not enough — the POST would
// return but the agent would keep working and keep writing parts. Ask
// opencode to stop the turn first (POST /api/session/{id}/interrupt), then
// cancel as the backstop. Every other engine keeps the process-kill path.
func (m *Manager) Stop(sessionID string) {
	// Stopping a turn also stops the automation behind it: leaving the goal
	// active would queue another round the moment the interrupted turn ends.
	m.StopGoal(sessionID)
	if m.interruptOpenCodeTurn(sessionID) {
		return
	}
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
		m.resetGoalTurn(sessionID)
		// Resident sessions run turns on their long-lived streaming-input
		// process; everyone else spawns a one-shot process per turn.
		if sess := m.st.Get(sessionID); sess != nil && sess.Resident {
			m.runResidentTurn(sessionID, msg)
		} else {
			m.runTurn(sessionID, msg)
		}
	}
}

// mimeForFile guesses the MIME type of an uploaded file from its extension.
// opencode's file part requires `mime` to be present, and rejects the part
// otherwise; the guess is display/metadata only (the payload rides the url).
func mimeForFile(name string) string {
	t := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	if t == "" {
		return "application/octet-stream"
	}
	return t
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
//
// effort (the session's --effort override, "" = inherit) is part of the file
// name because the file carries CLAUDE_CODE_EFFORT_LEVEL: a resident session
// holds its settings file for its whole lifetime, so two sessions on the same
// model with different efforts must not overwrite each other's copy.
func SettingsPath(dataDir, cliName, providerName, modelName, effort string) string {
	return SettingsPathWithPerm(dataDir, cliName, providerName, modelName, effort, false)
}

// SettingsPathWithPerm extends SettingsPath with the permission-hook marker:
// hook-carrying files get a "__perm" suffix so a bypass session and an
// approval session on the same model never overwrite each other's copy.
// The resume-command display path (EnsureSettingsFile) stays hook-free on
// purpose: a terminal answers prompts interactively itself, and the command
// stays portable (no data_dir absolute path) for env-less providers.
func SettingsPathWithPerm(dataDir, cliName, providerName, modelName, effort string, withHook bool) string {
	name := cliName + "__" + providerName + "__" + modelName
	if effort != "" {
		name += "__effort-" + effort
	}
	if withHook {
		name += "__perm"
	}
	return filepath.Join(dataDir, "settings", sanitizeName(name)+".json")
}

// settingsFile materializes env into a claude --settings JSON file under
// <data_dir>/settings/. Rewritten on every turn so config edits only need a
// process restart (not a session rebuild).
//
// effort, when non-empty, is written as CLAUDE_CODE_EFFORT_LEVEL — see
// applyEffortEnv for why the --effort flag alone is not enough.
//
// crossSessionInbound:"accept" rides along in every file we do write: cc-webui
// is a single-user console where inbound peer messages come from the user's
// own other sessions, and "hold" would park them forever (there is no
// interactive approver in headless mode). Repo/managed settings can still
// tighten it. Providers with no config env get no file at all, so their
// sessions carry no pin and inbound peer messages follow the CLI default.
func (m *Manager) settingsFile(cliName, providerName, modelName, effort string, env map[string]string) (string, error) {
	return m.settingsFileEx(cliName, providerName, modelName, effort, env, false)
}

// settingsFileEx is settingsFile plus the interactive-approval hook. withHook
// injects a PermissionRequest hook pointing at this binary's -perm-helper so
// tool prompts in headless -p runs are answered from the web UI instead of
// being auto-denied. Only the spawn path uses it; the resume-command display
// path (EnsureSettingsFile) stays hook-free (see SettingsPathWithPerm).
func (m *Manager) settingsFileEx(cliName, providerName, modelName, effort string, env map[string]string, withHook bool) (string, error) {
	dir := filepath.Join(m.cfg().DataDir, "settings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		out[k] = v
	}
	applyEffortEnv(out, effort)
	doc := map[string]any{"env": out, "crossSessionInbound": "accept"}
	if withHook {
		doc["hooks"] = map[string]any{
			"PermissionRequest": []any{
				map[string]any{
					"matcher": "*",
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": permHelperCommand(),
							"timeout": 60,
						},
					},
				},
			},
		}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	path := SettingsPathWithPerm(m.cfg().DataDir, cliName, providerName, modelName, effort, withHook)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// permHelperCommand returns the shell command the CLI runs for every
// PermissionRequest hook invocation: this same binary in helper mode. The
// session/address/token travel via env (CC_WEBUI_PERM_*) so the shared
// settings file needs no per-session templating.
func permHelperCommand() string {
	ex, err := os.Executable()
	if err != nil || ex == "" {
		ex = os.Args[0]
	}
	return fmt.Sprintf("%q -perm-helper", ex)
}

// permLoopbackAddr derives the helper's dial address from the server listen
// address: the helper always dials 127.0.0.1:<port> regardless of the listen
// bind (0.0.0.0/:789 etc. all serve loopback).
func permLoopbackAddr(listen string) string {
	port := listen
	if i := strings.LastIndex(listen, ":"); i >= 0 && i+1 < len(listen) {
		port = listen[i+1:]
	}
	if port == "" {
		return listen
	}
	// listen may already be "127.0.0.1:8321" — LastIndex handles it; a bare
	// ":8321" or "0.0.0.0:8321" both collapse to loopback + port.
	return "127.0.0.1:" + port
}

// EnsureSettingsFile materializes the --settings file for a
// (cli, provider, model, effort) tuple and returns its path, for callers that
// only *display* the path (the resume command in the UI). The CLI hard-fails
// on a missing settings file ("Settings file not found"), and effort-scoped
// files only exist once a session with that level has run a turn — so a
// command copied right after picking an effort would point at nothing.
//
// No env means no file — the same rule the spawn paths follow (see
// runStreamJSONTurn): a --settings path is only worth handing out when the env it
// carries is the reason the CLI cannot work without it. A pinned effort alone
// does not qualify — the command passes --effort instead of pointing a
// terminal at this checkout's data dir.
//
// Falls back to the computed path when the write fails.
func (m *Manager) EnsureSettingsFile(cliName, providerName, modelName, effort string, env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	if p, err := m.settingsFile(cliName, providerName, modelName, effort, env); err == nil {
		return p
	} else {
		log.Printf("settings file for %s/%s/%s: %v", cliName, providerName, modelName, err)
	}
	return SettingsPath(m.cfg().DataDir, cliName, providerName, modelName, effort)
}

// applyEffortEnv mirrors the session's --effort level into
// CLAUDE_CODE_EFFORT_LEVEL: the spawn's process env, plus — for providers that
// carry config env — the --settings file the caller materializes.
//
// The env var **beats** the --effort flag inside the CLI, and a level from the
// user's global ~/.claude/settings.json beats the process env. So a session
// pinned to high could still ask gateways for whatever the global file said —
// e.g. "max", which SenseNova's endpoint rejects ("field ReasoningEffort
// invalid, should be one of: low, medium, high, xhigh, none") — and the
// dropdown looked broken. Only a --settings file of our own outranks the
// global one, which is why env-carrying providers get the level written there;
// env-less providers (ducc-models) fall back to the process env, where a
// global level still wins over the session's.
//
// Verified on CLI 2.1.226 by intercepting the request body: `--effort high`
// with a global max still sent output_config.effort=max; adding
// CLAUDE_CODE_EFFORT_LEVEL=high to the --settings file sent high.
func applyEffortEnv(env map[string]string, effort string) {
	if effort != "" {
		env["CLAUDE_CODE_EFFORT_LEVEL"] = effort
	}
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

// runTurn dispatches one user message to the session's engine: claude- and
// qoder-kind CLIs spawn a one-shot `-p --output-format stream-json` process
// (runStreamJSONTurn, one argv dialect each), opencode-kind CLIs drive a
// shared `opencode serve` process over HTTP+SSE (runOpenCodeTurn), mcode-kind
// CLIs spawn `mcode exec` (runMcodeTurn), codex-kind CLIs spawn
// `codex exec --json` (runCodexTurn).
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

	if cli.IsOpenCode() {
		// OpenCode 2's server API is a breaking rewrite (path prefixes, Basic
		// auth, a different event vocabulary); the live turn over it is not
		// wired yet. Reads (replay/import/session list/model catalog) already
		// route through opencodev2, so a v2 binary is usable for everything
		// except starting a new turn. Fail loudly instead of driving a v2
		// server with v1 calls (which would hang on a 401 with no explanation).
		if opencodever.IsV2(cli.Path, cli.OpenCodeAPI) {
			const msgText = "OpenCode v2 的实况对话尚未接入（回放/导入/会话列表/模型目录已支持 v2）。" +
				"可在 config.yaml 该 cli 上设 `opencode_api: v1` 用 v1 协议，或换回 v1 二进制。"
			m.emit(sessionID, "status", map[string]string{"state": "error", "error": msgText})
			m.setIdle(sessionID, msgText)
			return
		}
		m.runOpenCodeTurn(sessionID, msg, sess, cli, prov, resolvedCwd)
		return
	}
	if cli.IsMcode() {
		m.runMcodeTurn(sessionID, msg, sess, cli, prov, resolvedCwd)
		return
	}
	if cli.IsCodex() {
		m.runCodexTurn(sessionID, msg, sess, cli, prov, resolvedCwd)
		return
	}
	// claude-kind and qoder-kind CLIs share one spawn path: Qoder CLI CN is a
	// Claude Code fork, so only the argv spelling differs.
	m.runStreamJSONTurn(sessionID, msg, sess, cli, prov, resolvedCwd, dialectFor(cli))
}

// streamJSONDialect captures the argv-level differences between the CLIs that
// all speak the same `-p --output-format stream-json` protocol and write the
// same transcript layout. Everything else about the turn — stdin payload, event
// passthrough, session-id capture, error surfacing — is shared.
type streamJSONDialect struct {
	// verbose appends --verbose. Claude Code requires it alongside
	// stream-json; Qoder CLI CN has no such flag and exits 1 on it
	// ("error: unknown option '--verbose'").
	verbose bool
	// effortFlag names the reasoning-level flag. Same levels, different
	// spelling: claude takes --effort, qoder --reasoning-effort (and rejects
	// --effort outright).
	effortFlag string
	// permissionMode normalizes the mode spelling for this CLI.
	permissionMode func(string) string
	// effortEnv mirrors the level into the process env, which the CLI prefers
	// over the flag. Only claude's CLAUDE_CODE_EFFORT_LEVEL is known to do
	// that, so qoder leaves it off.
	effortEnv bool
	// checkpoint captures a file checkpoint after the turn, which is what
	// makes a message rewindable. claude restores those files itself
	// (`--rewind-files`); qoder exposes no such flag, so its sessions are not
	// rewindable and no checkpoint is captured.
	checkpoint bool
}

// claudeDialect is Claude Code's argv spelling.
var claudeDialect = streamJSONDialect{
	verbose:        true,
	effortFlag:     "--effort",
	permissionMode: func(s string) string { return s },
	effortEnv:      true,
	checkpoint:     true,
}

// qoderDialect is Qoder CLI CN's argv spelling (verified against 1.1.65:
// --verbose and --effort are both rejected, --permission-mode takes
// snake_case, and --reasoning-effort accepts auto/none/low/medium/high/
// xhigh/max/ultracode).
var qoderDialect = streamJSONDialect{
	verbose:        false,
	effortFlag:     "--reasoning-effort",
	permissionMode: snakeCasePermissionMode,
	effortEnv:      false,
	checkpoint:     false,
}

func dialectFor(cli *config.CLI) streamJSONDialect {
	if cli.IsQoder() {
		return qoderDialect
	}
	return claudeDialect
}

// snakeCasePermissionMode converts claude's camelCase permission modes to the
// snake_case spelling Qoder CLI CN expects (bypassPermissions ->
// bypass_permissions). Values that are already snake_case, or that the CLI
// rejects anyway, pass through untouched.
func snakeCasePermissionMode(mode string) string {
	var b strings.Builder
	for i, r := range mode {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// runStreamJSONTurn spawns one process for a single user message. If the
// message contains images, the CLI is invoked with --input-format stream-json
// and the message is encoded as a single JSON user message on stdin;
// otherwise the text is piped via stdin as before.
func (m *Manager) runStreamJSONTurn(sessionID string, msg Message, sess *store.Session, cli *config.CLI, prov *config.Provider, resolvedCwd string, d streamJSONDialect) {
	text := turnText(msg)
	multimodal := len(msg.Images) > 0
	args := []string{"-p", "--output-format", "stream-json"}
	if d.verbose {
		args = append(args, "--verbose")
	}
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
		args = append(args, "--permission-mode", d.permissionMode(permMode))
	}
	// A cli that owns its model list (models_command) has no meaningful
	// config-side default: the static provider list is only hint data. An
	// empty override means "let the launcher pick", not "pin the first model
	// config.yaml still remembers" (ducc's old [1m] aliases are now rejected).
	model := sess.ModelOverride
	if model == "" && len(cli.ModelsCommand) == 0 {
		model = prov.EffectiveModel("")
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if sess.EffortOverride != "" {
		args = append(args, d.effortFlag, sess.EffortOverride)
	}
	env := cli.EffectiveEnv(prov, model)
	// Interactive approval (PermissionRequest hook) needs a --settings file
	// even when there is no config env: without the file the CLI has nowhere
	// to discover the hook and prompts are auto-denied headless. Env-less
	// providers (ducc-models) otherwise skip the file for portability (see
	// SettingsPathWithPerm); the hook is the one exception worth pinning.
	withHook := permissionHookWanted(cli, sess.PermissionMode)
	// Only config env earns a --settings file (endpoint/token the CLI would not
	// otherwise have). An env-less provider such as ducc-models gets its
	// endpoint and per-model setup from the launcher itself and its level from
	// --effort plus the process env (applyEffortEnv), so there is nothing worth
	// pinning the spawn to <data_dir>/settings/ — unless the approval hook
	// needs a home (see above).
	needsSettings := len(env) > 0 || withHook
	if d.effortEnv {
		applyEffortEnv(env, sess.EffortOverride)
	}
	if sess.ClaudeSessionID != "" {
		args = append(args, "--resume", sess.ClaudeSessionID)
	}
	args = append(args, cli.Args...)
	args = m.appendHTMLRenderHint(args, cli)

	if needsSettings && !hasArg(args, "--settings") {
		if p, err := m.settingsFileEx(cli.Name, prov.Name, model, sess.EffortOverride, env, withHook); err == nil {
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
	if withHook {
		envMap["CC_WEBUI_PERM_SESSION"] = sessionID
		envMap["CC_WEBUI_PERM_ADDR"] = permLoopbackAddr(m.cfg().Listen)
		envMap["CC_WEBUI_PERM_TOKEN"] = m.cfg().GatewayToken
	}
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
				m.st.Upsert(sess, store.WithClaudeSID(sid))
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
		// Only dialects whose CLI can restore those files get one.
		if d.checkpoint {
			m.captureCheckpoint(sessionID, msg, sess)
		}
	}
}

// runOpenCodeTurn runs one user message through the opencode engine over its
// headless HTTP server (`opencode serve`), not the one-shot `opencode run`.
//
// Why: `run` cannot carry an approval card or a user-question prompt. In
// v1.18.35 it hardcodes a ruleset that DENIES the `question` / `plan_enter` /
// `plan_exit` permissions and auto-replies to any surviving permission event
// with `once` or `reject`; its `--format json` emitter never serializes
// permission/question events at all. The serve surface exposes both, plus
// interrupt and file-rewind, so this is what makes the approval card, the
// AskUserQuestion card and rewind available on this engine.
//
// Flow: acquire a shared serve process for this (cli, provider, model) env,
// create or resume the opencode session, POST the prompt, and drive the turn
// from the /event SSE bus (the same canonical mapping as the replay path —
// opencodehist.AdaptPart). The POST returns when the turn settles; pending
// permission/question events that arrive meanwhile are turned into cc-webui
// cards and answered back over the matching reply endpoints.
//
// The transcript stays in opencode's own store and is still read via
// `opencode export` on later SSE connects.
func (m *Manager) runOpenCodeTurn(sessionID string, msg Message, sess *store.Session, cli *config.CLI, prov *config.Provider, resolvedCwd string) {
	model := prov.EffectiveModel(sess.ModelOverride)

	srv, hub, release, err := m.ocPool.acquire(context.Background(), cli, prov, model, resolvedCwd)
	if err != nil {
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": err.Error()})
		m.setIdle(sessionID, err.Error())
		return
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	m.setRunning(sessionID, cancel)

	turn := newOpenCodeTurn(srv, hub, m, sessionID)
	turn.dir = resolvedCwd
	turn.ctx = ctx
	m.ocTurns.set(sessionID, turn)
	defer m.ocTurns.clear(sessionID, turn)

	// Create or adopt the opencode session before prompting. Permission mode
	// rides the create call as a ruleset — this is where cc-webui's
	// permission_mode finally gets a real landing point on this engine.
	ocSID := sess.ClaudeSessionID
	adopted := ocSID != ""
	if !adopted {
		created, err := turn.createSession(sess, cli, prov, model, resolvedCwd)
		if err != nil {
			cancel()
			m.emit(sessionID, "status", map[string]string{"state": "error", "error": err.Error()})
			m.setIdle(sessionID, err.Error())
			return
		}
		ocSID = created
		m.st.Upsert(sess, store.WithClaudeSID(ocSID))
		sess.ClaudeSessionID = ocSID
	}
	turn.ocSessionID = ocSID

	// Adopted session: align opencode's model with the session's current
	// cc-webui profile. opencode binds the model at creation time, so without
	// this a hot-switched model / provider / effort would silently keep using
	// the old one — the other three engines re-read sess.ModelOverride on
	// every turn, and opencode would be the odd one out. A freshly created
	// session already carries the right model from the create call above.
	if adopted {
		if _, err := turn.ensureModel(ctx, prov, model, sess.EffortOverride); err != nil {
			cancel()
			m.emit(sessionID, "status", map[string]string{"state": "error", "error": err.Error()})
			m.setIdle(sessionID, err.Error())
			return
		}
	}

	// Subscribe before prompting so no event of this turn can slip through the
	// gap between the POST starting and the stream being read.
	events, unsub := hub.Subscribe(ocSID)
	defer unsub()
	go turn.consume(events)
	// The /event fan-out drops ordinary events when a session's buffer is full
	// (token deltas can burst past 256). Prompt events must not be lost: a
	// missed question keeps opencode blocked forever, with no card and no 55s
	// timeout. This poller is the at-least-once safety net.
	go turn.reconcileLoop()

	// v1 prompt shape: {model, parts}. See opencodeapi.PromptInput for why the
	// v2 envelope must not be used here.
	//
	// opencode's part shapes are strict: a "text" part requires a non-empty
	// `text`, and a "file" part requires `mime` + `url` and rejects `text`
	// outright. So the text part is only added when there is text (an
	// image-only paste has none), and attachments carry their payload in `url`.
	var prompt opencodeapi.PromptInput
	if msg.Text != "" {
		prompt.Parts = append(prompt.Parts, opencodeapi.PromptPart{Type: "text", Text: msg.Text})
	}
	if ref := openCodeModelV1(prov, model); ref != nil {
		prompt.Model = ref
	}
	// Attachments ride as file parts. This is the channel the old
	// `opencode run -f` path lacked: there, an uploaded image had to degrade
	// into a path mention in the text; here a vision-capable model receives
	// the image itself. A data: uri carries inline bytes, a file:// uri points
	// at something already on disk.
	for _, img := range msg.Images {
		prompt.Parts = append(prompt.Parts, opencodeapi.PromptPart{
			Type: "file",
			Mime: img.MediaType,
			Url:  "data:" + img.MediaType + ";base64," + img.Data,
		})
	}
	for _, f := range msg.Files {
		prompt.Parts = append(prompt.Parts, opencodeapi.PromptPart{
			Type:     "file",
			Mime:     mimeForFile(f.Name),
			Url:      "file://" + f.Path,
			Filename: f.Name,
		})
	}

	// The v1 prompt endpoint returns immediately; the turn runs asynchronously
	// and closes on the session.status idle event. Wait for it here so the
	// turn's lifecycle stays inside runOpenCodeTurn (status → idle → card
	// cleanup), instead of letting a fired-and-forgotten POST decide when the
	// session is free.
	promptErr := srv.PromptAsync(ctx, ocSID, prompt)
	if promptErr == nil {
		turn.waitTurn(ctx)
	} else {
		turn.drain()
	}

	m.st.Upsert(sess, func(s *store.Session) { s.UpdatedAt = time.Now() })

	switch {
	case ctx.Err() != nil || turn.isInterrupted():
		if abortErr := turn.abortFailure(); abortErr != "" {
			m.emit(sessionID, "status", map[string]string{"state": "error", "error": abortErr})
			m.setIdle(sessionID, abortErr)
		} else {
			m.emit(sessionID, "status", map[string]string{"state": "interrupted"})
			m.setIdle(sessionID, "interrupted")
		}
	case turn.sessionErr != "":
		errMsg := m.diagnoseOpenCodeRunError(sess, cli, turn.sessionErr)
		log.Printf("opencode turn failed (session %s, opencode sid %s): %s", sessionID, ocSID, errMsg)
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": errMsg})
		m.setIdle(sessionID, errMsg)
	case promptErr != nil:
		errMsg := m.diagnoseOpenCodeRunError(sess, cli, promptErr.Error())
		log.Printf("opencode prompt failed (session %s, opencode sid %s): %s", sessionID, ocSID, errMsg)
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": errMsg})
		m.setIdle(sessionID, errMsg)
	default:
		m.emit(sessionID, "status", map[string]string{"state": "turn_end"})
		m.setIdle(sessionID, "")
	}
	// Every pending card for this session dies with the turn: opencode's
	// question/permission Deferreds would otherwise stay blocked forever and
	// the frontend would show a card the agent can no longer act on.
	turn.failPendingCards()
	// Stamp the opencode message id onto this turn's user event so the
	// rewind button has a target (the opencode counterpart of
	// captureCheckpoint).
	m.captureOpenCodeCheckpoint(sessionID, turn)
	cancel()
}

// buildMcodeArgs assembles the `mcode exec` argv. Split out of runMcodeTurn so
// the ordering contract is testable, because getting it wrong fails silently:
//   - the positional prompt must come **before** `--file <path>` (an array flag
//     would consume a trailing positional and eat the message)
//   - the HTML render hint must come **after** cli.Args, or a user's own
//     --append-system-prompt in config.yaml wouldn't be seen by the dedup check
func (m *Manager) buildMcodeArgs(msg Message, sess *store.Session, cli *config.CLI, prov *config.Provider) []string {
	args := []string{"exec", "--output-format", "stream-json"}
	// mcode takes --permission smart|full|off; "ask" needs an interactive
	// approver, which a headless run has none of. cc-webui's own default is
	// bypassPermissions, which lands on "full".
	if p := mcodePermissionPolicy(sess.PermissionMode, cli.PermissionMode); p != "" {
		args = append(args, "--permission", p)
	}
	model := prov.EffectiveModel(sess.ModelOverride)
	if model != "" {
		args = append(args, "--model", mcodeModelID(prov.Name, model))
	}
	// Reasoning depth rides mcode's --effort. Unlike claude there is no env
	// var that overrides it and no global settings file to fight: the value
	// is validated per model by the CLI itself, and the UI only offers the
	// levels the model declares (mcodehist.Catalog).
	if sess.EffortOverride != "" {
		args = append(args, "--effort", sess.EffortOverride)
	}
	if sess.ClaudeSessionID != "" {
		args = append(args, "--session", sess.ClaudeSessionID)
	}
	args = append(args, cli.Args...)
	args = m.appendHTMLRenderHint(args, cli)
	// Positional prompt before the attachment flags: `--file <path> <msg>`
	// would let the array flag consume the message.
	if msg.Text != "" {
		args = append(args, msg.Text)
	}
	for _, f := range msg.Files {
		args = append(args, "--file", f.Path)
	}
	return args
}

// runMcodeTurn runs one user message through the MiniMax Code engine
// (`mcode exec --output-format stream-json`). Every stdout line is one JSON
// event adapted to the canonical cc-webui stream shape and broadcast live
// (mcodehist — the same mapping the replay path uses, so a reloaded page
// looks like the live run did). The mcode session id rides on every line
// (mvs_… ), so it is captured on first sight and stored in the same
// ClaudeSessionID field opencode reuses; the transcript itself stays in
// mcode's own store and is replayed from messages.jsonl on later connects.
//
// No stdin channel: the message rides as a positional argument, like
// opencode. Inline images are rejected at the API layer — images and files
// both travel as mcode's official `--file` attachments. The positional must
// precede those flags: --file is an array flag and would swallow a trailing
// positional.
func (m *Manager) runMcodeTurn(sessionID string, msg Message, sess *store.Session, cli *config.CLI, prov *config.Provider, resolvedCwd string) {
	args := m.buildMcodeArgs(msg, sess, cli, prov)

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
	envMap := buildEnvMap(cli.EffectiveEnv(prov, prov.EffectiveModel(sess.ModelOverride)))
	cmd.Env = envMapToSlice(envMap)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": err.Error()})
		return
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": "start mcode: " + err.Error()})
		return
	}

	tx := mcodehist.NewToolTx()
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		ev, ok := mcodehist.ParseLive(sc.Bytes())
		if !ok {
			continue
		}
		// Capture the mvs_ id from the first line that carries it (all do).
		if sid := ev.SessionIDOf(); sid != "" && sid != sess.ClaudeSessionID {
			m.st.Upsert(sess, store.WithClaudeSID(sid))
			sess.ClaudeSessionID = sid
		}
		for _, payload := range ev.Adapt(tx) {
			m.emit(sessionID, "stream", payload)
		}
		// turn.completed carries the per-turn token counts and model; mcode
		// reports them outside any assistant message, so they ride a
		// dedicated assistant event for the stats path to pick up.
		if ev.Type == "turn.completed" {
			if p := mcodehist.UsagePayload(ev.Usage, ev.Model, ev.TS); p != nil {
				m.emit(sessionID, "stream", p)
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
		log.Printf("mcode turn failed (session %s, mcode sid %s): %s", sessionID, sess.ClaudeSessionID, errMsg)
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": errMsg})
		m.setIdle(sessionID, errMsg)
	default:
		m.emit(sessionID, "status", map[string]string{"state": "turn_end"})
		m.setIdle(sessionID, "")
	}
	// No captureCheckpoint: mcode exposes no file-rewind surface, the same
	// reason the opencode engine skips it.
}

// mcodePermissionPolicy maps cc-webui's claude-style permission mode onto
// mcode's --permission policy. Only "bypassPermissions" means "run
// unattended without prompting" (mcode: full); every other mode keeps mcode's
// own smart policy, since its interactive "ask" needs a TUI or ACP client.
func mcodePermissionPolicy(sessionMode, cliMode string) string {
	mode := sessionMode
	if mode == "" {
		mode = cliMode
	}
	if mode == "" {
		return ""
	}
	if mode == "bypassPermissions" {
		return "full"
	}
	return "smart"
}

// mcodeModelID composes the --model argument: mcode addresses models as
// <providerId>/<modelId>, where the provider part must match the id in its own
// runtime config. A name that already carries a slash passes through.
func mcodeModelID(providerName, modelName string) string {
	if strings.Contains(modelName, "/") {
		return modelName
	}
	if providerName == "" {
		return modelName
	}
	return providerName + "/" + modelName
}

// buildCodexArgs assembles the `codex exec` argv (and `exec resume` when the
// session already has an id). The prompt is never a positional argument: it is
// written to stdin, which avoids argv length limits for long prompts that
// include file mentions.
//
// `exec resume` does not accept --sandbox: a resumed thread inherits the
// policy recorded when it was created. bypassPermissions is the only mode that
// drops the sandbox entirely.
func (m *Manager) buildCodexArgs(msg Message, sess *store.Session, cli *config.CLI, prov *config.Provider) []string {
	args := []string{"exec"}
	resume := sess.ClaudeSessionID != ""
	if resume {
		args = append(args, "resume")
	}
	args = append(args, "--json", "--skip-git-repo-check")
	mode := sess.PermissionMode
	if mode == "" {
		mode = cli.PermissionMode
	}
	switch mode {
	case "bypassPermissions":
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	case "plan", "readOnly":
		// resume inherits the thread's policy, so only new sessions take it.
		if !resume {
			args = append(args, "--sandbox", "read-only")
		}
	default:
		// Headless exec cannot surface approval prompts, so the useful
		// default is "edit inside the workspace, never ask".
		if !resume {
			args = append(args, "--sandbox", "workspace-write")
		}
	}
	model := prov.EffectiveModel(sess.ModelOverride)
	if model != "" {
		args = append(args, "-m", model)
	}
	// Any -c override makes ducx skip the provider it would otherwise inject,
	// so a configured id is repeated here before the overrides that need it.
	// The HTML hint below is one of them, which is why this is not gated on
	// EffortOverride alone.
	wantHint := m.htmlHintEnabled() && !hasConfigOverride(cli.Args, "instructions")
	if (sess.EffortOverride != "" || wantHint) && cli.ModelProvider != "" {
		args = append(args, "-c", "model_provider="+cli.ModelProvider)
	}
	if sess.EffortOverride != "" {
		args = append(args, "-c", "model_reasoning_effort="+sess.EffortOverride)
	}
	if wantHint {
		args = append(args, "-c", codexInstructionsArg())
	}
	for _, f := range msg.Files {
		if isImageFile(f.Path) {
			args = append(args, "-i", f.Path)
		}
	}
	args = append(args, cli.Args...)
	if resume {
		args = append(args, sess.ClaudeSessionID)
	}
	return args
}

// runCodexTurn runs one user message through the Codex engine
// (`codex exec --json`). Every stdout line is one JSON event adapted to the
// canonical cc-webui stream shape (codexhist — the same mapping the replay
// path uses, so a reloaded page looks like the live run). The thread id arrives
// in thread.started and is stored in the same ClaudeSessionID field opencode,
// mcode and qoder reuse; the transcript itself stays in codex's rollout tree
// under $CODEX_HOME and is replayed from there on later connects.
//
// Inline base64 images are rejected at the API layer for this engine: images
// arrive as uploaded files and image-typed ones are additionally passed with
// codex's -i flag.
func (m *Manager) runCodexTurn(sessionID string, msg Message, sess *store.Session, cli *config.CLI, prov *config.Provider, resolvedCwd string) {
	args := m.buildCodexArgs(msg, sess, cli, prov)
	model := prov.EffectiveModel(sess.ModelOverride)

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
	envMap := buildEnvMap(cli.EffectiveEnv(prov, model))
	cmd.Env = envMapToSlice(envMap)
	cmd.Stdin = strings.NewReader(turnText(msg))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": err.Error()})
		return
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": "start codex: " + err.Error()})
		return
	}

	tx := codexhist.NewToolTx()
	fatal := ""
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		ev, ok := codexhist.ParseLive(sc.Bytes())
		if !ok {
			continue
		}
		if sid := ev.SessionIDOf(); sid != "" && sid != sess.ClaudeSessionID {
			m.st.Upsert(sess, store.WithClaudeSID(sid))
			sess.ClaudeSessionID = sid
		}
		if emsg := ev.ErrorMessage(); emsg != "" {
			fatal = emsg
		}
		for _, payload := range ev.Adapt(tx) {
			m.emit(sessionID, "stream", payload)
		}
		if ev.Type == "turn.completed" {
			if p := codexhist.UsagePayload(ev.Usage, model, ev.TS); p != nil {
				m.emit(sessionID, "stream", p)
			}
		}
	}

	waitErr := cmd.Wait()
	m.st.Upsert(sess, func(s *store.Session) { s.UpdatedAt = time.Now() })

	switch {
	case ctx.Err() != nil:
		m.emit(sessionID, "status", map[string]string{"state": "interrupted"})
		m.setIdle(sessionID, "interrupted")
	case waitErr != nil || fatal != "":
		errMsg := strings.TrimSpace(fatal)
		if errMsg == "" {
			errMsg = strings.TrimSpace(stderr.String())
		}
		if errMsg == "" && waitErr != nil {
			errMsg = waitErr.Error()
		}
		log.Printf("codex turn failed (session %s, codex sid %s): %s", sessionID, sess.ClaudeSessionID, errMsg)
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": errMsg})
		m.setIdle(sessionID, errMsg)
	default:
		m.emit(sessionID, "status", map[string]string{"state": "turn_end"})
		m.setIdle(sessionID, "")
	}
	// No captureCheckpoint: codex exposes no claude-style file rewind.
}

// isImageFile reports whether path looks like an image codex can attach with
// -i. The composer uploads inline images as files for this engine, so the
// extension is the only type signal available (UploadedFile has no MIME).
func isImageFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp":
		return true
	}
	return false
}

// diagnoseOpenCodeRunError enriches a generic opencode run failure with an
// actionable cause when one can be determined cheaply. Best-effort: any
// diagnostic failure returns errMsg unchanged, and the happy path never pays
// for it (only called after a turn already failed).
func (m *Manager) diagnoseOpenCodeRunError(sess *store.Session, cli *config.CLI, errMsg string) string {
	if !strings.Contains(errMsg, "Unexpected server error") || sess.ClaudeSessionID == "" {
		return errMsg
	}
	oc, err := opencodehist.FetchSession(cli.Path, sess.ClaudeSessionID)
	if err != nil || oc == nil || oc.Directory == "" {
		return errMsg
	}
	_, statErr := os.Stat(oc.Directory)
	return enrichOpenCodeError(errMsg, oc.Directory, statErr != nil)
}

// enrichOpenCodeError is the pure message half of diagnoseOpenCodeRunError:
// when the run died with opencode's generic server error and the session's
// own directory is confirmed missing, append a Chinese diagnosis telling the
// user the session cannot continue and what to do instead. All other inputs
// pass through unchanged.
func enrichOpenCodeError(errMsg, sessionDir string, dirMissing bool) string {
	if !strings.Contains(errMsg, "Unexpected server error") || sessionDir == "" || !dirMissing {
		return errMsg
	}
	return fmt.Sprintf("%s\n（诊断：该 opencode 会话创建时的目录 %s 已不存在（被删除、改名或创建时填错），此会话无法继续运行，只能查看历史。请用正确的目录新建会话继续。）", errMsg, sessionDir)
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
		// No model call in this path (--rewind-files restores and exits), so
		// the session effort is irrelevant — use the plain settings file.
		if p, err := m.settingsFile(cli.Name, prov.Name, model, "", env); err == nil {
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
