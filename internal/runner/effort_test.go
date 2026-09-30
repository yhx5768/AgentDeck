package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"cc-webui/internal/config"
)

// TestSettingsPathEffortScoped pins the file-name rule: sessions on the same
// (cli, provider, model) with different efforts must not share a settings
// file, because the file carries CLAUDE_CODE_EFFORT_LEVEL and a resident
// session keeps its copy for the whole process lifetime.
func TestSettingsPathEffortScoped(t *testing.T) {
	base := SettingsPath("/data", "claude", "sensenova", "sensenova-6.8-flash-lite", "")
	if want := "/data/settings/claude__sensenova__sensenova-6_8-flash-lite.json"; base != want {
		t.Fatalf("inherit path = %q, want %q", base, want)
	}
	high := SettingsPath("/data", "claude", "sensenova", "sensenova-6.8-flash-lite", "high")
	max := SettingsPath("/data", "claude", "sensenova", "sensenova-6.8-flash-lite", "max")
	if high != "/data/settings/claude__sensenova__sensenova-6_8-flash-lite__effort-high.json" {
		t.Fatalf("effort path = %q", high)
	}
	if high == base || high == max {
		t.Fatalf("effort levels must not collide: base=%q high=%q max=%q", base, high, max)
	}
}

// TestApplyEffortEnv pins the fix for the global-settings trap: the CLI lets
// CLAUDE_CODE_EFFORT_LEVEL beat the --effort flag, so a session's level has to
// be injected into the materialized --settings env. Empty (inherit) must leave
// the env untouched — config-provided levels (e.g. a provider env) still apply.
func TestApplyEffortEnv(t *testing.T) {
	env := map[string]string{"ANTHROPIC_MODEL": "m"}
	applyEffortEnv(env, "high")
	if env["CLAUDE_CODE_EFFORT_LEVEL"] != "high" {
		t.Fatalf("effort not injected: %v", env)
	}

	inherited := map[string]string{"CLAUDE_CODE_EFFORT_LEVEL": "max"}
	applyEffortEnv(inherited, "")
	if inherited["CLAUDE_CODE_EFFORT_LEVEL"] != "max" {
		t.Fatalf("inherit must not clobber a config-provided level: %v", inherited)
	}
}

// TestSettingsFileCarriesEffort checks the materialized JSON the CLI is
// actually handed: the session level must be in the file's env section (that
// file outranks ~/.claude/settings.json), and the caller's env map must not be
// mutated as a side effect.
func TestSettingsFileCarriesEffort(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(config.NewManager(filepath.Join(dir, "config.yaml"), &config.Config{DataDir: dir}), nil)

	env := map[string]string{"ANTHROPIC_MODEL": "m"}
	path, err := m.settingsFile("claude", "sensenova", "m", "high", env)
	if err != nil {
		t.Fatalf("settingsFile: %v", err)
	}
	if _, ok := env["CLAUDE_CODE_EFFORT_LEVEL"]; ok {
		t.Fatalf("caller env mutated: %v", env)
	}
	var got struct {
		Env                 map[string]string `json:"env"`
		CrossSessionInbound string            `json:"crossSessionInbound"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if got.Env["CLAUDE_CODE_EFFORT_LEVEL"] != "high" {
		t.Fatalf("materialized env = %v, want CLAUDE_CODE_EFFORT_LEVEL=high", got.Env)
	}
	if got.Env["ANTHROPIC_MODEL"] != "m" || got.CrossSessionInbound != "accept" {
		t.Fatalf("materialized settings lost fields: %+v", got)
	}

	// Inherit: no level injected, so a config-provided one is left alone.
	p2, err := m.settingsFile("claude", "sensenova", "m", "", env)
	if err != nil {
		t.Fatalf("settingsFile(inherit): %v", err)
	}
	raw, err = os.ReadFile(p2)
	if err != nil {
		t.Fatalf("read %s: %v", p2, err)
	}
	got.Env = nil // reuse would merge keys into the previous map
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if _, ok := got.Env["CLAUDE_CODE_EFFORT_LEVEL"]; ok {
		t.Fatalf("inherit must not inject a level: %v", got.Env)
	}
}

// TestEnsureSettingsFile covers the resume-command path: the displayed path
// must exist on disk (the CLI refuses to start otherwise), and a
// (cli, provider) pair with no env must keep producing no --settings at all —
// an env-less provider (ducc-models) gets its model from --model and its
// endpoint from the CLI itself, so the copied command has no business pointing
// at a file inside this checkout.
func TestEnsureSettingsFile(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(config.NewManager(filepath.Join(dir, "config.yaml"), &config.Config{DataDir: dir}), nil)

	p := m.EnsureSettingsFile("claude", "sensenova", "m", "high", map[string]string{"ANTHROPIC_MODEL": "m"})
	if p != SettingsPath(dir, "claude", "sensenova", "m", "high") {
		t.Fatalf("path = %q", p)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("displayed path must exist: %v", err)
	}

	if p := m.EnsureSettingsFile("ducc", "ducc-models", "deepseek-v4.1-flash[1m]", "max", nil); p != "" {
		t.Fatalf("no env must yield no --settings, got %q", p)
	}
}
