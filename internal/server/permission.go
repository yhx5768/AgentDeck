package server

import (
	"encoding/json"
	"net/http"
	"time"

	"cc-webui/internal/runner"
)

// Permission endpoints (interactive approval card).
//
// Flow: the CLI's PermissionRequest hook runs this binary as
// `cc-webui -perm-helper` with CC_WEBUI_PERM_* env. The helper POSTs the tool
// call here and blocks; the server broadcasts a transient permission_request
// SSE event, the frontend renders the card, the user answers via the resolve
// endpoint, and this handler returns the decision to the blocked helper,
// which prints the hook JSON and exits. Allowance ("本次会话都允许") skips
// the card on later same-tool calls. Timeout (hook budget 60s, we wait 55s)
// auto-denies.
//
// This HTTP helper path serves claude and qoder: their spawns carry the
// PermissionRequest hook, which runs this binary as `cc-webui -perm-helper`.
//
// opencode does not use the helper — its engine drives `opencode serve` and
// answers opencode's permission.asked / question.asked events itself — but it
// still routes through the same pending registry, the same transient SSE
// events and this same resolve endpoint, so the frontend needs no engine
// branch. mcode (smart/full, no headless approver) is the only engine that
// never reaches this path at all.

func (s *Server) handlePermissionRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.st.Get(id)
	if sess == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	var body struct {
		Tool      string         `json:"tool"`
		Input     map[string]any `json:"input"`
		ToolUseID string         `json:"tool_use_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Tool == "" {
		body.Tool = "tool"
	}
	// Same-tool session allowance: approve silently, still notify the UI so
	// the turn shows "已按规则允许" instead of a mystery gap.
	if s.mgr.PermissionAllowed(id, body.Tool) {
		pid := newID()
		s.mgr.BroadcastTransient(id, "permission_request", map[string]any{
			"type": "permission_request", "id": pid, "tool": body.Tool,
			"input": body.Input, "tool_use_id": body.ToolUseID,
			"auto_allowed": true, "ts": time.Now().UnixMilli(),
		})
		s.mgr.BroadcastTransient(id, "permission_resolved", map[string]any{
			"type": "permission_resolved", "id": pid,
			"behavior": "allow", "auto_allowed": true,
		})
		writeJSON(w, http.StatusOK, map[string]any{"behavior": "allow", "auto_allowed": true})
		return
	}
	pid := newID()
	pr := s.mgr.PermissionRequest(id, pid, body.Tool, body.Input, body.ToolUseID)
	s.mgr.BroadcastTransient(id, "permission_request", map[string]any{
		"type": "permission_request", "id": pr.ID, "tool": pr.Tool,
		"input": pr.Input, "tool_use_id": pr.ToolUseID,
		"ts": pr.CreatedAt.UnixMilli(),
	})
	ch := s.mgr.PermissionWaitChannel(pid)
	if ch == nil {
		writeJSON(w, http.StatusOK, map[string]any{"behavior": "deny", "message": "permission queue lost the request"})
		return
	}
	select {
	case d := <-ch:
		s.mgr.BroadcastTransient(id, "permission_resolved", map[string]any{
			"type": "permission_resolved", "id": pid, "behavior": d.Behavior,
		})
		out := map[string]any{"behavior": d.Behavior}
		if d.Message != "" {
			out["message"] = d.Message
		}
		if len(d.Answers) > 0 {
			out["answers"] = d.Answers
		}
		writeJSON(w, http.StatusOK, out)
	case <-time.After(runner.PermissionTimeout):
		if s.mgr.PermissionTimeoutRemove(pid) {
			s.mgr.BroadcastTransient(id, "permission_resolved", map[string]any{
				"type": "permission_resolved", "id": pid,
				"behavior": "deny", "timeout": true,
			})
		}
		// CLI hook 上限 60s，无限等做不到（超了 CLI 直接杀 helper），55s 已是顶格。
		// AskUser 超时不断干：理由让模型稍后重问，等于变相续命；普通工具仍自动拒绝。
		msg := "用户 55s 内未审批，自动拒绝"
		if pr.Tool == "AskUserQuestion" {
			msg = "用户暂未回答，请稍后重新向用户提问"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"behavior": "deny", "message": msg,
		})
	}
}

func (s *Server) handlePermissionResolve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.st.Get(id) == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	pid := r.PathValue("pid")
	var body struct {
		Behavior string            `json:"behavior"`
		Message  string            `json:"message"`
		Answers  map[string]string `json:"answers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	switch body.Behavior {
	case "allow", "allow_tool_session", "deny":
	default:
		writeErr(w, http.StatusBadRequest, "behavior must be allow, allow_tool_session, or deny")
		return
	}
	if body.Behavior == "deny" && body.Message == "" {
		body.Message = "user denied"
	}
	// Answers ride straight into the tool's updatedInput (AskUserQuestion);
	// keep only non-empty string pairs, they are replayed verbatim to the CLI.
	answers := map[string]string{}
	for k, v := range body.Answers {
		if k != "" && v != "" {
			answers[k] = v
		}
	}
	ok := s.mgr.PermissionResolve(pid, runner.PermissionDecision{
		Behavior: body.Behavior, Message: body.Message, Answers: answers,
	})
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "prompt already resolved or timed out"})
		return
	}
	// The request handler broadcasts the resolved event when it wakes; also
	// broadcast here so a resolve racing the timeout still updates the UI.
	behavior := body.Behavior
	if behavior == "allow_tool_session" {
		behavior = "allow"
	}
	s.mgr.BroadcastTransient(id, "permission_resolved", map[string]any{
		"type": "permission_resolved", "id": pid, "behavior": behavior,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePermissionPending(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.st.Get(id) == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	pending := s.mgr.PermissionPending(id)
	out := make([]map[string]any, 0, len(pending))
	for _, pr := range pending {
		out = append(out, map[string]any{
			"id": pr.ID, "tool": pr.Tool, "input": pr.Input,
			"tool_use_id": pr.ToolUseID, "ts": pr.CreatedAt.UnixMilli(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": out})
}
