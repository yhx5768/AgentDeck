package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"cc-webui/internal/config"
	"cc-webui/internal/runner"
	"cc-webui/internal/store"
)

// resumeTestServer wires a Server around a synthetic config: enough to call
// resumeCommand, no store or HTTP needed.
func resumeTestServer(t *testing.T, dir string) *Server {
	t.Helper()
	cfg := &config.Config{
		DataDir: dir,
		CLIs: []config.CLI{
			{Name: "ducc", Path: "/opt/ducc"},
			{Name: "claude", Path: "claude"},
		},
		Providers: []config.Provider{
			// Env-less backend: models are routed by the launcher itself,
			// nothing to put in a settings file (see config.example.yaml).
			{Name: "ducc-models", Models: []config.Model{{Name: "deepseek-v4.1-flash[1m]"}}},
			{Name: "sensenova",
				Env:    map[string]string{"ANTHROPIC_BASE_URL": "https://token.sensenova.cn"},
				Models: []config.Model{{Name: "sensenova-6.8-flash-lite", Env: map[string]string{"ANTHROPIC_MODEL": "sensenova-6.8-flash-lite"}}}},
		},
	}
	cm := config.NewManager(filepath.Join(dir, "config.yaml"), cfg)
	return &Server{cm: cm, mgr: runner.NewManager(cm, nil)}
}

func settingsEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var got struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return got.Env
}

// TestResumeCommandCarriesModelAndEffort pins the ducc-models fix: an env-less
// provider used to hand out a bare `ducc --resume <sid>`, losing the session's
// model and reasoning level. The model can only travel on --model — the
// launcher canonicalizes the flag into its own inner settings (ducc maps
// "Claude Sonnet 5" to "Claude Sonnet 5[1m]" plus that model's context window
// and auth, and our settings file is merged last, so an env-pinned name would
// override that with the raw config name) — and there is no --settings here:
// the CLI supplies the endpoint itself, so the command stays portable instead
// of pointing into this checkout's data dir.
func TestResumeCommandCarriesModelAndEffort(t *testing.T) {
	s := resumeTestServer(t, t.TempDir())
	sess := &store.Session{
		ID: "s1", CLI: "ducc", Provider: "ducc-models",
		ModelOverride: "deepseek-v4.1-flash[1m]", EffortOverride: "max",
		Cwd: "/w", ClaudeSessionID: "abc-123",
	}
	got := s.resumeCommand(sess)
	want := "cd '/w' && '/opt/ducc' --resume abc-123 --model 'deepseek-v4.1-flash[1m]' --effort 'max'"
	if got != want {
		t.Fatalf("resumeCommand =\n  %s\nwant\n  %s", got, want)
	}
}

// TestResumeCommandEnvLessInheritEffort: the provider's default model still
// rides along when the session pins no effort.
func TestResumeCommandEnvLessInheritEffort(t *testing.T) {
	s := resumeTestServer(t, t.TempDir())
	sess := &store.Session{ID: "s1", CLI: "ducc", Provider: "ducc-models", Cwd: "/w", ClaudeSessionID: "abc"}
	got := s.resumeCommand(sess)
	if want := "cd '/w' && '/opt/ducc' --resume abc --model 'deepseek-v4.1-flash[1m]'"; got != want {
		t.Fatalf("resumeCommand =\n  %s\nwant\n  %s", got, want)
	}
}

// TestResumeCommandEnvProviderKeepsSettings: a provider whose endpoint lives in
// config env keeps handing out its materialized settings file — without it the
// terminal would resume against whatever endpoint the shell happens to have.
func TestResumeCommandEnvProviderKeepsSettings(t *testing.T) {
	dir := t.TempDir()
	s := resumeTestServer(t, dir)
	sess := &store.Session{
		ID: "s2", CLI: "claude", Provider: "sensenova",
		ModelOverride: "sensenova-6.8-flash-lite", EffortOverride: "high",
		Cwd: "/w", ClaudeSessionID: "sid",
	}
	got := s.resumeCommand(sess)
	wantSettings := filepath.Join(dir, "settings", "claude__sensenova__sensenova-6_8-flash-lite__effort-high.json")
	want := "cd '/w' && 'claude' --resume sid" +
		" --model 'sensenova-6.8-flash-lite' --effort 'high' --settings '" + wantSettings + "'"
	if got != want {
		t.Fatalf("resumeCommand =\n  %s\nwant\n  %s", got, want)
	}
	// The file has to exist — the CLI hard-fails on a missing --settings — and
	// it is the same path the runner passes to its spawns.
	env := settingsEnv(t, wantSettings)
	if env["ANTHROPIC_MODEL"] != "sensenova-6.8-flash-lite" || env["CLAUDE_CODE_EFFORT_LEVEL"] != "high" {
		t.Fatalf("materialized env = %v", env)
	}
}

// TestResumeCommandWithoutEngineSession: no claude session id, no command.
func TestResumeCommandWithoutEngineSession(t *testing.T) {
	s := resumeTestServer(t, t.TempDir())
	if got := s.resumeCommand(&store.Session{ID: "s1", CLI: "ducc", Provider: "ducc-models", Cwd: "/w"}); got != "" {
		t.Fatalf("resumeCommand = %q, want empty", got)
	}
}
