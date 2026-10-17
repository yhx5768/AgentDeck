package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ruleHome points HOME at a temp dir seeded with a fake ~/.config/opencode —
// config plus the user's own AGENTS.md — so every test can assert cc-webui
// never reaches into it.
func ruleHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	userDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "opencode.jsonc"),
		[]byte("{\n  \"provider\": {\"x\": {}}\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "AGENTS.md"),
		[]byte("# 我自己的全局规则\n\n别动我。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// TestEnsureOpencodeRuleIsolated is the point of the whole design. The hint must
// land in cc-webui's own config dir and reach the model via the env the caller
// sets — never in the user's ~/.config/opencode, which would leak into a TUI
// session they start by hand.
func TestEnsureOpencodeRuleIsolated(t *testing.T) {
	home := ruleHome(t)
	userRules := filepath.Join(home, ".config", "opencode", "AGENTS.md")
	userConfig := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	beforeRules, err := os.Stat(userRules)
	if err != nil {
		t.Fatal(err)
	}
	beforeConfig, err := os.Stat(userConfig)
	if err != nil {
		t.Fatal(err)
	}

	dir := hintManager(t, nil).ensureOpencodeRule()
	want := filepath.Join(home, ".config", "cc-webui", "opencode")
	if dir != want {
		t.Fatalf("config dir = %q, want %q", dir, want)
	}
	if filepath.Dir(userRules) == dir {
		t.Fatal("cc-webui wrote into the user's opencode config dir")
	}

	for _, p := range []string{userRules, userConfig} {
		after, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if !beforeRules.ModTime().Equal(after.ModTime()) && p == userRules {
			t.Fatal("the user's own opencode AGENTS.md was rewritten")
		}
		if p == userConfig && !beforeConfig.ModTime().Equal(after.ModTime()) {
			t.Fatal("the user's own opencode.jsonc was rewritten")
		}
	}
	if !strings.Contains(readFile(t, userRules), "别动我。") {
		t.Fatal("the user's own rules were lost")
	}

	// The hint itself must be readable, and reachable only through instructions.
	hintPath := filepath.Join(dir, "HTML_RENDER_HINT.md")
	if !strings.Contains(readFile(t, hintPath), htmlRenderHint) {
		t.Fatal("hint file does not carry htmlRenderHint")
	}
	cfg := readFile(t, filepath.Join(dir, "opencode.json"))
	if !strings.Contains(cfg, `"instructions"`) {
		t.Fatalf("config does not declare instructions:\n%s", cfg)
	}
	if !strings.Contains(cfg, hintPath) {
		t.Fatalf("instructions does not point at the hint file:\n%s", cfg)
	}
}

// TestEnsureOpencodeRuleConfigIsMinimal guards the merge semantics: opencode
// merges our config over the user's rather than replacing it, so our file must
// carry only the one key we own. Anything else we put in here would silently
// override the user's provider / mcp / theme settings for cc-webui sessions.
func TestEnsureOpencodeRuleConfigIsMinimal(t *testing.T) {
	ruleHome(t)
	dir := hintManager(t, nil).ensureOpencodeRule()
	cfg := readFile(t, filepath.Join(dir, "opencode.json"))
	for _, forbidden := range []string{`"provider"`, `"mcp"`, `"theme"`, `"model"`, `"agent"`} {
		if strings.Contains(cfg, forbidden) {
			t.Errorf("our config declares %s, which would override the user's:\n%s", forbidden, cfg)
		}
	}
}

// TestEnsureOpencodeRuleIdempotent: a recycled serve must not churn the files.
func TestEnsureOpencodeRuleIdempotent(t *testing.T) {
	ruleHome(t)
	m := hintManager(t, nil)
	dir := m.ensureOpencodeRule()
	if dir == "" {
		t.Fatal("no dir")
	}
	paths := []string{filepath.Join(dir, "opencode.json"), filepath.Join(dir, "HTML_RENDER_HINT.md")}
	before := make([]int64, len(paths))
	for i, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		before[i] = fi.ModTime().UnixNano()
	}
	if again := m.ensureOpencodeRule(); again != dir {
		t.Fatalf("dir changed between calls: %q then %q", dir, again)
	}
	for i, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.ModTime().UnixNano() != before[i] {
			t.Errorf("%s rewritten despite identical content", p)
		}
	}
}

// TestEnsureOpencodeRuleFollowsTheConstant: the hint text has one source of
// truth, so editing it must propagate. Simulated by checking the file carries
// the current constant verbatim rather than a stale copy.
func TestEnsureOpencodeRuleFollowsTheConstant(t *testing.T) {
	ruleHome(t)
	m := hintManager(t, nil)
	dir := m.ensureOpencodeRule()
	hintPath := filepath.Join(dir, "HTML_RENDER_HINT.md")
	if err := os.WriteFile(hintPath, []byte("STALE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if again := m.ensureOpencodeRule(); again != dir {
		t.Fatalf("dir changed: %q then %q", dir, again)
	}
	if got := readFile(t, hintPath); !strings.Contains(got, htmlRenderHint) {
		t.Fatalf("stale hint not refreshed:\n%s", got)
	}
}

// TestEnsureOpencodeRuleNoHome: no HOME means no predictable path. Return empty
// so the caller leaves the env alone rather than pointing opencode somewhere odd.
func TestEnsureOpencodeRuleNoHome(t *testing.T) {
	t.Setenv("HOME", "")
	if dir := hintManager(t, nil).ensureOpencodeRule(); dir != "" {
		t.Fatalf("want empty dir with HOME unset, got %q", dir)
	}
}

// TestEnsureOpencodeRuleRespectsSwitch: with the hint off we must not create
// the directory, write anything, or return a path for the env.
func TestEnsureOpencodeRuleRespectsSwitch(t *testing.T) {
	home := ruleHome(t)
	no := false
	if dir := hintManager(t, &no).ensureOpencodeRule(); dir != "" {
		t.Fatalf("opt-out returned %q", dir)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "cc-webui")); !os.IsNotExist(err) {
		t.Fatalf("opt-out still created our config dir (stat err = %v)", err)
	}
	yes := true
	if dir := hintManager(t, &yes).ensureOpencodeRule(); dir == "" {
		t.Fatal("opt-in did not return a config dir")
	}
}
