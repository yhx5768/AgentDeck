package server

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"cc-webui/internal/config"
)

func TestServeStaticQueryTokenSetsCookie(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, GatewayToken: "preview-token"}
	s := &Server{
		cm: config.NewManager(filepath.Join(dir, "config.yaml"), cfg),
		static: fstest.MapFS{
			"index.html": {Data: []byte("<!doctype html><link rel=stylesheet href=/style.css>")},
			"style.css":  {Data: []byte("body{color:red}")},
		},
	}
	w := httptest.NewRecorder()
	s.serveStatic().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/?token=preview-token", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("index code = %d", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != cookieName || cookies[0].Value != "preview-token" {
		t.Fatalf("cookies = %+v", cookies)
	}

	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/style.css", nil)
	req.AddCookie(cookies[0])
	s.serveStatic().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("style.css code = %d", w.Code)
	}
}

func TestBoardHubBroadcast(t *testing.T) {
	h := newBoardHub()
	a := h.subscribe()
	b := h.subscribe()
	h.broadcast()
	for _, ch := range []chan struct{}{a, b} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("subscriber did not receive broadcast")
		}
	}
	h.unsubscribe(a)
	h.unsubscribe(b)
}

func TestHandleAgentBoardEventsStreams(t *testing.T) {
	dir := t.TempDir()
	hub := newBoardHub()
	s := &Server{
		cm:       config.NewManager(filepath.Join(dir, "config.yaml"), &config.Config{DataDir: dir}),
		boardHub: hub,
	}
	ts := httptest.NewServer(http.HandlerFunc(s.handleAgentBoardEvents))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	readEvent := func() string {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if strings.HasPrefix(line, "event: ") {
				return strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			}
		}
	}
	if got := readEvent(); got != "ready" {
		t.Fatalf("first event = %q", got)
	}
	hub.broadcast()
	if got := readEvent(); got != "board" {
		t.Fatalf("second event = %q", got)
	}
}
