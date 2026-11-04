package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cc-webui/internal/a2a"
	"cc-webui/internal/store"
)

// waitIdle blocks until the session's async worker drains. Enqueue starts a
// goroutine that spawns the (missing) cli and fails fast; without waiting, the
// test's TempDir cleanup races that goroutine's event writes.
func waitIdle(t *testing.T, s *Server, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !s.mgr.Busy(id) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func putSession(t *testing.T, st *store.Store, sess *store.Session) {
	t.Helper()
	if err := st.Upsert(sess, nil); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
}

func TestA2ASendDeliversAsPeerEvent(t *testing.T) {
	s, st := createResidentServer(t)
	putSession(t, st, &store.Session{
		ID: "t1", Name: "主Agent", PeerName: "mainAgent",
		Cwd: t.TempDir(), CLI: "claude", Provider: "gw",
	})

	raw, _ := json.Marshal(map[string]any{
		"from": "worker-1", "to": "mainAgent", "message": "【回报】done",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/a2a/send", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	s.handleA2ASend(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("send: code = %d, body=%s", w.Code, w.Body.String())
	}
	waitIdle(t, s, "t1")

	evs, _ := st.Events("t1")
	var user *store.Event
	for _, e := range evs {
		if e.Kind == "user" {
			user = e
		}
	}
	if user == nil {
		t.Fatal("no persisted user event for the A2A message")
	}
	var d map[string]string
	if err := json.Unmarshal(user.Data, &d); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if d["source"] != a2a.Source {
		t.Fatalf("source = %q, want %q", d["source"], a2a.Source)
	}
	if d["from"] != "worker-1" || d["body"] != "【回报】done" {
		t.Fatalf("from/body = %q/%q", d["from"], d["body"])
	}
	// The stored text must be the envelope: the engine echoes it verbatim and
	// mergeStoredAndClaude dedups the echo by trimmed-text match.
	from, body, ok := a2a.Parse(d["text"])
	if !ok || from != "worker-1" || body != "【回报】done" {
		t.Fatalf("stored text is not a matching envelope: %q", d["text"])
	}
}

func TestA2ASendResolvesByNameFallback(t *testing.T) {
	s, st := createResidentServer(t)
	putSession(t, st, &store.Session{
		ID: "t2", Name: "cc-webui-修SSE", Cwd: t.TempDir(), CLI: "claude", Provider: "gw",
	})
	if got := s.resolveAddress("cc-webui-修SSE"); got == nil || got.ID != "t2" {
		t.Fatalf("resolveAddress by display name failed: %+v", got)
	}
	// Case-insensitive match against the cleaned address.
	putSession(t, st, &store.Session{
		ID: "t3", Name: "Worker", PeerName: "Alice", Cwd: t.TempDir(), CLI: "claude", Provider: "gw",
	})
	if got := s.resolveAddress("alice"); got == nil || got.ID != "t3" {
		t.Fatalf("resolveAddress case-insensitive failed: %+v", got)
	}
	// "name [ref]" disambiguator is stripped.
	if got := s.resolveAddress("Alice [3fa9c1]"); got == nil || got.ID != "t3" {
		t.Fatalf("resolveAddress ref-strip failed: %+v", got)
	}
}

func TestAgentdeckMCPEndpoint(t *testing.T) {
	s, st := createResidentServer(t)
	putSession(t, st, &store.Session{
		ID: "t1", Name: "主Agent", PeerName: "mainAgent",
		Cwd: t.TempDir(), CLI: "claude", Provider: "gw",
	})

	mcpPost := func(method string, params any, remote string) *httptest.ResponseRecorder {
		payload := map[string]any{"method": method}
		if params != nil {
			payload["params"] = params
		}
		raw, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/internal/agentdeck/mcp", bytes.NewReader(raw))
		req.RemoteAddr = remote
		req.Header.Set("X-CC-Webui-Session", "t1")
		w := httptest.NewRecorder()
		s.handleInternalAgentdeckMCP(w, req)
		return w
	}

	// tools/list exposes the surface.
	w := mcpPost("tools/list", nil, "127.0.0.1:12345")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "send_message") {
		t.Fatalf("tools/list: code=%d body=%s", w.Code, w.Body.String())
	}

	// tools/call list_sessions renders sessions with their address.
	w = mcpPost("tools/call", map[string]any{"name": "list_sessions", "arguments": map[string]any{}}, "127.0.0.1:12345")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "mainAgent") {
		t.Fatalf("list_sessions: code=%d body=%s", w.Code, w.Body.String())
	}

	// tools/call send_message delivers over the bus; `from` defaults to the
	// calling session's address when the session header is present.
	w = mcpPost("tools/call", map[string]any{
		"name":      "send_message",
		"arguments": map[string]any{"to": "mainAgent", "message": "hi from mcp"},
	}, "127.0.0.1:12345")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "isError") {
		t.Fatalf("send_message: code=%d body=%s", w.Code, w.Body.String())
	}
	waitIdle(t, s, "t1")
	evs, _ := st.Events("t1")
	found := false
	for _, e := range evs {
		if e.Kind == "user" && strings.Contains(string(e.Data), "hi from mcp") {
			found = true
		}
	}
	if !found {
		t.Fatal("send_message did not persist an a2a user event")
	}

	// A non-loopback caller is rejected outright.
	if w := mcpPost("tools/list", nil, "10.0.0.1:9999"); w.Code != http.StatusForbidden {
		t.Fatalf("non-loopback: code=%d, want 403", w.Code)
	}
}

func TestA2ASendUnknownAddress(t *testing.T) {
	s, _ := createResidentServer(t)
	raw, _ := json.Marshal(map[string]any{"to": "nobody", "message": "hi"})
	req := httptest.NewRequest(http.MethodPost, "/api/a2a/send", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	s.handleA2ASend(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 (body=%s)", w.Code, w.Body.String())
	}
}
