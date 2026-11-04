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
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
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
	switch name {
	case "list_sessions":
		return s.a2aListSessionsJSON()
	case "send_message":
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
		code, body := s.callHandler(s.handleA2ASend, http.MethodPost, "/api/a2a/send", map[string]any{
			"from": from, "to": to, "message": message,
		}, nil)
		return resultText(code, body, "message sent")
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
