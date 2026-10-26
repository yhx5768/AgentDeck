// Package opencodeapi drives an opencode engine through its headless HTTP
// server (`opencode serve`) instead of the one-shot `opencode run --format
// json` CLI.
//
// Why a server and not `opencode run`: `run` is a fire-and-forget child
// process. In v1.18.35 it hardcodes a permission ruleset that DENIES the
// `question`, `plan_enter` and `plan_exit` permissions, then auto-replies to
// any surviving `permission.asked` event with `once` (under `--auto`) or
// `reject` (without it). Its `--format json` emitter only serializes
// text/tool/step payloads, so permission and question events never reach
// stdout at all. In other words `run` structurally cannot carry an approval
// card or a user-question prompt, no matter how it is configured.
//
// `opencode serve` is opencode's own supported headless surface and exposes
// everything cc-webui needs over HTTP + SSE:
//
//	POST   /session                              create a session (permission ruleset here)
//	POST   /session/{id}/prompt                  send a turn (blocks until idle, or returns assistant message)
//	GET    /event                                SSE bus: all session events
//	GET    /api/permission/request               pending permission requests (reconnect catch-up)
//	POST   /api/session/{id}/permission/{rid}/reply  {reply:"once"|"always"|"reject", message?}
//	GET    /api/question/request                 pending question requests
//	POST   /api/session/{id}/question/{rid}/reply    {answers:[["label"],…]}
//	POST   /api/session/{id}/question/{rid}/reject
//	POST   /api/session/{id}/interrupt           stop a running turn
//	POST   /api/session/{id}/revert/stage        rewind to a message (+ optional file list)
//	POST   /api/session/{id}/revert/commit       apply the staged revert
//	GET    /api/session/{id}/message            message history
//
// One server process is shared by every opencode session in this cc-webui
// process; per-session state (cwd, model, permissions) rides the HTTP calls,
// so a single server serves many sessions. The server binds to 127.0.0.1 on an
// ephemeral port.
package opencodeapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Server is a running `opencode serve` child process plus its HTTP base URL.
type Server struct {
	baseURL string
	cmd     *exec.Cmd
	logFile *os.File

	mu     sync.Mutex
	closed bool
}

// Close shuts the child process down. Safe to call more than once.
func (s *Server) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.cmd != nil && s.cmd.Process != nil {
		// The serve process spawns its own children; kill the whole group the
		// same way the one-shot run path does so nothing is orphaned.
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		_ = s.cmd.Wait()
	}
	if s.logFile != nil {
		_ = s.logFile.Close()
	}
}

// startupTimeout bounds "spawn + wait for /global/health to answer".
const startupTimeout = 60 * time.Second

// Start launches `opencode serve` on a free loopback port and waits until it
// answers health checks. The child inherits env so provider credentials
// (ANTHROPIC_API_KEY and friends) resolve exactly as they would for `run`.
func Start(cliPath string, env []string, workDir string) (*Server, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	args := []string{
		"serve",
		"--port", fmt.Sprint(port),
		"--hostname", "127.0.0.1",
	}
	cmd := exec.Command(cliPath, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	cmd.Env = env
	// Its own process group: Close kills the group, and a stray server can
	// never take down the cc-webui binary.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// opencode writes a startup banner and provider warnings to stderr; keep
	// them out of cc-webui's stderr and surface them only in start errors.
	logFile, err := os.CreateTemp("", "opencode-serve-*.log")
	if err != nil {
		return nil, err
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		_ = os.Remove(logFile.Name())
		return nil, fmt.Errorf("start opencode serve: %w", err)
	}

	srv := &Server{
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		cmd:     cmd,
		logFile: logFile,
	}
	if err := srv.waitHealthy(startupTimeout); err != nil {
		tail := tailFile(logFile.Name(), 2048)
		srv.Close()
		_ = os.Remove(logFile.Name())
		if tail != "" {
			return nil, fmt.Errorf("opencode serve 未就绪：%w\n%s", err, tail)
		}
		return nil, fmt.Errorf("opencode serve 未就绪：%w", err)
	}
	return srv, nil
}

// BaseURL is the http://127.0.0.1:<port> root of the running server.
func (s *Server) BaseURL() string { return s.baseURL }

// PID is the child process id, or 0 if the process never started. Exposed so
// the owner can persist it and reap the server after an unclean exit.
func (s *Server) PID() int {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

func (s *Server) waitHealthy(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Second}
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/global/health", nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("health 状态码 %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return lastErr
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// freePort asks the kernel for an unused loopback port. opencode binds it
// moments later; the race window is negligible and a bind failure surfaces as
// a startup error.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func tailFile(path string, max int) string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return s
}

// ---- HTTP plumbing ----

func (s *Server) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("opencode %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// ---- domain types ----

// Session is opencode's session record (the fields cc-webui reads).
type Session struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Directory string   `json:"directory"`
	Agent     string   `json:"agent"`
	Model     ModelRef `json:"model"`
}

// CreateSessionInput creates a session on POST /session. Verified against the
// running server's OpenAPI in 1.18.35: {parentID, title, agent, model,
// metadata, permission, workspaceID}. `directory` is not part of the body —
// it rides as a query parameter, which is how opencode scopes per-project
// state.
type CreateSessionInput struct {
	Title      string           `json:"title,omitempty"`
	Agent      string           `json:"agent,omitempty"`
	Model      *ModelRef        `json:"model,omitempty"`
	Permission []PermissionRule `json:"permission,omitempty"`
	ParentID   string           `json:"parentID,omitempty"`
	// Query carries `directory`.
	Query map[string]any `json:"-"`
}

// ModelRef is opencode's provider/model pair.
type ModelRef struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant,omitempty"`
}

// RuleAction is one permission verdict. The create-session ruleset only
// accepts these three; the separate reply endpoint adds "always".
type RuleAction string

const (
	ActionAllow RuleAction = "allow"
	ActionAsk   RuleAction = "ask"
	ActionDeny  RuleAction = "deny"
	// ActionAlways is only valid in a permission *reply*, never in a ruleset.
	ActionAlways RuleAction = "always"
)

// PermissionRule gates one permission name against one pattern. Verified shape
// (1.18.35): {permission, pattern, action}, all three required.
type PermissionRule struct {
	Name    string     `json:"permission"`
	Pattern string     `json:"pattern"`
	Action  RuleAction `json:"action"`
}

// PromptInput is the v1 prompt body, verified against opencode 1.18.35's own
// generated types (packages/sdk/.../types.gen.ts, SessionPromptData.body):
//
//	{model:{providerID,modelID}, agent?, parts:[TextPartInput|FilePartInput…]}
//
// This is the shape `opencode run` itself uses (run.ts calls the v1
// `session.prompt`), which matters: the v2 `POST /api/session/{id}/prompt`
// endpoint takes a different envelope ({prompt:{text,files}}) and in 1.18.35
// only ADMITS the message — it returns `{admittedSeq}` in ~10ms without ever
// running the turn, so the agent goes silent. Measured directly: v1
// prompt_async spends real tokens and answers, v2 prompt never leaves 0/0.
// Do not "modernize" this to the v2 endpoint without re-testing.
type PromptInput struct {
	Model *ModelV1     `json:"model,omitempty"`
	Agent string       `json:"agent,omitempty"`
	Parts []PromptPart `json:"parts"`
	// System overrides the agent's system prompt (rarely needed).
	System string `json:"system,omitempty"`
	// Variant is the per-model reasoning-depth key, carried at the top level of
	// the v1 body (not inside `model`). This is how cc-webui applies a
	// hot-switched effort: the v2 `POST /api/session/{id}/model` route writes a
	// `session_message` row whose session_id is FOREIGN KEY'd to `session_v2`,
	// and sessions created through the v1 `POST /session` route live only in
	// the legacy `session` table — so that route answers 500 and the whole turn
	// dies. The v1 prompt runs the turn with model+variant and updates the
	// session's recorded model as a side effect (verified live).
	Variant string `json:"variant,omitempty"`
}

// ModelV1 is opencode's v1 model reference ({providerID, modelID}), distinct
// from the v2 ModelRef ({id, providerID}) used by session create / model switch.
type ModelV1 struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

// PromptPart is one input part. opencode's TextPartInput and FilePartInput are
// separate shapes and both are strict (`additionalProperties:false`):
//
//	text → {type:"text", text}          (text required)
//	file → {type:"file", mime, url}     (mime + url required; "text" is illegal)
//
// so a file part must carry its payload in `url` (a data: uri for inline
// images, a file:// uri for on-disk uploads), never in `text`. Leaving the
// wrong field unset (or an empty text part with the key omitted) makes
// opencode answer 400 "Missing key at parts[0].text".
type PromptPart struct {
	Type string `json:"type"`
	// Text is set only for type=="text". Omitted for file parts (illegal there).
	Text string `json:"text,omitempty"`
	// Mime is required for a file part.
	Mime string `json:"mime,omitempty"`
	// Url is the file payload: a data: uri (inline) or file:// uri (on disk).
	Url string `json:"url,omitempty"`
	// Filename is display-only.
	Filename string `json:"filename,omitempty"`
	// SourceID references a file added earlier via the file endpoints.
	SourceID string `json:"sourceID,omitempty"`
}

// CreateSession creates an opencode session rooted at dir.
func (s *Server) CreateSession(ctx context.Context, in CreateSessionInput) (*Session, error) {
	var out Session
	if err := s.do(ctx, http.MethodPost, queryPath("/session", in.Query), in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Prompt sends a turn on the v1 endpoint (POST /session/{id}/prompt).
//
// This call BLOCKS for the whole turn and resolves to the created assistant
// message — which is what makes it the right primitive here: the caller needs
// to know when the turn is over, and the v1 endpoint is the only one that
// actually runs the turn (see PromptInput's note on the v2 endpoint). SSE is
// used concurrently for the live event stream, not for turn completion.
func (s *Server) Prompt(ctx context.Context, sessionID string, in PromptInput) (json.RawMessage, error) {
	var raw json.RawMessage
	err := s.do(ctx, http.MethodPost,
		queryPath(fmt.Sprintf("/session/%s/prompt", sessionID), nil), in, &raw)
	return raw, err
}

// PromptAsync enqueues a turn without waiting for it (v1 prompt_async,
// returns 204). Used when the caller drives completion off the event bus
// instead of the POST.
func (s *Server) PromptAsync(ctx context.Context, sessionID string, in PromptInput) error {
	return s.do(ctx, http.MethodPost,
		queryPath(fmt.Sprintf("/session/%s/prompt_async", sessionID), nil), in, nil)
}

// Interrupt stops a running turn (the Stop button).
//
// The turns cc-webui drives use the v1 prompt_async path, whose abort is the
// flat /session/{id}/abort route. The v2 /api/session/{id}/interrupt route
// answers 204 but is a no-op for a v1 turn, so Stop would silently do nothing
// (verified live: v2 → the bash tool kept running; v1 abort → "Aborted" and
// the session went idle at once). directory scopes the abort to the session's
// project, exactly as create-session does.
func (s *Server) Interrupt(ctx context.Context, directory, sessionID string) error {
	path := queryPath(fmt.Sprintf("/session/%s/abort", sessionID),
		map[string]any{"directory": directory})
	return s.do(ctx, http.MethodPost, path, struct{}{}, nil)
}

// Messages returns the session's messages (for a fresh-session resync).
// The v2 endpoint wraps them in {data, cursor}; each message is a flat record
// ({id, type, time, …}) rather than the v1 {info, parts} pair.
func (s *Server) Messages(ctx context.Context, sessionID string) ([]map[string]any, error) {
	var env struct {
		Data []map[string]any `json:"data"`
	}
	if err := s.do(ctx, http.MethodGet,
		queryPath(fmt.Sprintf("/api/session/%s/message", sessionID), nil), nil, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

func queryPath(path string, q map[string]any) string {
	if len(q) == 0 {
		return path
	}
	parts := make([]string, 0, len(q))
	for k, v := range q {
		if s, ok := v.(string); ok && s != "" {
			parts = append(parts, k+"="+urlEscape(s))
		}
	}
	if len(parts) == 0 {
		return path
	}
	return path + "?" + strings.Join(parts, "&")
}

func urlEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' || c == '/' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// SwitchModel changes the model (and reasoning variant) of an existing
// session: POST /api/session/{id}/model {model:{id,providerID,variant}}.
//
// DO NOT call this from the turn path. It only works for sessions that live in
// opencode's `session_v2` table: the route writes a `model-switched` row into
// `session_message`, whose session_id is FOREIGN KEY'd to `session_v2`. Every
// cc-webui session is created through the v1 `POST /session` route and lives
// only in the legacy `session` table, so the insert fails with
// "FOREIGN KEY constraint failed" and the endpoint answers 500 (observed live
// on 1.18.35). Hot-switched model + variant ride the v1 prompt body instead —
// see PromptInput.Variant. Kept only as a wrapper for the route.
func (s *Server) SwitchModel(ctx context.Context, sessionID string, m ModelRef) error {
	return s.do(ctx, http.MethodPost,
		queryPath(fmt.Sprintf("/api/session/%s/model", sessionID), nil),
		map[string]any{"model": m}, nil)
}

// SessionModel reads the model a session is currently running on (GET
// /session/{id}, the v1 route — it answers for legacy sessions too). Returns
// the zero ModelRef when the server does not report one.
func (s *Server) SessionModel(ctx context.Context, sessionID string) (ModelRef, error) {
	var out Session
	if err := s.do(ctx, http.MethodGet,
		queryPath(fmt.Sprintf("/session/%s", sessionID), nil), nil, &out); err != nil {
		return ModelRef{}, err
	}
	return ModelRef{
		ID:         out.Model.ID,
		ProviderID: out.Model.ProviderID,
		Variant:    out.Model.Variant,
	}, nil
}

// opencode also exposes POST /api/session/{id}/agent for switching a
// session's agent, but cc-webui has no surface for it today (only model,
// provider and effort are exposed), so the client does not wrap it. Add
// it here when agent switching becomes a feature — do not assume it is
// already covered.
