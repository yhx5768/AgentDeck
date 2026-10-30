package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"cc-webui/internal/config"
	"cc-webui/internal/runner"
	"cc-webui/internal/store"
)

// createResidentServer wires a Server whose claude cli points at a
// non-existent binary, so enabling resident never actually spawns a process:
// spawnResident's cmd.Start() fails fast and handleCreateSession logs + still
// returns the session.
func createResidentServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		DataDir: dir,
		CLIs: []config.CLI{
			{Name: "claude", Kind: config.KindClaude, Path: filepath.Join(dir, "no-such-claude")},
			{Name: "oc", Kind: config.KindOpenCode, Path: "opencode"},
		},
		Providers: []config.Provider{
			{Name: "gw", Kind: config.KindClaude, Models: []config.Model{{Name: "m1"}}},
			{Name: "oc-p", Kind: config.KindOpenCode, Models: []config.Model{{Name: "kimi-k3"}}},
		},
	}
	cm := config.NewManager(filepath.Join(dir, "config.yaml"), cfg)
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return &Server{cm: cm, st: st, mgr: runner.NewManager(cm, st)}, st
}

func postCreateSession(t *testing.T, s *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	s.handleCreateSession(w, req)
	return w
}

func TestCreateSessionResidentClaudeOnly(t *testing.T) {
	s, _ := createResidentServer(t)

	// A non-claude engine must reject resident/peer_name outright.
	for _, body := range []map[string]any{
		{"cli": "oc", "provider": "oc-p", "cwd": t.TempDir(), "resident": true},
		{"cli": "oc", "provider": "oc-p", "cwd": t.TempDir(), "peer_name": "bob"},
	} {
		w := postCreateSession(t, s, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("non-claude resident: code = %d, want 400 (body=%s)", w.Code, w.Body.String())
		}
	}
}

func TestCreateSessionResidentClaude(t *testing.T) {
	s, st := createResidentServer(t)

	// Resident + explicit name + explicit address name. The claude binary is
	// missing, so the spawn fails — creation must still succeed (the flag is
	// persisted and retried later) and the address name must be stored.
	w := postCreateSession(t, s, map[string]any{
		"name": "小王", "cli": "claude", "provider": "gw", "model": "m1",
		"cwd": t.TempDir(), "resident": true, "peer_name": "小王",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create resident: code = %d, body=%s", w.Code, w.Body.String())
	}
	var view sessionView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal view: %v", err)
	}
	sess := st.Get(view.Session.ID)
	if sess == nil {
		t.Fatal("session not persisted")
	}
	if !sess.Resident {
		t.Fatal("resident flag not stored")
	}
	if sess.PeerName != "小王" {
		t.Fatalf("peer name = %q, want 小王", sess.PeerName)
	}
	if sess.Name != "小王" {
		t.Fatalf("name = %q, want 小王", sess.Name)
	}
}

func TestCreateSessionPeerNameWithoutResident(t *testing.T) {
	s, st := createResidentServer(t)

	// peer_name alone (resident toggled later) is allowed and stored.
	w := postCreateSession(t, s, map[string]any{
		"cli": "claude", "provider": "gw", "model": "m1",
		"cwd": t.TempDir(), "peer_name": "alice",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create: code = %d, body=%s", w.Code, w.Body.String())
	}
	var view sessionView
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	sess := st.Get(view.Session.ID)
	if sess == nil || sess.Resident || sess.PeerName != "alice" {
		t.Fatalf("unexpected session: %+v", sess)
	}
}

func TestCreateSessionRejectsBadPeerName(t *testing.T) {
	s, _ := createResidentServer(t)
	w := postCreateSession(t, s, map[string]any{
		"cli": "claude", "provider": "gw", "model": "m1",
		"cwd": t.TempDir(), "peer_name": "/bad",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad peer name: code = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
}
