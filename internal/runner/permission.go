package runner

import (
	"sync"
	"time"

	"cc-webui/internal/config"
)

// PermissionTimeout is how long the helper (PermissionRequest hook) waits for
// a user decision before auto-denying. Claude Code caps hook execution at 60s
// (per-hook timeout, up to 60s max), so this must stay below that budget with
// headroom for HTTP + spawn overhead.
const PermissionTimeout = 55 * time.Second

// PermissionDecision is the user's answer to one tool permission prompt.
type PermissionDecision struct {
	Behavior string `json:"behavior"` // "allow" | "allow_tool_session" | "deny"
	Message  string `json:"message,omitempty"`
	// Answers carries AskUserQuestion responses: question text -> option
	// label. The helper returns them as updatedInput.answers so the CLI
	// treats the questions as answered instead of skipped.
	Answers map[string]string `json:"answers,omitempty"`
}

// PermissionRequest is one outstanding tool approval prompt.
type PermissionRequest struct {
	ID        string         `json:"id"`
	SessionID string         `json:"session_id"`
	Tool      string         `json:"tool"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	ch        chan PermissionDecision
}

// permissionState holds the pending prompt registry plus per-session
// same-tool allowances ("本次会话都允许"). It lives on Manager so both the
// helper HTTP path (server) and the spawn path (runner) share it.
type permissionState struct {
	mu         sync.Mutex
	pending    map[string]*PermissionRequest
	allowances map[string]map[string]bool // sessionID -> tool name -> allowed
}

func newPermissionState() *permissionState {
	return &permissionState{
		pending:    map[string]*PermissionRequest{},
		allowances: map[string]map[string]bool{},
	}
}

// effectivePermissionMode returns the session's permission mode, falling back
// to the CLI default. Empty means "CLI default" (which prompts).
func effectivePermissionMode(sessPerm, cliPerm string) string {
	if sessPerm != "" {
		return sessPerm
	}
	return cliPerm
}

// permissionHookWanted reports whether a spawn should carry the
// PermissionRequest hook (interactive approval card).
//
// Only the stream-json dialects (claude + qoder, both Claude Code protocol)
// have a headless approval surface: the PermissionRequest hook via --settings.
//   - opencode has no headless permission prompt (--auto is auto-approve).
//   - mcode's "ask" needs an interactive TUI/ACP client; headless runs only
//     know smart/full.
// Bypass/dontAsk never prompt, so they need no hook either.
func permissionHookWanted(cli *config.CLI, sessPerm string) bool {
	if cli == nil {
		return false
	}
	kind := cli.Kind
	if kind == "" {
		kind = config.KindClaude
	}
	if kind != config.KindClaude && kind != config.KindQoder {
		return false
	}
	mode := effectivePermissionMode(sessPerm, cli.PermissionMode)
	// These modes never raise a prompt: no hook to answer it.
	if mode == "bypassPermissions" || mode == "dontAsk" {
		return false
	}
	return true
}

// permHookAllowed checks the same-tool session allowance without creating a
// pending entry. Callers use it to short-circuit before broadcasting.
func (m *Manager) permHookAllowed(sessionID, tool string) bool {
	if m.perm == nil || tool == "" {
		return false
	}
	m.perm.mu.Lock()
	defer m.perm.mu.Unlock()
	return m.perm.allowances[sessionID][tool]
}

// createPending registers a new outstanding prompt. The caller must broadcast
// the permission_request SSE event and eventually resolve or timeout it.
func (m *Manager) createPending(sessionID, tool string, input map[string]any, toolUseID string, id string) *PermissionRequest {
	pr := &PermissionRequest{
		ID:        id,
		SessionID: sessionID,
		Tool:      tool,
		Input:     input,
		ToolUseID: toolUseID,
		CreatedAt: time.Now(),
		ch:        make(chan PermissionDecision, 1),
	}
	m.perm.mu.Lock()
	m.perm.pending[id] = pr
	m.perm.mu.Unlock()
	return pr
}

// resolvePending records the decision (updating the allowance on
// allow_tool_session) and wakes the blocked helper. Returns false when the id
// is unknown (already timed out / resolved).
func (m *Manager) resolvePending(id string, d PermissionDecision) bool {
	m.perm.mu.Lock()
	pr, ok := m.perm.pending[id]
	if !ok {
		m.perm.mu.Unlock()
		return false
	}
	delete(m.perm.pending, id)
	// AskUserQuestion answers are per-question; never turn them into a
	// session allowance (the frontend offers no such button, this is belt
	// and braces against a crafted resolve call auto-answering later asks).
	if d.Behavior == "allow_tool_session" && pr.Tool != "" && pr.Tool != "AskUserQuestion" {
		allow := m.perm.allowances[pr.SessionID]
		if allow == nil {
			allow = map[string]bool{}
			m.perm.allowances[pr.SessionID] = allow
		}
		allow[pr.Tool] = true
		// Normalize to plain allow for the waiting helper: the allowance
		// already covers future calls, this call just needs approval.
		d.Behavior = "allow"
	}
	m.perm.mu.Unlock()
	select {
	case pr.ch <- d:
	default:
	}
	return true
}

// removePending drops a pending entry without resolving (timeout path keeps
// the entry removal + caller-side deny in one place).
func (m *Manager) removePending(id string) {
	m.perm.mu.Lock()
	delete(m.perm.pending, id)
	m.perm.mu.Unlock()
}

// pendingFor returns a snapshot of outstanding prompts for one session (for
// reconnects: permission events are transient SSE, never persisted).
func (m *Manager) pendingFor(sessionID string) []*PermissionRequest {
	m.perm.mu.Lock()
	defer m.perm.mu.Unlock()
	var out []*PermissionRequest
	for _, pr := range m.perm.pending {
		if pr.SessionID == sessionID {
			out = append(out, pr)
		}
	}
	return out
}

// BroadcastTransient sends a transient (non-persisted) event to SSE
// subscribers. Permission request/resolved events ride this path: they are
// live prompts, not history.
func (m *Manager) BroadcastTransient(sessionID, kind string, data any) {
	m.broadcast(sessionID, kind, data)
}

// PermissionAllowed reports a same-tool session allowance hit.
func (m *Manager) PermissionAllowed(sessionID, tool string) bool {
	return m.permHookAllowed(sessionID, tool)
}

// PermissionRequest registers a new outstanding prompt and returns it; the
// caller broadcasts the request event and waits on pr.ch (or times out).
func (m *Manager) PermissionRequest(sessionID, id, tool string, input map[string]any, toolUseID string) *PermissionRequest {
	return m.createPending(sessionID, tool, input, toolUseID, id)
}

// PermissionWaitChannel returns the decision channel for a pending id.
func (m *Manager) PermissionWaitChannel(id string) <-chan PermissionDecision {
	m.perm.mu.Lock()
	defer m.perm.mu.Unlock()
	if pr, ok := m.perm.pending[id]; ok {
		return pr.ch
	}
	return nil
}

// PermissionResolve records the user's decision. Returns false when unknown.
func (m *Manager) PermissionResolve(id string, d PermissionDecision) bool {
	return m.resolvePending(id, d)
}

// PermissionTimeoutRemove drops a timed-out entry. Returns true if it was
// still pending.
func (m *Manager) PermissionTimeoutRemove(id string) bool {
	m.perm.mu.Lock()
	_, ok := m.perm.pending[id]
	if ok {
		delete(m.perm.pending, id)
	}
	m.perm.mu.Unlock()
	return ok
}

// PermissionPending lists outstanding prompts for one session.
func (m *Manager) PermissionPending(sessionID string) []*PermissionRequest {
	return m.pendingFor(sessionID)
}

// ClearPermissions drops a session's allowances (call on session delete).
func (m *Manager) ClearPermissions(sessionID string) {
	if m.perm == nil {
		return
	}
	m.perm.mu.Lock()
	delete(m.perm.allowances, sessionID)
	m.perm.mu.Unlock()
}
