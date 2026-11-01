package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cc-webui/internal/claudehist"
	"cc-webui/internal/store"
)

// TestCreateSessionMainAgent pins the create path: the marker is stored and
// echoed in the session view (the UI's pin strip reads it from the list).
func TestCreateSessionMainAgent(t *testing.T) {
	s, st := createResidentServer(t)
	w := postCreateSession(t, s, map[string]any{
		"cli": "claude", "provider": "gw", "model": "m1", "cwd": t.TempDir(),
		"role_prompt": "rules", "main_agent": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create: code = %d, body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["main_agent"] != true {
		t.Fatalf("view main_agent = %v", body["main_agent"])
	}
	var view sessionView
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	sess := st.Get(view.Session.ID)
	if !sess.MainAgent || sess.RolePrompt != "rules" {
		t.Fatalf("stored session = %+v", sess)
	}
}

// TestPatchSessionMainAgent covers the toggle, including a patch that also
// carries a fast-path flag (unread) — the flags-only shortcut must not swallow
// the marker.
func TestPatchSessionMainAgent(t *testing.T) {
	s, st := createResidentServer(t)
	sess := createSession(t, s, st, map[string]any{"name": "main"})

	w := patchSession(t, s, sess.ID, map[string]any{"main_agent": true})
	if w.Code != http.StatusOK {
		t.Fatalf("on: code = %d, body=%s", w.Code, w.Body.String())
	}
	if !st.Get(sess.ID).MainAgent {
		t.Fatal("main_agent not stored")
	}

	w = patchSession(t, s, sess.ID, map[string]any{"unread": false, "main_agent": false})
	if w.Code != http.StatusOK {
		t.Fatalf("off+flag: code = %d, body=%s", w.Code, w.Body.String())
	}
	if st.Get(sess.ID).MainAgent {
		t.Fatal("flags-only fast path swallowed main_agent=false")
	}
}

// TestForkCopiesMainAgent: a fork is a branch of the same identity — the marker
// and role prompt ride along (unlike Goal, which is deliberately not copied).
func TestForkCopiesMainAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, st := createResidentServer(t)
	sess := createSession(t, s, st, map[string]any{
		"name": "main", "role_prompt": "routing rules", "main_agent": true,
	})
	const oldSID = "old-sid"
	hist := claudehist.HistoryPath(sess.Cwd, oldSID)
	if err := os.MkdirAll(filepath.Dir(hist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hist, []byte(`{"type":"user","sessionId":"old-sid"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert(sess, func(cur *store.Session) { cur.ClaudeSessionID = oldSID }); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sess.ID+"/fork", nil)
	req.SetPathValue("id", sess.ID)
	w := httptest.NewRecorder()
	s.handleFork(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("fork: code = %d, body=%s", w.Code, w.Body.String())
	}
	var view sessionView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	forked := st.Get(view.Session.ID)
	if forked == nil || !forked.MainAgent || forked.RolePrompt != "routing rules" {
		t.Fatalf("forked session = %+v", forked)
	}
}

// TestMainAgentPromptEndpoint pins the single-source default prompt and the
// advertised size cap the UI's editor uses.
func TestMainAgentPromptEndpoint(t *testing.T) {
	s, _ := createResidentServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/main-agent-prompt", nil)
	w := httptest.NewRecorder()
	s.handleMainAgentPrompt(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var body struct {
		Prompt   string `json:"prompt"`
		MaxBytes int    `json:"max_bytes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.MaxBytes != maxRolePromptBytes {
		t.Fatalf("max_bytes = %d, want %d", body.MaxBytes, maxRolePromptBytes)
	}
	for _, want := range []string{"1. 归属", "5. 状态", "9. 台账", "mainAgent", "Worker 规则"} {
		if !strings.Contains(body.Prompt, want) {
			t.Errorf("default prompt lost %q", want)
		}
	}
	if len(body.Prompt) > maxRolePromptBytes {
		t.Fatalf("default prompt is %d bytes, over the cap", len(body.Prompt))
	}
}
