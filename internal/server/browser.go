package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"cc-webui/internal/browser"
)

func loopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sameOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	return strings.EqualFold(origin, "http://"+r.Host) || strings.EqualFold(origin, "https://"+r.Host)
}

func (s *Server) handleBrowserStatus(w http.ResponseWriter, r *http.Request) {
	if s.browser == nil {
		writeErr(w, http.StatusServiceUnavailable, "浏览器管理器未初始化")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": s.browser.Status()})
}

func (s *Server) handleBrowserEnable(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
		Install bool `json:"install"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if s.browser == nil {
		writeErr(w, http.StatusServiceUnavailable, "浏览器管理器未初始化")
		return
	}
	if !body.Enabled {
		status, err := s.browser.Disable()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": status})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
		return
	}
	status, err := s.browser.Enable(r.Context(), body.Install)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "status": status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
}

func (s *Server) handleBrowserDisable(w http.ResponseWriter, r *http.Request) {
	if s.browser == nil {
		writeErr(w, http.StatusServiceUnavailable, "浏览器管理器未初始化")
		return
	}
	status, err := s.browser.Disable()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
}

func (s *Server) handleBrowserProfileMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Shared bool `json:"shared"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if s.browser == nil {
		writeErr(w, http.StatusServiceUnavailable, "浏览器管理器未初始化")
		return
	}
	status, err := s.browser.SetSharedProfile(body.Shared)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "status": status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
}

func (s *Server) handleBrowserInstall(w http.ResponseWriter, r *http.Request) {
	if s.browser == nil {
		writeErr(w, http.StatusServiceUnavailable, "浏览器管理器未初始化")
		return
	}
	status, err := s.browser.Install(r.Context())
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "status": status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
}

func (s *Server) handleBrowserSessionStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.st.Get(id) == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if s.browser == nil {
		writeErr(w, http.StatusServiceUnavailable, "浏览器管理器未初始化")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": s.browser.SessionStatus(id)})
}

func (s *Server) handleBrowserInput(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.st.Get(id) == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if s.browser == nil {
		writeErr(w, http.StatusServiceUnavailable, "浏览器管理器未初始化")
		return
	}
	if !sameOrigin(r) {
		writeErr(w, http.StatusForbidden, "cross-origin browser input refused")
		return
	}
	var ev browser.InputEvent
	if err := decode(r, &ev); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if err := s.browser.Input(r.Context(), id, ev); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "status": s.browser.SessionStatus(id)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": s.browser.SessionStatus(id)})
}

func (s *Server) handleBrowserStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.st.Get(id) == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if s.browser == nil {
		writeErr(w, http.StatusServiceUnavailable, "浏览器管理器未初始化")
		return
	}
	if !s.browser.Enabled() {
		writeErr(w, http.StatusConflict, "浏览器功能未开启")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch, unsub, err := s.browser.Subscribe(id)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	defer unsub()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	send := func(event string, payload any) bool {
		raw, err := json.Marshal(payload)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	send("status", s.browser.SessionStatus(id))
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case frame, ok := <-ch:
			if !ok {
				return
			}
			if !send("frame", frame) {
				return
			}
		case <-ticker.C:
			if !send("status", s.browser.SessionStatus(id)) {
				return
			}
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// handleInternalBrowserMCP is the loopback endpoint the stdio proxy calls.
// It uses the gateway token as a bearer capability and requires the exact
// AgentDeck session id in X-CC-Webui-Session.
func (s *Server) handleInternalBrowserMCP(w http.ResponseWriter, r *http.Request) {
	if !loopbackRequest(r) {
		writeErr(w, http.StatusForbidden, "loopback only")
		return
	}
	if !s.tokenOK(r) {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	session := strings.TrimSpace(r.Header.Get("X-CC-Webui-Session"))
	if session == "" {
		writeErr(w, http.StatusBadRequest, "missing session")
		return
	}
	if s.st.Get(session) == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if s.browser == nil {
		writeErr(w, http.StatusServiceUnavailable, "浏览器管理器未初始化")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16*1024*1024))
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
	result, err := s.browser.RPC(r.Context(), session, req.Method, req.Params)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}
