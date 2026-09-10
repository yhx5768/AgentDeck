package runner

// Resident mode: a long-lived streaming-input claude process per session, so
// the session stays registered in ~/.claude/sessions and reachable from other
// Claude sessions via the built-in SendMessage tool (peer messaging).
// Design doc: peer-rd.md. Wire protocol mirrors the Agent SDK
// (claude_agent_sdk/_internal/query.py):
//
//	host → CLI  {"type":"user","message":{"role":"user","content":[...]}}
//	host → CLI  {"type":"control_request","request_id":...,"request":{"subtype":...}}
//	CLI  → host {"type":"control_response","response":{"subtype":"success"|"error","request_id":..., ...}}
//
// Turn boundaries come from the output stream: a user event with
// origin.kind=="peer" starts an injected turn while idle, and "result" ends
// whichever turn is in flight (the CLI may also inject peer messages mid-turn;
// those just render, the single result still belongs to our turn).

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"cc-webui/internal/claudehist"
	"cc-webui/internal/store"
)

// residentProc is one live resident process and its protocol state.
type residentProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	pid   int

	dead    chan struct{} // closed by the read loop when the process exits
	deadErr string

	mu             sync.Mutex
	reqN           int
	pendingCtrl    map[string]chan map[string]any // request_id -> response chan
	registeredName string                         // from ~/.claude/sessions/<pid>.json
	nameSource     string                         // registry nameSource ("derived" = auto)
	lastRegScan    time.Time                      // last on-demand registry refresh
	sessionID      string                         // claude session id from the init event
	initSeen       chan struct{}                  // closed on first system/init event
	busy           bool                           // a turn (ours or peer-injected) is in flight
	turnKind       string                         // "ours" | "peer"
	interrupted    bool                           // interrupt requested for the current turn
	turnDone       chan string                    // ours-turn result: "ok" | "interrupted"
	idleCh         chan struct{}                  // closed on every busy->idle transition
	stopping       bool                           // teardown in progress: death is expected
}

func (p *residentProc) regName() (string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.registeredName, p.nameSource
}

func (p *residentProc) setRegName(name, source string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.registeredName = name
	p.nameSource = source
}

// registryRefreshDue reports whether the registered name is still unknown and
// enough time has passed since the last scan to retry. GetStatus runs on every
// status poll, so re-scanning the registry dir on each call would be wasteful.
func (p *residentProc) registryRefreshDue() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.registeredName != "" {
		return false
	}
	now := time.Now()
	if p.lastRegScan.IsZero() || now.Sub(p.lastRegScan) >= 2*time.Second {
		p.lastRegScan = now
		return true
	}
	return false
}

func (p *residentProc) isBusy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.busy
}

// writeLine sends one JSON line on stdin. Serialized against concurrent
// control requests.
func (p *residentProc) writeLine(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin == nil {
		return fmt.Errorf("resident process is gone")
	}
	_, err = p.stdin.Write(append(raw, '\n'))
	return err
}

// controlRequest sends a control request and waits for the matching
// control_response. Used for the initialize handshake and interrupt.
func (p *residentProc) controlRequest(subtype string, extra map[string]any, timeout time.Duration) (map[string]any, error) {
	p.mu.Lock()
	p.reqN++
	rnd := make([]byte, 4)
	_, _ = rand.Read(rnd)
	rid := fmt.Sprintf("req_%d_%s", p.reqN, hex.EncodeToString(rnd))
	ch := make(chan map[string]any, 1)
	p.pendingCtrl[rid] = ch
	p.mu.Unlock()

	req := map[string]any{"subtype": subtype}
	for k, v := range extra {
		req[k] = v
	}
	if err := p.writeLine(map[string]any{
		"type":       "control_request",
		"request_id": rid,
		"request":    req,
	}); err != nil {
		p.mu.Lock()
		delete(p.pendingCtrl, rid)
		p.mu.Unlock()
		return nil, err
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("resident process exited")
		}
		if e, _ := resp["error"].(string); e != "" {
			return resp, fmt.Errorf("%s", e)
		}
		return resp, nil
	case <-p.dead:
		return nil, fmt.Errorf("resident process exited")
	case <-time.After(timeout):
		p.mu.Lock()
		delete(p.pendingCtrl, rid)
		p.mu.Unlock()
		return nil, fmt.Errorf("control request %s timed out", subtype)
	}
}

// interrupt asks the CLI to abort the current turn without killing the
// process. Fire-and-forget: the turn's result frame follows shortly.
func (p *residentProc) interrupt() {
	p.mu.Lock()
	p.interrupted = true
	p.mu.Unlock()
	go func() {
		if _, err := p.controlRequest("interrupt", nil, 30*time.Second); err != nil {
			log.Printf("resident interrupt: %v", err)
		}
	}()
}

// ---- Manager API ----

// EnsureResident spawns the resident process for a resident-enabled session
// if it isn't alive. When the claude session id is already known, the peer
// name is written to the transcript BEFORE spawn so the process registers
// under it immediately.
func (m *Manager) EnsureResident(sessionID string) error {
	sess := m.st.Get(sessionID)
	if sess == nil {
		return fmt.Errorf("session not found")
	}
	if !sess.Resident {
		return fmt.Errorf("session is not resident-enabled")
	}
	// Resident/peer messaging is a Claude Code protocol feature (stream-json
	// input + registry); opencode has no equivalent.
	if cli := m.cfg().CLIByName(sess.CLI); cli != nil && cli.IsOpenCode() {
		return fmt.Errorf("opencode 引擎不支持常驻/peer 消息")
	}
	_, err := m.ensureProc(sessionID, sess)
	return err
}

// TeardownResident stops the resident process (graceful stdin EOF, then
// SIGKILL the process group after a grace period). Safe to call when no
// resident process exists.
func (m *Manager) TeardownResident(sessionID string) {
	r := m.rt(sessionID)
	r.mu.Lock()
	proc := r.resident
	r.resident = nil
	r.mu.Unlock()
	if proc == nil {
		return
	}
	proc.mu.Lock()
	proc.stopping = true
	proc.mu.Unlock()
	_ = proc.stdin.Close()
	select {
	case <-proc.dead:
	case <-time.After(5 * time.Second):
		if proc.cmd.Process != nil {
			_ = syscall.Kill(-proc.cmd.Process.Pid, syscall.SIGKILL)
		}
		<-proc.dead
	}
	r.mu.Lock()
	r.notifyStatus()
	r.mu.Unlock()
}

// RestoreResidents spawns resident processes for all resident-enabled
// sessions, staggered to avoid a boot storm. Called once at server start; the
// previous cc-webui process's children already exited on stdin EOF, so there
// is nothing to adopt — only to respawn.
func (m *Manager) RestoreResidents() {
	for _, sess := range m.st.List() {
		if !sess.Resident || sess.Cwd == "" {
			continue
		}
		sid := sess.ID
		go func() {
			if err := m.EnsureResident(sid); err != nil {
				log.Printf("restore resident %s: %v", sid, err)
			}
		}()
		time.Sleep(300 * time.Millisecond)
	}
}

// ensureProc returns the live resident process, spawning one if needed.
func (m *Manager) ensureProc(sessionID string, sess *store.Session) (*residentProc, error) {
	r := m.rt(sessionID)
	r.mu.Lock()
	proc := r.resident
	r.mu.Unlock()
	if proc != nil {
		select {
		case <-proc.dead:
		default:
			return proc, nil
		}
	}
	return m.spawnResident(sessionID, sess)
}

// spawnResident starts the streaming-input process and its read loop.
func (m *Manager) spawnResident(sessionID string, sess *store.Session) (*residentProc, error) {
	cli := m.cfg().CLIByName(sess.CLI)
	prov := m.cfg().ProviderByName(sess.Provider)
	if cli == nil {
		return nil, fmt.Errorf("cli not found: %s", sess.CLI)
	}
	if prov == nil {
		return nil, fmt.Errorf("provider not found: %s", sess.Provider)
	}

	// Write the peer name before spawn so the process registers under it
	// (registry name is fixed at process start).
	if sess.ClaudeSessionID != "" {
		if name := effectivePeerName(sess); name != "" {
			if err := claudehist.RenameSession(sess.Cwd, sess.ClaudeSessionID, name); err != nil {
				log.Printf("rename session %s: %v", sess.ClaudeSessionID, err)
			}
		}
	}

	args := []string{"-p", "--output-format", "stream-json", "--verbose",
		"--input-format", "stream-json"}
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
	resolvedCwd, err := ResolveCwd(sess.Cwd)
	if err != nil {
		cancel()
		return nil, err
	}
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
	// 常驻 session：CLI 在 interactive kind 下忽略 transcript 的 custom-title
	//（见 bundled CLI 源码 ~"interactive" 分支强制走 derived 名）。要自定义
	// 地址名只能通过 CLAUDE_CODE_SESSION_NAME env 注入，CLI 启动时直接以它
	// 写注册表的 name 字段，sender 用 SendMessage(to:n) 即可寻址。
	if name := effectivePeerName(sess); name != "" {
		envMap["CLAUDE_CODE_SESSION_NAME"] = name
	}
	cmd.Env = envMapToSlice(envMap)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start claude: %w", err)
	}

	proc := &residentProc{
		cmd:         cmd,
		stdin:       stdin,
		pid:         cmd.Process.Pid,
		dead:        make(chan struct{}),
		pendingCtrl: map[string]chan map[string]any{},
		initSeen:    make(chan struct{}),
		turnDone:    make(chan string, 1),
		idleCh:      make(chan struct{}),
	}

	r := m.rt(sessionID)
	r.mu.Lock()
	if r.resident != nil {
		// Lost a spawn race; tear the loser down (reap it so it doesn't
		// leak as a zombie).
		r.mu.Unlock()
		proc.mu.Lock()
		proc.stopping = true
		proc.mu.Unlock()
		_ = stdin.Close()
		cancel()
		go func() { _ = proc.cmd.Wait() }()
		r.mu.Lock()
		proc = r.resident
		r.mu.Unlock()
		return proc, nil
	}
	r.resident = proc
	r.notifyStatus()
	r.mu.Unlock()

	go m.procReadLoop(sessionID, proc, stdout, &stderrBuf, cancel)
	go m.procBootstrap(sessionID, proc)
	return proc, nil
}

// procBootstrap waits for the CLI to register itself in ~/.claude/sessions
// and captures the name into proc state. With CLAUDE_CODE_SESSION_NAME
// injected at spawn, the name matches the desired peer name and no restart is
// needed (the old transcript-custom-title path used to fight derived names by
// restarting; CLI 2.1.226 ignores custom-title in interactive kind, so env var
// is now the only mechanism).
func (m *Manager) procBootstrap(sessionID string, proc *residentProc) {
	// 等 initSeen 给个机会，但超时也不能直接 return —— registry 文件是进程
	// 启动就写的（与 init 事件无关），必须 poll，否则 CLI 不发 init 时
	// registered_name 永远空着，前端卡在"注册中"。
	select {
	case <-proc.initSeen:
	case <-proc.dead:
		return
	case <-time.After(15 * time.Second):
		// fall through: still poll registry below
	}
	// Poll for the registry entry (written at process start, may lag init by
	// a slow model/auth warmup). Look up by pid first, then by session id:
	// launcher CLIs (ducc) fork the real claude as a child, so the file is
	// named after the child's pid, not ours.
	sess := m.st.Get(sessionID)
	found := false
	for i := 0; i < 120; i++ { // up to 60s
		if e := m.lookupRegistry(proc, sess); e != nil {
			proc.setRegName(e.Name, e.NameSource)
			log.Printf("resident %s pid=%d: registry name=%q source=%q", sessionID, proc.pid, e.Name, e.NameSource)
			found = true
			break
		}
		select {
		case <-proc.dead:
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
	if !found {
		log.Printf("resident %s pid=%d: no registry entry after 60s polling — CLI may not write ~/.claude/sessions/*.json (peerProtocol off, wrong $HOME, or old CLI)", sessionID, proc.pid)
	}
}

// lookupRegistry finds the CLI's registry entry: first by the pid we spawned
// (real binaries write <pid>.json), then by claude session id (launcher CLIs
// like ducc fork the real claude as a child, so the file carries the child's
// pid but the same sessionId).
func (m *Manager) lookupRegistry(proc *residentProc, sess *store.Session) *claudehist.RegistryEntry {
	if e := claudehist.ReadRegistryEntry(proc.pid); e != nil {
		return e
	}
	proc.mu.Lock()
	sid := proc.sessionID
	proc.mu.Unlock()
	if sid == "" && sess != nil {
		sid = sess.ClaudeSessionID
	}
	if sid == "" {
		return nil
	}
	return claudehist.ReadRegistryForSession(sid)
}

// restartResident tears down proc and spawns a replacement.
func (m *Manager) restartResident(sessionID string, proc *residentProc) {
	r := m.rt(sessionID)
	r.mu.Lock()
	if r.resident != proc {
		r.mu.Unlock()
		return
	}
	r.resident = nil
	r.mu.Unlock()
	proc.mu.Lock()
	proc.stopping = true
	proc.mu.Unlock()
	_ = proc.stdin.Close()
	select {
	case <-proc.dead:
	case <-time.After(5 * time.Second):
		if proc.cmd.Process != nil {
			_ = syscall.Kill(-proc.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	sess := m.st.Get(sessionID)
	if sess == nil || !sess.Resident {
		return
	}
	if _, err := m.spawnResident(sessionID, sess); err != nil {
		log.Printf("respawn resident %s: %v", sessionID, err)
	}
}

// procReadLoop consumes stdout for the life of the process: forwards stream
// events, answers control requests, tracks turn boundaries, and handles
// process death.
func (m *Manager) procReadLoop(sessionID string, proc *residentProc, stdout io.Reader, stderrBuf *strings.Builder, cancel context.CancelFunc) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(line, &obj); err != nil {
			continue
		}
		m.procEvent(sessionID, proc, obj)
	}
	waitErr := proc.cmd.Wait()
	cancel()
	proc.mu.Lock()
	switch {
	case waitErr != nil:
		proc.deadErr = strings.TrimSpace(stderrBuf.String())
		if proc.deadErr == "" {
			proc.deadErr = waitErr.Error()
		}
	default:
		proc.deadErr = "resident process exited"
	}
	deadErr := proc.deadErr
	stopping := proc.stopping
	// Fail any pending control requests and an in-flight ours-turn waiter.
	for rid, ch := range proc.pendingCtrl {
		delete(proc.pendingCtrl, rid)
		close(ch)
	}
	if proc.busy && proc.turnKind == "ours" {
		select {
		case proc.turnDone <- "dead":
		default:
		}
	}
	proc.mu.Unlock()
	close(proc.dead)

	r := m.rt(sessionID)
	r.mu.Lock()
	if r.resident == proc {
		r.resident = nil
	}
	r.mu.Unlock()
	if !stopping {
		log.Printf("resident %s (pid %d) died: %s", sessionID, proc.pid, deadErr)
		m.emit(sessionID, "status", map[string]string{"state": "resident_down", "error": deadErr})
	}
}

// procEvent handles one parsed stdout object.
func (m *Manager) procEvent(sessionID string, proc *residentProc, obj map[string]any) {
	typ, _ := obj["type"].(string)

	switch typ {
	case "control_response":
		// request_id lives inside the response object.
		if resp, ok := obj["response"].(map[string]any); ok {
			if rid, _ := resp["request_id"].(string); rid != "" {
				proc.mu.Lock()
				if ch, ok := proc.pendingCtrl[rid]; ok {
					delete(proc.pendingCtrl, rid)
					ch <- resp
				}
				proc.mu.Unlock()
			}
		}
		return
	case "control_request":
		// We register no hooks / SDK MCP servers / canUseTool callback, so any
		// inbound control request is unsupported — answer with an error frame
		// instead of leaving the CLI hanging.
		rid, _ := obj["request_id"].(string)
		if rid != "" {
			_ = proc.writeLine(map[string]any{
				"type": "control_response",
				"response": map[string]any{
					"subtype":    "error",
					"request_id": rid,
					"error":      "cc-webui resident host does not handle control requests",
				},
			})
		}
		return
	}

	// Everything else is a regular stream event — forward it.
	m.emit(sessionID, "stream", obj)

	switch typ {
	case "system":
		if sub, _ := obj["subtype"].(string); sub == "init" {
			m.procOnInit(sessionID, proc, obj)
		}
	case "user":
		if origin, ok := obj["origin"].(map[string]any); ok {
			if kind, _ := origin["kind"].(string); kind == "peer" {
				m.procOnPeerMessage(sessionID, proc)
			}
		}
	case "result":
		m.procOnResult(sessionID, proc)
	}
}

// procOnInit captures the claude session id and slash commands from the init
// event, mirroring runTurn's handling.
func (m *Manager) procOnInit(sessionID string, proc *residentProc, obj map[string]any) {
	sess := m.st.Get(sessionID)
	if sess == nil {
		return
	}
	if sid, _ := obj["session_id"].(string); sid != "" {
		proc.mu.Lock()
		first := proc.sessionID == ""
		proc.sessionID = sid
		proc.mu.Unlock()
		if sid != sess.ClaudeSessionID {
			m.st.Upsert(sess, store.WithClaudeSID(sid))
		}
		if first {
			close(proc.initSeen)
		}
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
		}
	}
}

// procOnPeerMessage starts an injected peer turn when the process is idle
// (mid-turn injections are rendered by the stream forward and need no state).
func (m *Manager) procOnPeerMessage(sessionID string, proc *residentProc) {
	proc.mu.Lock()
	if proc.busy {
		proc.mu.Unlock()
		return
	}
	proc.busy = true
	proc.turnKind = "peer"
	proc.interrupted = false
	proc.mu.Unlock()

	m.setRunning(sessionID, proc.interrupt)
	m.emit(sessionID, "status", map[string]string{"state": "turn_start"})
}

// procOnResult ends whichever turn is in flight.
func (m *Manager) procOnResult(sessionID string, proc *residentProc) {
	proc.mu.Lock()
	if !proc.busy {
		// A result with no tracked turn (shouldn't happen) — ignore.
		proc.mu.Unlock()
		return
	}
	kind := proc.turnKind
	interrupted := proc.interrupted
	proc.busy = false
	proc.turnKind = ""
	proc.interrupted = false
	close(proc.idleCh)
	proc.idleCh = make(chan struct{})
	proc.mu.Unlock()

	if kind == "ours" {
		state := "ok"
		if interrupted {
			state = "interrupted"
		}
		select {
		case proc.turnDone <- state:
		default:
		}
		return
	}
	// Peer-injected turn: no checkpoint capture (the user event isn't ours),
	// just status lifecycle.
	if interrupted {
		m.emit(sessionID, "status", map[string]string{"state": "interrupted"})
		m.setIdle(sessionID, "interrupted")
	} else {
		m.emit(sessionID, "status", map[string]string{"state": "turn_end"})
		m.setIdle(sessionID, "")
	}
}

// runResidentTurn runs one queued user message on the resident process:
// waits for any injected turn to drain, writes the message, and blocks until
// the result frame.
func (m *Manager) runResidentTurn(sessionID string, msg Message) {
	sess := m.st.Get(sessionID)
	if sess == nil {
		return
	}
	proc, err := m.ensureProc(sessionID, sess)
	if err != nil {
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": err.Error()})
		m.setIdle(sessionID, err.Error())
		return
	}

	// Wait for an in-flight injected (peer) turn to drain before claiming the
	// process — writing mid-turn would scramble result attribution.
	for proc.isBusy() {
		proc.mu.Lock()
		idle := proc.idleCh
		proc.mu.Unlock()
		select {
		case <-idle:
		case <-proc.dead:
			m.emit(sessionID, "status", map[string]string{"state": "error", "error": "resident process exited"})
			m.setIdle(sessionID, "resident process exited")
			return
		}
	}

	m.emit(sessionID, "status", map[string]string{"state": "turn_start"})
	proc.mu.Lock()
	proc.busy = true
	proc.turnKind = "ours"
	proc.interrupted = false
	proc.mu.Unlock()
	m.setRunning(sessionID, proc.interrupt)

	content := []map[string]any{}
	if text := turnText(msg); text != "" {
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
	if err := proc.writeLine(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": content},
	}); err != nil {
		proc.mu.Lock()
		proc.busy = false
		proc.turnKind = ""
		proc.mu.Unlock()
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": err.Error()})
		m.setIdle(sessionID, err.Error())
		return
	}

	select {
	case state := <-proc.turnDone:
		m.st.Upsert(sess, func(s *store.Session) { s.UpdatedAt = time.Now() })
		switch state {
		case "interrupted":
			m.emit(sessionID, "status", map[string]string{"state": "interrupted"})
			m.setIdle(sessionID, "interrupted")
		case "dead":
			proc.mu.Lock()
			deadErr := proc.deadErr
			proc.mu.Unlock()
			if deadErr == "" {
				deadErr = "resident process exited"
			}
			m.emit(sessionID, "status", map[string]string{"state": "error", "error": deadErr})
			m.setIdle(sessionID, deadErr)
		default:
			m.emit(sessionID, "status", map[string]string{"state": "turn_end"})
			m.setIdle(sessionID, "")
			// Fresh copy: ClaudeSessionID may have been learned at init.
			if cur := m.st.Get(sessionID); cur != nil {
				m.captureCheckpoint(sessionID, msg, cur)
			}
		}
	case <-proc.dead:
		proc.mu.Lock()
		deadErr := proc.deadErr
		proc.mu.Unlock()
		if deadErr == "" {
			deadErr = "resident process exited"
		}
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": deadErr})
		m.setIdle(sessionID, deadErr)
	}
}

// ApplyPeerName persists the session's effective peer name into the claude
// transcript (custom-title) and, when a resident process is alive under a
// different name, restarts it so the registry picks the name up. No-op when
// the session has no claude session id yet — the name is applied at the
// first resident spawn instead.
func (m *Manager) ApplyPeerName(sess *store.Session) error {
	if sess.ClaudeSessionID == "" {
		return nil
	}
	name := effectivePeerName(sess)
	if name == "" {
		return fmt.Errorf("no usable peer name")
	}
	if err := claudehist.RenameSession(sess.Cwd, sess.ClaudeSessionID, name); err != nil {
		return err
	}
	r := m.rt(sess.ID)
	r.mu.Lock()
	proc := r.resident
	r.mu.Unlock()
	if proc == nil {
		return nil
	}
	regName, src := proc.regName()
	if regName == "" {
		// Bootstrap may not have caught the registry yet (slow start, or a
		// launcher CLI whose file carries the child's pid) — look it up now
		// before deciding a restart is needed.
		if e := m.lookupRegistry(proc, sess); e != nil {
			proc.setRegName(e.Name, e.NameSource)
			regName, src = e.Name, e.NameSource
		}
	}
	if regName == name && src != "derived" {
		return nil // already registered under this exact custom name
	}
	m.restartResident(sess.ID, proc)
	return nil
}

// effectivePeerName resolves the session's address name: explicit PeerName
// wins, otherwise the display name is cleaned up. "" means "leave unnamed".
func effectivePeerName(sess *store.Session) string {
	if sess.PeerName != "" {
		return CleanPeerName(sess.PeerName)
	}
	return CleanPeerName(sess.Name)
}

// CleanPeerName normalizes an address name: the CLI rejects control
// characters and titles starting with "/" are unaddressable (a documented CLI
// quirk). Unicode (e.g. Chinese) names are valid — no ASCII folding here.
// "" means "not usable as a peer name".
func CleanPeerName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || (r < 0x20 && r != '\t') {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "/") {
		return ""
	}
	runes := []rune(s)
	if len(runes) > 80 {
		return string(runes[:80])
	}
	return s
}
