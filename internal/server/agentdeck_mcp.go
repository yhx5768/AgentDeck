package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"cc-webui/internal/a2a"
	"cc-webui/internal/store"
)

// handleInternalAgentdeckMCP is the loopback endpoint the agentdeck stdio MCP
// proxy calls. It authenticates with the gateway token as a bearer capability
// (like the browser MCP endpoint) and optionally reads the calling session id
// from X-CC-Webui-Session — unlike browser MCP, the session may be absent
// (opencode/mcode inject one shared MCP config, not a per-session one).
func (s *Server) handleInternalAgentdeckMCP(w http.ResponseWriter, r *http.Request) {
	if !loopbackRequest(r) {
		writeErr(w, http.StatusForbidden, "loopback only")
		return
	}
	if !s.tokenOK(r) {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	session := strings.TrimSpace(r.Header.Get("X-CC-Webui-Session"))
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxArtifactBodyBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	switch req.Method {
	case "tools/list":
		writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"tools": a2a.Tools}})
	case "tools/call":
		var call struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &call); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid tools/call params")
			return
		}
		result, err := s.agentdeckToolCall(session, call.Name, call.Arguments)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{
				"content": a2a.TextContent(err.Error()), "isError": true,
			}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{
			"content": a2a.TextContent(result),
		}})
	default:
		writeErr(w, http.StatusBadRequest, "method not found: "+req.Method)
	}
}

// callHandler runs one of the existing HTTP handlers in-process and returns its
// status + body. The MCP tools are thin wrappers over the same handlers the UI
// and the agentdeck skill use, so validation and persistence stay single-sourced.
func (s *Server) callHandler(h http.HandlerFunc, method, path string, body any, pathVals map[string]string) (int, []byte) {
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rdr)
	// In-process calls have no real client host; clear the placeholder so
	// handlers that mint absolute URLs fall back to config/listen instead of
	// emitting http://example.com.
	req.Host = ""
	for k, v := range pathVals {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// agentdeckToolCall dispatches one MCP tool. It returns the text the model sees.
func (s *Server) agentdeckToolCall(sessionID, name string, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	// Board tools are main-agent only — re-checked here so a worker (or any
	// client that guesses a tool name) can never write the board.
	if a2a.IsBoardTool(name) {
		if err := s.requireMainAgent(sessionID); err != nil {
			return "", err
		}
		return s.agentdeckBoardCall(name, args)
	}
	// Knowledge review/collect is main-agent only, with the same two-layer gate
	// as the board tools (tool list + server-side main_agent re-check).
	if a2a.IsKnowledgeAdminTool(name) {
		if err := s.requireMainAgent(sessionID); err != nil {
			return "", err
		}
		actor := s.knowledgeActorForMCP(sessionID)
		switch name {
		case "knowledge_review":
			id := stringArg(args, "id")
			if id == "" {
				return "", fmt.Errorf("knowledge_review requires 'id'")
			}
			result, err := s.knowledgeReviewData(actor, id, args)
			return jsonResult(result, err)
		case "knowledge_collect":
			result, err := s.knowledgeCollectData(actor)
			return jsonResult(result, err)
		}
		return "", fmt.Errorf("unknown knowledge admin tool: %s", name)
	}
	switch name {
	case "knowledge_search":
		actor := s.knowledgeActorForMCP(sessionID)
		result, err := s.knowledgeSearchData(actor, knowledgeSearchParamsFromArgs(args))
		return jsonResult(result, err)
	case "knowledge_get":
		actor := s.knowledgeActorForMCP(sessionID)
		id := stringArg(args, "id")
		if id == "" {
			return "", fmt.Errorf("knowledge_get requires 'id'")
		}
		result, err := s.knowledgeGetData(actor, id)
		return jsonResult(result, err)
	case "knowledge_add":
		actor := s.knowledgeActorForMCP(sessionID)
		result, err := s.knowledgeAddData(actor, args)
		return jsonResult(result, err)
	case "list_sessions":
		return s.a2aListSessionsJSON()
	case "send_message":
		return s.agentdeckSendMessage(sessionID, args)
	case "read_session_messages":
		session, _ := args["session"].(string)
		if strings.TrimSpace(session) == "" {
			return "", fmt.Errorf("read_session_messages requires 'session'")
		}
		return s.handleReadSessionMessages(session, intArg(args, "before"), intArg(args, "limit"))
	case "search_messages":
		query, _ := args["query"].(string)
		session, _ := args["session"].(string)
		return s.handleSearchMessages(query, session, intArg(args, "limit"))
	case "artifact_publish":
		// Not a board tool: workers on remote machines must be able to publish
		// artifacts too. The server still validates the task id and payload.
		return s.agentdeckPublishArtifact(sessionID, args)
	case "new_session":
		code, body := s.callHandler(s.handleCreateSession, http.MethodPost, "/api/sessions", args, nil)
		return resultText(code, body, "")
	case "get_session":
		id, _ := args["id"].(string)
		if strings.TrimSpace(id) == "" {
			return "", fmt.Errorf("get_session requires 'id'")
		}
		code, body := s.callHandler(s.handleGetSession, http.MethodGet, "/api/sessions/"+id, nil,
			map[string]string{"id": id})
		return resultText(code, body, "")
	case "stop_session":
		id, _ := args["id"].(string)
		if strings.TrimSpace(id) == "" {
			return "", fmt.Errorf("stop_session requires 'id'")
		}
		code, body := s.callHandler(s.handleStop, http.MethodPost, "/api/sessions/"+id+"/stop", nil,
			map[string]string{"id": id})
		return resultText(code, body, "stopped")
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// agentdeckSendMessage is the send_message tool. It is idempotent on an
// optional `send_id`: a retry with the same id (from the same sender) returns
// the first delivery's receipt instead of delivering the message twice, so an
// agent that loses a reply can safely resend.
func (s *Server) agentdeckSendMessage(sessionID string, args map[string]any) (string, error) {
	to, _ := args["to"].(string)
	message, _ := args["message"].(string)
	if strings.TrimSpace(to) == "" || strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("send_message requires 'to' and 'message'")
	}
	from, _ := args["from"].(string)
	if strings.TrimSpace(from) == "" && sessionID != "" {
		if caller := s.st.Get(sessionID); caller != nil {
			from = sessionAddress(caller)
		}
	}
	// The HTTP handler owns idempotency: it looks up send_id in sendDedup and
	// returns the first receipt (with duplicate:true) instead of re-delivering.
	payload := map[string]any{"from": from, "to": to, "message": message}
	if sendID := strings.TrimSpace(stringArg(args, "send_id")); sendID != "" {
		payload["send_id"] = sendID
	}
	code, body := s.callHandler(s.handleA2ASend, http.MethodPost, "/api/a2a/send", payload, nil)
	result, err := resultText(code, body, "message sent")
	if err != nil {
		return "", err
	}
	// A deduplicated replay must not re-comment the card.
	var receipt map[string]any
	_ = json.Unmarshal(body, &receipt)
	if dup, _ := receipt["duplicate"].(bool); dup {
		return result, nil
	}
	// Optional board bookkeeping: a task_id on the send records a one-line
	// comment on that card, so dispatch/report does not need a separate
	// board_comment call. A missing/unknown card must not fail the send.
	if taskID, _ := args["task_id"].(string); strings.TrimSpace(taskID) != "" {
		s.appendA2AComment(strings.TrimSpace(taskID), from, to, message)
	}
	return result, nil
}

// dedupFields returns a remembered send receipt with duplicate=true so the
// caller can tell a replay from a fresh delivery.
func dedupFields(receipt map[string]any) map[string]any {
	out := map[string]any{"duplicate": true}
	for k, v := range receipt {
		out[k] = v
	}
	return out
}

func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func intArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

// requireMainAgent rejects board-tool calls from anything but the workspace
// main agent (defence in depth: the proxy only advertises board tools to main
// agents, but a caller could still guess a tool name).
func (s *Server) requireMainAgent(sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("看板工具仅主 Agent 可用")
	}
	if sess := s.st.Get(sessionID); sess == nil || !sess.MainAgent {
		return fmt.Errorf("看板工具仅主 Agent 可用")
	}
	return nil
}

// agentdeckBoardCall dispatches the main-agent board tools onto the existing
// board HTTP handlers, so validation/persistence stay single-sourced.
func (s *Server) agentdeckBoardCall(name string, args map[string]any) (string, error) {
	strArg := func(k string) string { v, _ := args[k].(string); return strings.TrimSpace(v) }
	switch name {
	case "board_list":
		code, body := s.callHandler(s.handleAgentBoardDB, http.MethodGet, "/api/agent-board", nil, nil)
		if code >= 400 {
			return resultText(code, body, "")
		}
		return filterBoardTasks(body, strArg("project"), strArg("status"))
	case "board_get":
		id := strArg("id")
		if id == "" {
			return "", fmt.Errorf("board_get requires 'id'")
		}
		code, body := s.callHandler(s.handleAgentBoardGetTask, http.MethodGet, "/api/agent-board/tasks/"+id, nil, map[string]string{"id": id})
		return resultText(code, body, "")
	case "board_create":
		if strArg("title") == "" {
			return "", fmt.Errorf("board_create requires 'title'")
		}
		code, body := s.callHandler(s.handleAgentBoardCreateTask, http.MethodPost, "/api/agent-board/tasks", args, nil)
		return resultText(code, body, "")
	case "board_update":
		id := strArg("id")
		if id == "" {
			return "", fmt.Errorf("board_update requires 'id'")
		}
		patch := map[string]any{}
		for k, v := range args {
			if k != "id" {
				patch[k] = v
			}
		}
		if len(patch) == 0 {
			return "", fmt.Errorf("board_update requires at least one field to change")
		}
		code, body := s.callHandler(s.handleAgentBoardUpdateTask, http.MethodPatch, "/api/agent-board/tasks/"+id, patch, map[string]string{"id": id})
		return resultText(code, body, "")
	case "board_comment":
		id, text := strArg("id"), strArg("text")
		if id == "" || text == "" {
			return "", fmt.Errorf("board_comment requires 'id' and 'text'")
		}
		body := map[string]any{"text": text}
		for _, k := range []string{"kind", "at", "source"} {
			if v, ok := args[k]; ok {
				body[k] = v
			}
		}
		code, resp := s.callHandler(s.handleAgentBoardAppendEventDB, http.MethodPost, "/api/agent-board/tasks/"+id+"/events", body, map[string]string{"id": id})
		return resultText(code, resp, "")
	case "board_bind_worker":
		id := strArg("id")
		if id == "" || strArg("worker") == "" {
			return "", fmt.Errorf("board_bind_worker requires 'id' and 'worker'")
		}
		body := map[string]any{}
		for _, k := range []string{"worker", "session", "cwd"} {
			if v, ok := args[k]; ok {
				body[k] = v
			}
		}
		code, resp := s.callHandler(s.handleAgentBoardBindWorker, http.MethodPost, "/api/agent-board/tasks/"+id+"/worker", body, map[string]string{"id": id})
		return resultText(code, resp, "")
	default:
		return "", fmt.Errorf("unknown board tool: %s", name)
	}
}

// filterBoardTasks returns the snapshot's tasks (optionally narrowed by project
// and/or status) as a JSON array.
func filterBoardTasks(snapshot []byte, project, status string) (string, error) {
	var snap struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		return "", err
	}
	out := make([]map[string]any, 0, len(snap.Tasks))
	for _, t := range snap.Tasks {
		if project != "" {
			if p, _ := t["project"].(string); p != project {
				continue
			}
		}
		if status != "" {
			if st, _ := t["status"].(string); st != status {
				continue
			}
		}
		out = append(out, t)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// appendA2AComment records an A2A delivery on a board task's timeline (the
// `task_id` option on send_message). Failures are swallowed: the message itself
// was already delivered, and a missing/unknown card must not fail the send.
func (s *Server) appendA2AComment(taskID, from, to, message string) {
	line := a2aFirstLine(message)
	if line == "" {
		line = "(空消息)"
	}
	s.callHandler(s.handleAgentBoardAppendEventDB, http.MethodPost,
		"/api/agent-board/tasks/"+taskID+"/events",
		map[string]any{
			"kind":   "路由事件",
			"text":   fmt.Sprintf("⇢ %s → %s：%s", from, to, line),
			"source": "a2a",
		},
		map[string]string{"id": taskID})
}

// a2aFirstLine returns the first non-empty line of s, capped at 200 runes.
func a2aFirstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		r := []rune(t)
		if len(r) > 200 {
			return string(r[:200]) + "…"
		}
		return t
	}
	return ""
}

// resultText turns an in-process handler response into the model-facing string:
// the raw JSON on success (so the model can read ids/fields), an error on 4xx/5xx.
func resultText(code int, body []byte, okText string) (string, error) {
	if code >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return "", fmt.Errorf("%s", e.Error)
		}
		return "", fmt.Errorf("request failed (HTTP %d)", code)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return okText, nil
	}
	return string(body), nil
}

// a2aListSessionsJSON renders every session as a compact JSON array.
func (s *Server) a2aListSessionsJSON() (string, error) {
	list := s.st.List()
	out := make([]map[string]any, 0, len(list))
	for _, sess := range list {
		if sess.GroupID != "" {
			continue // group shadow sessions are internal
		}
		if sess.Archived {
			continue // archived sessions are not dispatch targets
		}
		out = append(out, s.a2aSessionSummary(sess))
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (s *Server) a2aSessionSummary(sess *store.Session) map[string]any {
	st := s.mgr.GetStatus(sess.ID)
	return map[string]any{
		"id":              sess.ID,
		"name":            sess.Name,
		"address":         sessionAddress(sess),
		"engine":          s.engineOf(sess),
		"cli":             sess.CLI,
		"provider":        sess.Provider,
		"model":           sess.ModelOverride,
		"cwd":             sess.Cwd,
		"resident":        sess.Resident,
		"online":          st.Resident,
		"registered_name": st.RegisteredName,
		"running":         st.Running,
		"queued":          st.Queued,
		"main_agent":      sess.MainAgent,
	}
}
