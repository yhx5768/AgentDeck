package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cc-webui/internal/config"
)

// ruleHome points HOME at a temp dir and returns it, so every test operates on
// a throwaway ~/.config/opencode instead of the developer's real one.
func ruleHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func readRule(t *testing.T, home string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "AGENTS.md"))
	if err != nil {
		t.Fatalf("read rule file: %v", err)
	}
	return string(raw)
}

// TestEnsureOpencodeRuleCreates is the happy path: no file at all, so one is
// created with the hint between the markers.
func TestEnsureOpencodeRuleCreates(t *testing.T) {
	home := ruleHome(t)
	changed, err := ensureOpencodeGlobalRule("HINT BODY")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first write must report changed")
	}
	got := readRule(t, home)
	if !strings.Contains(got, "HINT BODY") {
		t.Fatalf("hint missing:\n%s", got)
	}
	if !strings.Contains(got, ocRuleBegin) || !strings.Contains(got, ocRuleEnd) {
		t.Fatalf("markers missing:\n%s", got)
	}
}

// TestEnsureOpencodeRuleIdempotent is the one that keeps us from being a
// nuisance: a second call with the same text must not rewrite the file, or
// every opencode turn would bump the user's mtime for no reason.
func TestEnsureOpencodeRuleIdempotent(t *testing.T) {
	home := ruleHome(t)
	if _, err := ensureOpencodeGlobalRule("HINT BODY"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "opencode", "AGENTS.md")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := ensureOpencodeGlobalRule("HINT BODY")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("second identical write reported changed")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("file was rewritten despite identical content")
	}
}

// TestEnsureOpencodeRuleUpdatesBlock pins that cc-webui owns the text *between*
// the markers and follows the constant, while the user's own rules above and
// below stay byte-identical.
func TestEnsureOpencodeRuleUpdatesBlock(t *testing.T) {
	home := ruleHome(t)
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "AGENTS.md")
	original := "# 我的个人规则\n\n永远用中文回答。\n"
	stale := original + "\n" + ocRuleBegin + "\nOLD HINT\n" + ocRuleEnd + "\n\n## 尾部备注\n\n不要动我。\n"
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ensureOpencodeGlobalRule("NEW HINT")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("stale hint should report changed")
	}
	got := readRule(t, home)
	if !strings.Contains(got, "NEW HINT") || strings.Contains(got, "OLD HINT") {
		t.Fatalf("hint not updated:\n%s", got)
	}
	if !strings.Contains(got, "永远用中文回答。") {
		t.Fatalf("user's rule above the block was lost:\n%s", got)
	}
	if !strings.Contains(got, "不要动我。") {
		t.Fatalf("user's text below the block was lost:\n%s", got)
	}
	if i, j := strings.Index(got, "永远用中文回答。"), strings.Index(got, ocRuleBegin); i > j {
		t.Fatal("content above the block moved below it")
	}
}

// TestEnsureOpencodeRuleAppendsToExisting covers the common upgrade path: the
// user already has global rules, we must add ours after them, not replace.
func TestEnsureOpencodeRuleAppendsToExisting(t *testing.T) {
	home := ruleHome(t)
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(path, []byte("# 我的规则\n\n提交信息用中文。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureOpencodeGlobalRule("HINT BODY"); err != nil {
		t.Fatal(err)
	}
	got := readRule(t, home)
	if !strings.Contains(got, "提交信息用中文。") {
		t.Fatalf("existing rules lost:\n%s", got)
	}
	if strings.Index(got, "提交信息用中文。") > strings.Index(got, "HINT BODY") {
		t.Fatal("our hint should come after the user's existing rules")
	}
}

// TestEnsureOpencodeRuleAdoptsClaudeMD is the subtle one. opencode's global
// rules are first-match-wins: once ~/.config/opencode/AGENTS.md exists,
// ~/.claude/CLAUDE.md is silently ignored. Creating the file without carrying
// that content over would delete the user's global rules as a side effect of a
// cosmetic feature.
func TestEnsureOpencodeRuleAdoptsClaudeMD(t *testing.T) {
	home := ruleHome(t)
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	claudeMD := "# Claude 全局规则\n\n提交前必须跑测试。\n"
	if err := os.WriteFile(filepath.Join(claudeDir, "CLAUDE.md"), []byte(claudeMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureOpencodeGlobalRule("HINT BODY"); err != nil {
		t.Fatal(err)
	}
	got := readRule(t, home)
	if !strings.Contains(got, "提交前必须跑测试。") {
		t.Fatalf("CLAUDE.md content was not carried over:\n%s", got)
	}
	if !strings.Contains(got, "CLAUDE.md") {
		t.Fatalf("no note explaining where the content came from:\n%s", got)
	}
	if strings.Index(got, "提交前必须跑测试。") > strings.Index(got, "HINT BODY") {
		t.Fatal("adopted CLAUDE.md content should precede the hint block")
	}
}

// TestEnsureOpencodeRuleDoesNotAdoptTwice: once the rule file exists, opencode
// was already preferring it over CLAUDE.md. Re-importing on every later call
// would duplicate the user's rules forever.
func TestEnsureOpencodeRuleDoesNotAdoptTwice(t *testing.T) {
	home := ruleHome(t)
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "CLAUDE.md"),
		[]byte("# Claude 全局规则\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureOpencodeGlobalRule("HINT BODY"); err != nil {
		t.Fatal(err)
	}
	// Change the hint so the second call takes the update path, not the
	// adopt path — that is what would reintroduce a duplicate.
	if _, err := ensureOpencodeGlobalRule("HINT BODY v2"); err != nil {
		t.Fatal(err)
	}
	got := readRule(t, home)
	if n := strings.Count(got, "# Claude 全局规则"); n != 1 {
		t.Fatalf("CLAUDE.md content imported %d times, want 1:\n%s", n, got)
	}
}

// TestEnsureOpencodeRuleNoHome: HOME unset must be a clean error, not a write
// to a relative path that would land somewhere unpredictable.
func TestEnsureOpencodeRuleNoHome(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := ensureOpencodeGlobalRule("HINT BODY"); err == nil {
		t.Fatal("want error when HOME is unset")
	}
}

// TestEnsureOpencodeRuleRespectsSwitch is the opt-out: html_render_hint=false
// must not touch the user's file at all — not create it, not update it.
func TestEnsureOpencodeRuleRespectsSwitch(t *testing.T) {
	home := ruleHome(t)
	no := false
	m := NewManager(configManagerWithHint(t, &no), nil)
	m.ensureOpencodeRule()
	if _, err := os.Stat(filepath.Join(home, ".config", "opencode", "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("opt-out still wrote the rule file (stat err = %v)", err)
	}

	// And with the switch flipped back on, it writes.
	yes := true
	m2 := NewManager(configManagerWithHint(t, &yes), nil)
	m2.ensureOpencodeRule()
	if _, err := os.Stat(filepath.Join(home, ".config", "opencode", "AGENTS.md")); err != nil {
		t.Fatalf("opt-in did not write the rule file: %v", err)
	}
}

func configManagerWithHint(t *testing.T, on *bool) *config.Manager {
	t.Helper()
	cfg := &config.Config{DataDir: t.TempDir(), HTMLRenderHint: on}
	return config.NewManager(filepath.Join(cfg.DataDir, "config.yaml"), cfg)
}
