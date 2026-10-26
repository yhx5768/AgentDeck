package opencodev2

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Server is one private `opencode serve` child process.
//
// Unlike v1, a v2 server requires HTTP Basic auth on every /api route (the
// password is printed on stdout at startup); /global/health stays open so the
// readiness probe does not need it.
type Server struct {
	baseURL  string
	password string
	cmd      *exec.Cmd
	logFile  *os.File

	mu     sync.Mutex
	closed bool
}

// startupTimeout bounds "spawn + read the password + answer health".
const startupTimeout = 60 * time.Second

// Start launches `opencode serve` on a free loopback port, reads the password
// it prints, and waits until /global/health answers.
func Start(cliPath string, env []string, workDir string) (*Server, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	args := []string{"serve", "--port", strconv.Itoa(port), "--hostname", "127.0.0.1"}
	cmd := exec.Command(cliPath, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	logFile, err := os.CreateTemp("", "opencode-v2-serve-*.log")
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logFile.Close()
		return nil, err
	}
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		_ = os.Remove(logFile.Name())
		return nil, fmt.Errorf("start opencode serve: %w", err)
	}

	srv := &Server{
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		cmd:     cmd,
		logFile: logFile,
	}

	// The banner ("server listening on …" / "server password …") is the only
	// way to learn the auth password; keep draining stdout afterwards so the
	// pipe can never fill and block the server.
	pwCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			_, _ = fmt.Fprintln(logFile, line)
			if pw, ok := parseServerPassword(line); ok {
				select {
				case pwCh <- pw:
				default:
				}
			}
		}
	}()

	select {
	case pw := <-pwCh:
		srv.password = pw
	case <-time.After(startupTimeout):
		srv.Close()
		return nil, fmt.Errorf("opencode serve 未输出启动密码：%s", tailFile(logFile.Name(), 1024))
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

// parseServerPassword extracts the password from a serve banner line.
func parseServerPassword(line string) (string, bool) {
	const marker = "server password"
	i := strings.Index(line, marker)
	if i < 0 {
		return "", false
	}
	pw := strings.TrimSpace(line[i+len(marker):])
	if pw == "" {
		return "", false
	}
	return pw, true
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

// Close kills the server's process group. Safe to call more than once.
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
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		_ = s.cmd.Wait()
	}
	if s.logFile != nil {
		_ = s.logFile.Close()
	}
}

func (s *Server) PID() int {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

func (s *Server) BaseURL() string { return s.baseURL }

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
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, rdr)
	if err != nil {
		return err
	}
	if s.password != "" {
		req.SetBasicAuth("opencode", s.password)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("opencode %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// ---- API types ----

// ModelRef addresses one model (and optional reasoning variant) the way v2's
// session/prompt API expects.
type ModelRef struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant,omitempty"`
}

// PermissionRule is one entry of the v2 ordered permission ruleset.
type PermissionRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"` // allow | ask | deny
}

// SessionState is the subset of a v2 session the live turn needs.
type SessionState struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	ParentID string    `json:"parentID"`
	Model    *ModelRef `json:"model"`
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
}

// PromptFile is one attachment on a v2 prompt.
type PromptFile struct {
	URI         string `json:"uri"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// PromptInput is the v2 prompt body (v2 shape: flat text + files, not v1's
// {model, parts} envelope).
type PromptInput struct {
	Text  string       `json:"text"`
	Files []PromptFile `json:"files,omitempty"`
}

// PromptResult is the admitted user message.
type PromptResult struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
}

// PermissionRequest is one pending permission ask (also the `permission.asked`
// event payload).
type PermissionRequest struct {
	ID        string   `json:"id"`
	SessionID string   `json:"sessionID"`
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
	Save      []string `json:"save"`
	Source    struct {
		Type      string `json:"type"`
		MessageID string `json:"messageID"`
		ID        string `json:"id"`
	} `json:"source"`
}

// FormOption is one selectable form option.
type FormOption struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// FormField is one field of a session form (the v2 replacement for the
// question tool).
type FormField struct {
	Key         string       `json:"key"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Type        string       `json:"type"` // string | number | integer | boolean | multiselect | external
	Required    bool         `json:"required"`
	Options     []FormOption `json:"options"`
	Custom      bool         `json:"custom"`
}

// FormInfo is a pending form.
type FormInfo struct {
	ID        string      `json:"id"`
	SessionID string      `json:"sessionID"`
	Title     string      `json:"title"`
	Fields    []FormField `json:"fields"`
}

// RevertFile is one file touched by a staged revert.
type RevertFile struct {
	Path    string `json:"path"`
	Hash    string `json:"hash"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Time    int64  `json:"time"`
}

// RevertStaged is the staged revert preview.
type RevertStaged struct {
	MessageID string       `json:"messageID"`
	Files     []RevertFile `json:"files"`
}

// ---- API calls ----

// CreateSession creates a v2 session at a directory with a model and
// permission ruleset.
func (s *Server) CreateSession(ctx context.Context, title, dir string, model *ModelRef, perms []PermissionRule) (*SessionState, error) {
	body := map[string]any{
		"title":    title,
		"location": map[string]any{"directory": dir},
	}
	if model != nil {
		body["model"] = model
	}
	if len(perms) > 0 {
		body["permissions"] = perms
	}
	var out struct {
		Data SessionState `json:"data"`
	}
	if err := s.do(ctx, http.MethodPost, "/api/session", body, &out); err != nil {
		return nil, err
	}
	return &out.Data, nil
}

// GetSession reads a session's current model/profile.
func (s *Server) GetSession(ctx context.Context, sessionID string) (*SessionState, error) {
	var out struct {
		Data SessionState `json:"data"`
	}
	if err := s.do(ctx, http.MethodGet, "/api/session/"+sessionID, nil, &out); err != nil {
		return nil, err
	}
	return &out.Data, nil
}

// SwitchModel changes the model subsequent turns run on.
func (s *Server) SwitchModel(ctx context.Context, sessionID string, model ModelRef) error {
	return s.do(ctx, http.MethodPost, "/api/session/"+sessionID+"/model",
		map[string]any{"model": model}, nil)
}

// Prompt admits one user message. v2's prompt is durable and schedules the
// agent loop itself; the turn boundary arrives later on the event stream.
func (s *Server) Prompt(ctx context.Context, sessionID string, in PromptInput) (*PromptResult, error) {
	var out struct {
		Data PromptResult `json:"data"`
	}
	if err := s.do(ctx, http.MethodPost, "/api/session/"+sessionID+"/prompt", in, &out); err != nil {
		return nil, err
	}
	return &out.Data, nil
}

// Interrupt stops the session's running execution.
func (s *Server) Interrupt(ctx context.Context, sessionID string) error {
	return s.do(ctx, http.MethodPost, "/api/session/"+sessionID+"/interrupt", map[string]any{}, nil)
}

// ListPermissions returns the pending permission asks of one session.
func (s *Server) ListPermissions(ctx context.Context, sessionID string) ([]PermissionRequest, error) {
	var out struct {
		Data []PermissionRequest `json:"data"`
	}
	if err := s.do(ctx, http.MethodGet, "/api/session/"+sessionID+"/permission", nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// ReplyPermission answers one permission ask.
func (s *Server) ReplyPermission(ctx context.Context, sessionID, requestID, decision, message string) error {
	body := map[string]any{"decision": decision}
	if message != "" {
		body["message"] = message
	}
	return s.do(ctx, http.MethodPost, "/api/session/"+sessionID+"/permission/"+requestID+"/reply", body, nil)
}

// ListForms returns the pending forms of one session.
func (s *Server) ListForms(ctx context.Context, sessionID string) ([]FormInfo, error) {
	var out struct {
		Data []FormInfo `json:"data"`
	}
	if err := s.do(ctx, http.MethodGet, "/api/session/"+sessionID+"/form", nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// ReplyForm submits answers to a form.
func (s *Server) ReplyForm(ctx context.Context, sessionID, formID string, answer map[string]any) error {
	return s.do(ctx, http.MethodPost, "/api/session/"+sessionID+"/form/"+formID+"/reply",
		map[string]any{"answer": answer}, nil)
}

// RejectForm cancels a form.
func (s *Server) RejectForm(ctx context.Context, sessionID, formID string) error {
	return s.do(ctx, http.MethodDelete, "/api/session/"+sessionID+"/form/"+formID, nil, nil)
}

// StageRevert previews a revert to a message, reporting affected files.
func (s *Server) StageRevert(ctx context.Context, sessionID, messageID string) (*RevertStaged, error) {
	var out struct {
		Data RevertStaged `json:"data"`
	}
	body := map[string]any{"messageID": messageID, "files": true}
	if err := s.do(ctx, http.MethodPost, "/api/session/"+sessionID+"/revert/stage", body, &out); err != nil {
		return nil, err
	}
	return &out.Data, nil
}

// CommitRevert applies the staged revert.
func (s *Server) CommitRevert(ctx context.Context, sessionID string) error {
	return s.do(ctx, http.MethodPost, "/api/session/"+sessionID+"/revert/commit", map[string]any{}, nil)
}

// ClearRevert discards a staged revert.
func (s *Server) ClearRevert(ctx context.Context, sessionID string) error {
	return s.do(ctx, http.MethodDelete, "/api/session/"+sessionID+"/revert", nil, nil)
}
