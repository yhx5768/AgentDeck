package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cc-webui/internal/config"
)

func TestSharedProfileMultiTarget(t *testing.T) {
	if DetectChrome("").Path == "" {
		t.Skip("Chrome not found")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/set":
			fmt.Fprint(w, `<html><body><div id="out">set</div><script>
				document.cookie = "shared_cookie=yes; path=/";
				localStorage.setItem("shared_ls", "yes");
			</script></body></html>`)
		case "/read":
			fmt.Fprint(w, `<html><body><div id="out"></div><script>
				document.getElementById("out").textContent =
					document.cookie + "|" + (localStorage.getItem("shared_ls") || "");
			</script></body></html>`)
		default:
			fmt.Fprint(w, `<html><body>ok</body></html>`)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	// Chrome writes profile files asynchronously during shutdown; let the
	// TempDir cleanup run after those writes settle.
	t.Cleanup(func() { time.Sleep(500 * time.Millisecond) })
	if err := os.MkdirAll(filepath.Join(dir, "browser"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "browser", "settings.json"),
		[]byte(`{"enabled":true,"shared_profile":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cm := config.NewManager("", &config.Config{Browser: config.BrowserConfig{
		MaxBrowsers: 2, MaxWindows: 4, IdleMinutes: 10, FPS: 8, Quality: 60, AllowLocal: true,
	}})
	m := NewManager(cm, dir)
	defer m.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := m.getOrCreate("s1"); err != nil {
		t.Fatalf("start s1: %v", err)
	}
	if _, err := m.getOrCreate("s2"); err != nil {
		t.Fatalf("start s2: %v", err)
	}
	if got := m.Status().Running; got != 2 {
		t.Fatalf("running = %d, want 2", got)
	}
	m.mu.Lock()
	if len(m.runtimes) != 1 {
		t.Fatalf("runtimes = %d, want 1", len(m.runtimes))
	}
	for _, rt := range m.runtimes {
		if rt.refs != 2 {
			t.Fatalf("runtime refs = %d, want 2", rt.refs)
		}
	}
	m.mu.Unlock()

	if res := m.Execute(ctx, "s1", "browser_open", map[string]any{"url": srv.URL + "/set"}); res.IsError {
		t.Fatalf("s1 open: %+v", res)
	}
	if res := m.Execute(ctx, "s2", "browser_open", map[string]any{"url": srv.URL + "/read"}); res.IsError {
		t.Fatalf("s2 open: %+v", res)
	}
	out := m.Execute(ctx, "s2", "browser_read", map[string]any{"selector": "#out"})
	if out.IsError {
		t.Fatalf("s2 read: %+v", out)
	}
	text := resultText(out)
	if !strings.Contains(text, "shared_cookie=yes") || !strings.Contains(text, "|yes") {
		t.Fatalf("s2 did not see values set by s1: %q", text)
	}

	m.CloseSession("s1")
	if st := m.SessionStatus("s2"); !st.Running {
		t.Fatalf("s2 stopped after s1 close: %+v", st)
	}
	m.mu.Lock()
	if len(m.runtimes) != 1 || len(m.sessions) != 1 {
		t.Fatalf("after s1 close: runtimes=%d sessions=%d", len(m.runtimes), len(m.sessions))
	}
	m.mu.Unlock()
	out = m.Execute(ctx, "s2", "browser_read", map[string]any{"selector": "#out"})
	if out.IsError || !strings.Contains(resultText(out), "shared_cookie=yes") {
		t.Fatalf("s2 lost shared state after s1 close: %+v", out)
	}

	m.CloseSession("s2")
	m.mu.Lock()
	runtimes := len(m.runtimes)
	sessions := len(m.sessions)
	m.mu.Unlock()
	if runtimes != 0 || sessions != 0 {
		t.Fatalf("after last close: runtimes=%d sessions=%d", runtimes, sessions)
	}
}

func TestSharedProfileTargetDestroyed(t *testing.T) {
	if DetectChrome("").Path == "" {
		t.Skip("Chrome not found")
	}
	dir := t.TempDir()
	// Chrome writes profile files asynchronously during shutdown; let the
	// TempDir cleanup run after those writes settle.
	t.Cleanup(func() { time.Sleep(500 * time.Millisecond) })
	if err := os.MkdirAll(filepath.Join(dir, "browser"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "browser", "settings.json"),
		[]byte(`{"enabled":true,"shared_profile":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cm := config.NewManager("", &config.Config{Browser: config.BrowserConfig{
		MaxBrowsers: 2, MaxWindows: 4, IdleMinutes: 10, FPS: 8, Quality: 60, AllowLocal: true,
	}})
	m := NewManager(cm, dir)
	defer m.Shutdown()

	if _, err := m.getOrCreate("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.getOrCreate("s2"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	s2 := m.sessions["s2"]
	rt := s2.runtime
	id := s2.targetID
	m.mu.Unlock()
	if err := rt.closeTarget(context.Background(), id); err != nil {
		t.Fatalf("close target: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		_, alive := m.sessions["s2"]
		refs := 0
		if rt2 := m.runtimes[rt.key]; rt2 != nil {
			refs = rt2.refs
		}
		m.mu.Unlock()
		if !alive {
			if refs != 1 {
				t.Fatalf("runtime refs after target destroy = %d, want 1", refs)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("session s2 was not removed after its target was closed")
}

func resultText(r Result) string {
	if len(r.Content) == 0 {
		return ""
	}
	return r.Content[0].Text
}
