package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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

func sameOrigin(r *http.Request, trusted []string, trustForwarded bool) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.Host == "" || u.Hostname() == "" ||
		(u.Scheme != "http" && u.Scheme != "https") ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	// Direct access: the browser's Origin is the same scheme+host it used to
	// reach us. The scheme is not checked here because TLS may terminate at a
	// reverse proxy; the Host still pins the request to this deployment.
	if originHostMatches(u, r.Host) {
		return true
	}
	// Explicitly trusted origins cover proxies that rewrite Host and do not
	// forward the original one.
	for _, allowed := range trusted {
		if originMatchesAllowed(u, allowed) {
			return true
		}
	}
	// Behind a local proxy the request Host may be rewritten to
	// 127.0.0.1:PORT, so the public host only survives in X-Forwarded-*.
	// This is opt-in because a proxy that does not overwrite those headers
	// (nginx's default) forwards client-supplied values.
	if !trustForwarded || !loopbackRequest(r) {
		return false
	}
	forwardedHost := firstForwarded(r.Header.Get("X-Forwarded-Host"))
	if forwardedHost == "" || !originHostMatches(u, forwardedHost) {
		return false
	}
	forwardedProto := firstForwarded(r.Header.Get("X-Forwarded-Proto"))
	if forwardedProto == "" {
		return true
	}
	return strings.EqualFold(forwardedProto, u.Scheme)
}

// originMatchesAllowed compares an Origin against a configured
// scheme://host[:port] entry. Path/query/fragment are not meaningful for an
// origin, so only an empty path or "/" is accepted in the configuration.
func originMatchesAllowed(u *url.URL, allowed string) bool {
	v, err := url.Parse(strings.TrimSpace(allowed))
	if err != nil || v.User != nil || v.Hostname() == "" ||
		(v.Scheme != "http" && v.Scheme != "https") ||
		(v.Path != "" && v.Path != "/") || v.RawQuery != "" || v.Fragment != "" {
		return false
	}
	return strings.EqualFold(u.Scheme, v.Scheme) && sameHostPort(u, v)
}

// firstForwarded returns the first value of a comma-separated forwarding
// header. Proxies append hosts/protos from left to right; the first entry is
// the one the browser used.
func firstForwarded(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// originHostMatches reports whether the Origin URL and a Host header value
// name the same host and port. Empty ports are compared against the Origin
// scheme's default so https://example.com and example.com:443 are equal.
func originHostMatches(u *url.URL, host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	parsed, err := url.Parse("//" + host)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return false
	}
	return sameHostPort(u, parsed)
}

func sameHostPort(a, b *url.URL) bool {
	if !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	aPort := a.Port()
	if aPort == "" {
		aPort = defaultPort(a.Scheme)
	}
	bPort := b.Port()
	if bPort == "" {
		// A bare Host header has no scheme; its empty port means the default
		// port of the Origin's scheme (https://x and x:443 are the same).
		scheme := b.Scheme
		if scheme == "" {
			scheme = a.Scheme
		}
		bPort = defaultPort(scheme)
	}
	return aPort == bPort
}

func defaultPort(scheme string) string {
	if strings.EqualFold(scheme, "https") {
		return "443"
	}
	return "80"
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
	if !sameOrigin(r, s.cfg().Browser.TrustedOrigins, s.cfg().Browser.TrustForwardedHeaders) {
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
