package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cc-webui/internal/claudehist"
	"cc-webui/internal/config"
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
	for _, want := range []string{
		"1. 归属", "4. 状态", "mainAgent", "Worker 规则",
		"board_create", "board_comment", "board_bind_worker", "send_message",
		"新建 / 进行中 / 待人工 / 待验收 / 已完成 / 已取消",
	} {
		if !strings.Contains(body.Prompt, want) {
			t.Errorf("default prompt lost %q", want)
		}
	}
	// The default prompt must be self-contained: no machine-specific paths or
	// removed tooling from the reading workspace it was once copied from.
	for _, bad := range []string{"/home/soar", "routing-table", "tracking_ledger", "iql", "ducc"} {
		if strings.Contains(body.Prompt, bad) {
			t.Errorf("default prompt still references %q", bad)
		}
	}
	if len(body.Prompt) > maxRolePromptBytes {
		t.Fatalf("default prompt is %d bytes, over the cap", len(body.Prompt))
	}
}

// TestMainAgentPromptFromConfig: the worker defaults come from
// main_agent.worker, and main_agent.prompt replaces the whole template.
func TestMainAgentPromptFromConfig(t *testing.T) {
	dir := t.TempDir()
	yes := true
	cfg := &config.Config{
		DataDir: dir,
		MainAgent: &config.MainAgentConfig{
			Worker: config.WorkerDefaults{
				CLI: "claude", Provider: "sensenova", Model: "deepseek-v4-flash",
				Effort: "max", Resident: &yes,
			},
		},
	}
	s := &Server{cm: config.NewManager(filepath.Join(dir, "config.yaml"), cfg)}
	if p := s.mainAgentPrompt(); !strings.Contains(p, "claude / sensenova / deepseek-v4-flash / max / 常驻") {
		t.Fatalf("worker defaults not interpolated:\n%s", p)
	}

	cfg.MainAgent.Prompt = "CUSTOM PROMPT"
	s2 := &Server{cm: config.NewManager(filepath.Join(dir, "config.yaml"), cfg)}
	if got := s2.mainAgentPrompt(); got != "CUSTOM PROMPT" {
		t.Fatalf("override = %q", got)
	}
}

// getMainAgentWorkerView calls the Settings read endpoint.
func getMainAgentWorkerView(t *testing.T, s *Server) mainAgentWorkerView {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/main-agent/worker", nil)
	w := httptest.NewRecorder()
	s.handleMainAgentWorker(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get worker settings: code = %d, body=%s", w.Code, w.Body.String())
	}
	var view mainAgentWorkerView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode worker settings: %v", err)
	}
	return view
}

// putMainAgentWorker calls the Settings write endpoint.
func putMainAgentWorker(t *testing.T, s *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/main-agent/worker", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	s.handleSaveMainAgentWorker(w, req)
	return w
}

// TestMainAgentWorkerSettingsOverride pins the runtime-setting precedence:
// the Settings page wins over config.yaml, field by field, and the built-in
// prompt interpolates the merged profile.
func TestMainAgentWorkerSettingsOverride(t *testing.T) {
	s, _ := createResidentServer(t)
	no := false
	s.cm.Get().MainAgent = &config.MainAgentConfig{Worker: config.WorkerDefaults{
		CLI: "claude", Provider: "gw", Model: "m1", Effort: "low", Resident: &no,
	}}

	view := getMainAgentWorkerView(t, s)
	if view.Worker.CLI != "claude" || view.Worker.Provider != "gw" ||
		view.Worker.Model != "m1" || view.Worker.Effort != "low" ||
		view.Worker.Resident == nil || *view.Worker.Resident {
		t.Fatalf("initial effective worker = %+v", view.Worker)
	}
	if view.WorkerLine != "claude / gw / m1 / low / 非常驻" {
		t.Fatalf("initial worker_line = %q", view.WorkerLine)
	}
	if view.Sources["effort"] != "config" || view.Sources["resident"] != "config" {
		t.Fatalf("initial sources = %+v", view.Sources)
	}
	if view.PromptCustomized {
		t.Fatal("prompt_customized = true without main_agent.prompt")
	}
	if p := s.mainAgentPrompt(); !strings.Contains(p, "默认 claude / gw / m1 / low / 非常驻") {
		t.Fatalf("prompt did not interpolate config worker defaults:\n%s", p)
	}

	yes := true
	w := putMainAgentWorker(t, s, map[string]any{
		"cli": "claude", "provider": "gw", "model": "m1", "effort": "high", "resident": yes,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("save: code = %d, body=%s", w.Code, w.Body.String())
	}
	view = getMainAgentWorkerView(t, s)
	if view.Worker.Effort != "high" || view.Worker.Resident == nil || !*view.Worker.Resident {
		t.Fatalf("saved effective worker = %+v", view.Worker)
	}
	if view.Sources["effort"] != "setting" || view.Sources["resident"] != "setting" {
		t.Fatalf("saved sources = %+v", view.Sources)
	}
	if view.WorkerLine != "claude / gw / m1 / high / 常驻" {
		t.Fatalf("saved worker_line = %q", view.WorkerLine)
	}
	if p := s.mainAgentPrompt(); !strings.Contains(p, "默认 claude / gw / m1 / high / 常驻") {
		t.Fatalf("prompt did not interpolate the runtime override:\n%s", p)
	}
}

// TestMainAgentWorkerSettingsInheritAndClear: empty override fields inherit
// config.yaml, and a full empty save clears the runtime layer again.
func TestMainAgentWorkerSettingsInheritAndClear(t *testing.T) {
	s, _ := createResidentServer(t)
	yes := true
	s.cm.Get().MainAgent = &config.MainAgentConfig{Worker: config.WorkerDefaults{
		CLI: "claude", Provider: "gw", Model: "m1", Effort: "low", Resident: &yes,
	}}

	w := putMainAgentWorker(t, s, map[string]any{"effort": "max"})
	if w.Code != http.StatusOK {
		t.Fatalf("save effort only: code = %d, body=%s", w.Code, w.Body.String())
	}
	view := getMainAgentWorkerView(t, s)
	if view.Worker.CLI != "claude" || view.Worker.Provider != "gw" || view.Worker.Model != "m1" {
		t.Fatalf("inherited fields = %+v", view.Worker)
	}
	if view.Worker.Effort != "max" || view.Sources["effort"] != "setting" {
		t.Fatalf("effort override = %q (%s)", view.Worker.Effort, view.Sources["effort"])
	}
	if view.Worker.Resident == nil || !*view.Worker.Resident || view.Sources["resident"] != "config" {
		t.Fatalf("resident did not inherit config: %+v / %s", view.Worker.Resident, view.Sources["resident"])
	}
	if view.WorkerLine != "claude / gw / m1 / max / 常驻" {
		t.Fatalf("worker_line = %q", view.WorkerLine)
	}

	// A null resident (JSON null) and empty strings clear every override.
	w = putMainAgentWorker(t, s, map[string]any{
		"cli": "", "provider": "", "model": "", "effort": "", "resident": nil,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("clear: code = %d, body=%s", w.Code, w.Body.String())
	}
	view = getMainAgentWorkerView(t, s)
	if view.Overrides.CLI != "" || view.Overrides.Provider != "" ||
		view.Overrides.Model != "" || view.Overrides.Effort != "" || view.Overrides.Resident != nil {
		t.Fatalf("overrides not cleared: %+v", view.Overrides)
	}
	if view.Worker.Effort != "low" || view.Sources["effort"] != "config" {
		t.Fatalf("effort did not fall back to config: %q (%s)", view.Worker.Effort, view.Sources["effort"])
	}
}

// TestMainAgentWorkerSettingsPersists: the override is written under the data
// dir and a fresh Server (same config manager) reads it back — the Settings
// page is server-side, not localStorage.
func TestMainAgentWorkerSettingsPersists(t *testing.T) {
	s, _ := createResidentServer(t)
	w := putMainAgentWorker(t, s, map[string]any{
		"cli": "claude", "provider": "gw", "model": "m1", "effort": "high",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("save: code = %d, body=%s", w.Code, w.Body.String())
	}
	path := filepath.Join(s.cfg().DataDir, filepath.FromSlash(mainAgentWorkerFile))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("override file not written: %v", err)
	}

	s2 := &Server{cm: s.cm, st: s.st, mgr: s.mgr}
	view := getMainAgentWorkerView(t, s2)
	if view.Overrides.CLI != "claude" || view.Overrides.Provider != "gw" ||
		view.Overrides.Model != "m1" || view.Overrides.Effort != "high" {
		t.Fatalf("reloaded overrides = %+v", view.Overrides)
	}
	if p := s2.mainAgentPrompt(); !strings.Contains(p, "默认 claude / gw / m1 / high") {
		t.Fatalf("reloaded prompt lost the override:\n%s", p)
	}
}

// TestMainAgentWorkerSettingsValidation: invalid values are rejected with a
// message and never persisted.
func TestMainAgentWorkerSettingsValidation(t *testing.T) {
	s, _ := createResidentServer(t)
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"unknown cli", map[string]any{"cli": "nope"}, "未知 cli"},
		{"unknown provider", map[string]any{"provider": "nope"}, "未知 provider"},
		{"engine mismatch", map[string]any{"cli": "claude", "provider": "oc-p"}, "引擎不匹配"},
		{"model without provider", map[string]any{"cli": "claude", "model": "m1"}, "先选择 provider"},
		{"unknown model", map[string]any{"cli": "claude", "provider": "gw", "model": "nope"}, "未知模型"},
		{"invalid effort", map[string]any{"cli": "claude", "provider": "gw", "model": "m1", "effort": "ultra"}, "invalid effort"},
		{"resident non-claude", map[string]any{"cli": "oc", "provider": "oc-p", "resident": true}, "只有 claude"},
		{"resident without cli", map[string]any{"resident": true}, "先选择 claude cli"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := putMainAgentWorker(t, s, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400 (body=%s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("error = %s, want %q", w.Body.String(), tc.want)
			}
			view := getMainAgentWorkerView(t, s)
			if view.Overrides.CLI != "" || view.Overrides.Provider != "" ||
				view.Overrides.Model != "" || view.Overrides.Effort != "" || view.Overrides.Resident != nil {
				t.Fatalf("invalid save persisted: %+v", view.Overrides)
			}
		})
	}
}

// TestMainAgentWorkerRoutes pins the authenticated routes the Settings page
// uses (a handler-only test would not catch a missing mux registration).
func TestMainAgentWorkerRoutes(t *testing.T) {
	s, _ := createResidentServer(t)
	s.mux = http.NewServeMux()
	s.routes()

	req := httptest.NewRequest(http.MethodGet, "/api/main-agent/worker", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET route: code = %d, body=%s", w.Code, w.Body.String())
	}

	raw, _ := json.Marshal(map[string]any{"effort": "high"})
	req = httptest.NewRequest(http.MethodPut, "/api/main-agent/worker", bytes.NewReader(raw))
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT route: code = %d, body=%s", w.Code, w.Body.String())
	}
	var view mainAgentWorkerView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Overrides.Effort != "high" {
		t.Fatalf("overrides = %+v", view.Overrides)
	}
}

// TestMainAgentWorkerViewCustomPrompt: a custom main_agent.prompt wins over
// the whole template, and the Settings view tells the UI so it can warn.
func TestMainAgentWorkerViewCustomPrompt(t *testing.T) {
	s, _ := createResidentServer(t)
	s.cm.Get().MainAgent = &config.MainAgentConfig{Prompt: "CUSTOM"}
	view := getMainAgentWorkerView(t, s)
	if !view.PromptCustomized {
		t.Fatal("prompt_customized = false with main_agent.prompt set")
	}
	if got := s.mainAgentPrompt(); got != "CUSTOM" {
		t.Fatalf("prompt = %q", got)
	}
}
