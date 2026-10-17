package runner

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"cc-webui/internal/config"
	"cc-webui/internal/opencodeapi"
)

// opencodeServePool owns the `opencode serve` child processes, one per
// (cli, provider, model) triple whose env differs. The previous engine spawned
// a fresh `opencode run` per turn and passed the provider env through that
// child's environment; a long-lived server gets its credentials the same way,
// at Start time.
//
// Sessions sharing an identical env signature share one server. opencode keys
// its per-project state off the `directory` query parameter, not the process
// cwd, so one server can host sessions across many directories.
type opencodeServePool struct {
	m *Manager
	// pidFile records every serve child this process started, so the NEXT
	// cc-webui start can reap whatever a SIGKILL left behind.
	pidFile string

	mu      sync.Mutex
	servers map[string]*serveEntry
}

type serveEntry struct {
	srv  *opencodeapi.Server
	hub  *opencodeapi.Hub
	stop context.CancelFunc
	refs int
	// lastUse drives idle shutdown: a serve process holds provider
	// credentials in memory, so it is reaped after a quiet period.
	lastUse time.Time
}

func newOpencodeServePool(m *Manager) *opencodeServePool {
	p := &opencodeServePool{m: m, servers: map[string]*serveEntry{}}
	// m.cfg() panics on a nil config manager (unit tests construct a bare
	// Manager), so guard before asking for the data dir.
	if m.cm != nil {
		cfg := m.cfg()
		if cfg != nil && cfg.DataDir != "" {
			p.pidFile = filepath.Join(cfg.DataDir, "opencode-serve.pids")
		}
	}
	// A previous run may have died without running its defers; reclaim first.
	if p.pidFile != "" {
		reapOrphanServes(p.pidFile)
	}
	return p
}

// reapOrphanServes kills serve processes recorded by a previous cc-webui run.
//
// Graceful shutdown (SIGINT/SIGTERM) removes them, but SIGKILL, a panic, or
// pulling the plug skips every defer — and the serve children survive, reparented
// to launchd, each holding provider credentials in memory. Since only one
// cc-webui owns the data dir at a time, any pid still listed in the file at
// startup belongs to a dead instance and is safe to kill.
//
// Each pid is validated as an actual `opencode serve` before signalling, so a
// recycled pid can never be hit by mistake.
func reapOrphanServes(pidFile string) {
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return // never ran, or already reaped
	}
	var killed int
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil || pid <= 0 {
			continue
		}
		if !isOpenCodeServe(pid) {
			continue
		}
		if syscall.Kill(pid, syscall.SIGKILL) == nil {
			killed++
		}
	}
	if killed > 0 {
		log.Printf("已清理上一次运行遗留的 %d 个 opencode serve 孤儿进程", killed)
	}
	_ = os.Remove(pidFile)
}

// isOpenCodeServe reports whether pid is an `opencode serve` process. Guards
// against pid reuse: never signal a pid we cannot positively identify.
func isOpenCodeServe(pid int) bool {
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		// macOS has no /proc; fall back to ps.
		out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
		if err != nil {
			return false
		}
		return isOpenCodeServeCmd(string(out))
	}
	return strings.Contains(exe, "opencode")
}

// isOpenCodeServeCmd matches the serve argv (not `opencode run`/TUI, which a
// user may legitimately have open elsewhere).
func isOpenCodeServeCmd(cmdline string) bool {
	if !strings.Contains(cmdline, "opencode") {
		return false
	}
	return strings.Contains(cmdline, " serve ") || strings.HasSuffix(strings.TrimSpace(cmdline), "serve")
}

// recordServePID appends a started serve pid to the pid file.
func (p *opencodeServePool) recordServePID(pid int) {
	if p.pidFile == "" || pid <= 0 {
		return
	}
	f, err := os.OpenFile(p.pidFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%d\n", pid)
}

// serveIdleTimeout reaps a serve process that no session has used for this
// long. Pending permissions live only in that process's memory (opencode
// keeps them in InstanceState and rejects them all on dispose), so reaping a
// busy server would kill in-flight cards — hence "idle" only.
const serveIdleTimeout = 30 * time.Minute

// acquire returns a running serve for this env signature, starting one if
// needed. The returned release func must be called when the turn ends.
func (p *opencodeServePool) acquire(ctx context.Context, cli *config.CLI, prov *config.Provider, model, workDir string) (*opencodeapi.Server, *opencodeapi.Hub, func(), error) {
	key := serveKey(cli, prov, model)

	p.mu.Lock()
	// Sweep idle servers first so a long-lived process does not linger.
	now := time.Now()
	for k, e := range p.servers {
		if e.refs == 0 && now.Sub(e.lastUse) > serveIdleTimeout {
			e.stop()
			e.srv.Close()
			delete(p.servers, k)
			log.Printf("opencode serve idle-reaped (%s)", k)
		}
	}
	if e, ok := p.servers[key]; ok {
		e.refs++
		e.lastUse = now
		p.mu.Unlock()
		return e.srv, e.hub, p.release(key), nil
	}
	p.mu.Unlock()

	// opencode has no --append-system-prompt, so the HTML render hint reaches
	// it as a global rules file instead. Must land before the server starts:
	// a serve process is long-lived and shared, and we do not want to depend on
	// whether it re-reads rules per turn. Failures here are logged and never
	// fatal — losing the hint degrades output quality, but failing the whole
	// turn over a file in $HOME would be a much worse trade.
	p.m.ensureOpencodeRule()

	env := envMapToSlice(buildEnvMap(cli.EffectiveEnv(prov, model)))
	srv, err := opencodeapi.Start(cli.Path, env, workDir)
	if err != nil {
		return nil, nil, nil, err
	}
	hubCtx, cancel := context.WithCancel(context.Background())
	hub := opencodeapi.NewHub(srv)
	go hub.Run(hubCtx)

	p.mu.Lock()
	// Another turn raced us to the same key; keep the first and drop ours.
	if e, ok := p.servers[key]; ok {
		e.refs++
		e.lastUse = now
		p.mu.Unlock()
		cancel()
		srv.Close()
		return e.srv, e.hub, p.release(key), nil
	}
	p.servers[key] = &serveEntry{srv: srv, hub: hub, stop: cancel, refs: 1, lastUse: now}
	p.mu.Unlock()
	p.recordServePID(srv.PID())
	log.Printf("opencode serve started (%s)", serveLabel(key))
	return srv, hub, p.release(key), nil
}

func (p *opencodeServePool) release(key string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			if e, ok := p.servers[key]; ok {
				e.refs--
				e.lastUse = time.Now()
			}
			p.mu.Unlock()
		})
	}
}

// Close stops every serve process (called on server shutdown).
func (p *opencodeServePool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, e := range p.servers {
		e.stop()
		e.srv.Close()
		delete(p.servers, k)
	}
}

// serveLabel is a log-safe rendering of a serve key.
//
// The key itself embeds every credential-shaped env var (it has to, so two
// providers never share a process) — logging it verbatim would dump tokens
// and base URLs into the cc-webui log. This reduces it to the identity part
// (binary, cli, provider, model) and a count of the env vars that made the
// split.
func serveLabel(key string) string {
	parts := strings.Split(key, "\x00")
	identity := parts[:4]
	label := strings.Join(identity, "/")
	envPart := ""
	if len(parts) > 4 {
		envPart = parts[4]
	}
	n := 0
	if envPart != "" {
		n = len(strings.Split(envPart, "\x00"))
	}
	return fmt.Sprintf("%s (env:%d)", label, n)
}

// credentialEnv decides whether an env var belongs in the serve identity.
//
// This must be a WHITELIST, not a blacklist. The input is buildEnvMap's output,
// which starts from the whole of os.Environ() — on a normal desktop login that
// is 50+ variables, and a blacklist of the obvious few (PATH/HOME/TERM/…) let
// through everything else: SHLVL, _, OLDPWD, STY, SECURITYSESSIONID,
// ITERM_SESSION_ID, TERM_SESSION_ID, WINDOW… Those change between shell
// launches, so the key changed too, and every cc-webui restart (or any new
// terminal) spawned yet another `opencode serve` instead of reusing the pooled
// one — leaving orphans behind holding provider credentials in memory.
//
// Only vars that can actually change which credentials the server ends up
// using belong here: API keys, base URLs, explicit model pinning, and the
// profile/redirect knobs opencode itself reads.
func credentialEnv(k string) bool {
	// Exact names.
	switch k {
	case "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_REASONING_MODEL",
		"ANTHROPIC_CUSTOM_HEADERS",
		"OPENAI_BASE_URL", "OPENAI_API_KEY", "OPENAI_ORGANIZATION",
		"OPENROUTER_API_KEY", "OPENROUTER_BASE_URL",
		"OPENCODE_SERVER_PASSWORD", "OPENCODE_CONFIG", "OPENCODE_PROFILE",
		"GEMINI_API_KEY", "GROQ_API_KEY", "MISTRAL_API_KEY", "COHERE_API_KEY",
		"AWS_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
		"AZURE_OPENAI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		"HTTP_PROXY_URL", "HTTPS_PROXY_URL":
		return true
	}
	// Suffix families: anything that is a token/key/secret, or a base url.
	switch {
	case strings.HasSuffix(k, "_API_KEY"),
		strings.HasSuffix(k, "_AUTH_TOKEN"),
		strings.HasSuffix(k, "_ACCESS_TOKEN"),
		strings.HasSuffix(k, "_BASE_URL"),
		strings.HasSuffix(k, "_API_BASE"),
		strings.HasSuffix(k, "_API_HOST"),
		strings.HasSuffix(k, "_SECRET"),
		strings.HasSuffix(k, "_SECRET_KEY"):
		return true
	}
	return false
}

// serveKey identifies a serve process by everything that changes its
// credentials or binary: the CLI path, cli name/args, the provider name, and
// the model (whose env can carry a per-model base URL / key suffix).
//
// The env segment is deliberately limited to credentialEnv — see its doc for
// why a blacklist here was a real bug.
func serveKey(cli *config.CLI, prov *config.Provider, model string) string {
	env := buildEnvMap(cli.EffectiveEnv(prov, model))
	var parts []string
	for k, v := range env {
		if credentialEnv(k) {
			parts = append(parts, k+"="+v)
		}
	}
	sort.Strings(parts)
	return cli.Path + "\x00" + cli.Name + "\x00" + strings.Join(cli.Args, "\x00") +
		"\x00" + prov.Name + "\x00" + model + "\x00" + strings.Join(parts, "\x00")
}
