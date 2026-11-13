package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cc-webui/internal/config"
	"cc-webui/internal/store"
)

func mainAgentSessionsOf(st *store.Store) []*store.Session {
	var out []*store.Session
	for _, sess := range st.List() {
		if sess.MainAgent {
			out = append(out, sess)
		}
	}
	return out
}

func TestEnsureMainAgentDefaultOff(t *testing.T) {
	s, st := createResidentServer(t)
	cfg := s.cm.Get()
	cfg.DefaultCwd = t.TempDir()

	s.EnsureMainAgent()

	if mains := mainAgentSessionsOf(st); len(mains) != 0 {
		t.Fatalf("default autocreate created %d main agent(s), want 0", len(mains))
	}
}

func TestEnsureMainAgentCreatesDefault(t *testing.T) {
	s, st := createResidentServer(t)
	cfg := s.cm.Get()
	cfg.DefaultCwd = t.TempDir()
	on := true
	cfg.MainAgent = &config.MainAgentConfig{Autocreate: &on}

	s.EnsureMainAgent()

	mains := mainAgentSessionsOf(st)
	if len(mains) != 1 {
		t.Fatalf("main agents = %d, want 1", len(mains))
	}
	sess := mains[0]
	if sess.Name != "mainAgent" || sess.PeerName != "mainAgent" {
		t.Fatalf("name/peer = %q/%q", sess.Name, sess.PeerName)
	}
	if sess.CLI != "claude" || sess.Provider != "gw" {
		t.Fatalf("default cli/provider = %q/%q, want claude/gw", sess.CLI, sess.Provider)
	}
	if !sess.Resident {
		t.Fatal("default claude main agent should be resident")
	}
	if sess.Cwd != cfg.DefaultCwd {
		t.Fatalf("cwd = %q, want %q", sess.Cwd, cfg.DefaultCwd)
	}
	if sess.RolePrompt == "" {
		t.Fatal("main agent role prompt is empty")
	}
	if !sess.CreatedAt.After(time.Time{}) || !sess.UpdatedAt.After(time.Time{}) {
		t.Fatal("timestamps not set")
	}
}

func TestEnsureMainAgentSettingOverride(t *testing.T) {
	s, st := createResidentServer(t)
	cfg := s.cm.Get()
	cfg.DefaultCwd = t.TempDir()
	if s.mainAgentAutocreateEnabled() {
		t.Fatal("default setting should be off")
	}
	if err := s.mainAgentAutocreate().save(true); err != nil {
		t.Fatal(err)
	}
	if !s.mainAgentAutocreateEnabled() {
		t.Fatal("runtime override should enable")
	}

	s.EnsureMainAgent()

	if mains := mainAgentSessionsOf(st); len(mains) != 1 {
		t.Fatalf("setting override created %d main agent(s), want 1", len(mains))
	}
	if v := s.mainAgentAutocreateView(); !v.Enabled || v.Source != "setting" {
		t.Fatalf("view = %+v", v)
	}
}

func TestEnsureMainAgentUsesConfiguredWorker(t *testing.T) {
	s, st := createResidentServer(t)
	cfg := s.cm.Get()
	cfg.DefaultCwd = t.TempDir()
	on := true
	cfg.MainAgent = &config.MainAgentConfig{Autocreate: &on, Worker: config.WorkerDefaults{
		CLI: "oc", Provider: "oc-p", Model: "kimi-k3", Effort: "high",
	}}

	s.EnsureMainAgent()

	mains := mainAgentSessionsOf(st)
	if len(mains) != 1 {
		t.Fatalf("main agents = %d, want 1", len(mains))
	}
	sess := mains[0]
	if sess.CLI != "oc" || sess.Provider != "oc-p" || sess.ModelOverride != "kimi-k3" || sess.EffortOverride != "high" {
		t.Fatalf("configured profile not applied: %+v", sess)
	}
	if sess.Resident {
		t.Fatal("non-claude main agent must not be resident")
	}
	if !strings.Contains(sess.RolePrompt, "oc / oc-p / kimi-k3 / high") {
		t.Fatalf("role prompt does not carry worker defaults: %q", sess.RolePrompt)
	}
}

func TestEnsureMainAgentKeepsExisting(t *testing.T) {
	s, st := createResidentServer(t)
	now := time.Now()
	existing := &store.Session{
		ID: "existing-main", Name: "keep me", MainAgent: true,
		CLI: "claude", Provider: "gw", Cwd: t.TempDir(),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.Upsert(existing, nil); err != nil {
		t.Fatal(err)
	}
	cfg := s.cm.Get()
	cfg.DefaultCwd = t.TempDir()
	on := true
	cfg.MainAgent = &config.MainAgentConfig{Autocreate: &on}

	s.EnsureMainAgent()

	mains := mainAgentSessionsOf(st)
	if len(mains) != 1 || mains[0].ID != existing.ID || mains[0].Name != existing.Name {
		t.Fatalf("existing main agent was changed: %+v", mains)
	}
}

func TestEnsureMainAgentDisabled(t *testing.T) {
	s, st := createResidentServer(t)
	off := false
	cfg := s.cm.Get()
	cfg.DefaultCwd = t.TempDir()
	cfg.MainAgent = &config.MainAgentConfig{Autocreate: &off}

	s.EnsureMainAgent()

	if mains := mainAgentSessionsOf(st); len(mains) != 0 {
		t.Fatalf("autocreate=false created %d main agent(s)", len(mains))
	}
}

func TestMainAgentAutocreateSettingsAPI(t *testing.T) {
	s, _ := createResidentServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/main-agent/autocreate", nil)
	w := httptest.NewRecorder()
	s.handleMainAgentAutocreate(w, req)
	var got mainAgentAutocreateView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.Source != "default" {
		t.Fatalf("default view = %+v", got)
	}

	req = httptest.NewRequest(http.MethodPut, "/api/main-agent/autocreate", strings.NewReader(`{"enabled":true}`))
	w = httptest.NewRecorder()
	s.handleSaveMainAgentAutocreate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("save: code = %d, body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Source != "setting" {
		t.Fatalf("saved view = %+v", got)
	}
	if !s.mainAgentAutocreateEnabled() {
		t.Fatal("saved override not effective")
	}
}

func TestHandleConfigDefaultUIMode(t *testing.T) {
	s, _ := createResidentServer(t)
	s.cm.Get().MainAgent = &config.MainAgentConfig{DefaultUI: "chat"}

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	w := httptest.NewRecorder()
	s.handleConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["default_ui_mode"] != "chat" {
		t.Fatalf("default_ui_mode = %v", body["default_ui_mode"])
	}
}
