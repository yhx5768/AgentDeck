package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"cc-webui/internal/config"
)

func TestPermissionHookWanted(t *testing.T) {
	claude := &config.CLI{Name: "c", Kind: "", Path: "claude"} // empty kind = claude
	qoder := &config.CLI{Name: "q", Kind: config.KindQoder, Path: "qoderclicn"}
	opencode := &config.CLI{Name: "o", Kind: config.KindOpenCode, Path: "opencode"}
	mcode := &config.CLI{Name: "m", Kind: config.KindMcode, Path: "mcode"}

	cases := []struct {
		name string
		cli  *config.CLI
		perm string
		want bool
	}{
		{"claude default prompts", claude, "default", true},
		{"claude empty prompts", claude, "", true},
		{"claude plan prompts", claude, "plan", true},
		{"claude auto prompts", claude, "auto", true},
		{"claude bypass skips", claude, "bypassPermissions", false},
		{"claude dontAsk skips", claude, "dontAsk", false},
		{"qoder default prompts", qoder, "default", true},
		{"qoder bypass skips", qoder, "bypassPermissions", false},
		{"opencode never hooks", opencode, "default", false},
		{"opencode bypass never hooks", opencode, "bypassPermissions", false},
		{"mcode never hooks", mcode, "default", false},
		{"nil cli never hooks", nil, "default", false},
	}
	for _, tc := range cases {
		if got := permissionHookWanted(tc.cli, tc.perm); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSettingsFileExHook(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(config.NewManager(filepath.Join(dir, "config.yaml"), &config.Config{DataDir: dir}), nil)
	plain, err := m.settingsFileEx("cl", "pr", "mo", "", map[string]string{"A": "1"}, false)
	if err != nil {
		t.Fatalf("plain settings: %v", err)
	}
	raw, _ := os.ReadFile(plain)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("plain json: %v", err)
	}
	if _, ok := doc["hooks"]; ok {
		t.Errorf("plain settings should not carry hooks")
	}

	hooked, err := m.settingsFileEx("cl", "pr", "mo", "", map[string]string{"A": "1"}, true)
	if err != nil {
		t.Fatalf("hooked settings: %v", err)
	}
	if hooked == plain {
		t.Errorf("hook file must not overwrite the plain file: %s", hooked)
	}
	raw, _ = os.ReadFile(hooked)
	doc = nil
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("hooked json: %v", err)
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooked settings missing hooks: %s", raw)
	}
	pr, ok := hooks["PermissionRequest"].([]any)
	if !ok || len(pr) != 1 {
		t.Fatalf("PermissionRequest hook missing: %s", raw)
	}
	// File stays 0600 (carries endpoint tokens).
	if fi, _ := os.Stat(hooked); fi.Mode().Perm() != 0o600 {
		t.Errorf("hook file mode = %o, want 600", fi.Mode().Perm())
	}
	_ = filepath.Base(hooked) // silence unused in some toolchains
}

func TestParseHookTool(t *testing.T) {
	tool, input, useID := parseHookTool([]byte(`{"tool_name":"Bash","tool_input":{"command":"ls"},"tool_use_id":"u1"}`))
	if tool != "Bash" || input["command"] != "ls" || useID != "u1" {
		t.Errorf("got %q %v %q", tool, input, useID)
	}
	tool, _, _ = parseHookTool([]byte(`{"tool":"Read","input":{"file_path":"/x"}}`))
	if tool != "Read" {
		t.Errorf("alt keys: got %q", tool)
	}
	tool, _, _ = parseHookTool([]byte(`not json`))
	if tool != "tool" {
		t.Errorf("bad json: got %q", tool)
	}
}

func TestResolvePendingAllowance(t *testing.T) {
	m := NewManager(config.NewManager(filepath.Join(t.TempDir(), "config.yaml"), &config.Config{DataDir: t.TempDir()}), nil)
	if m.permHookAllowed("s1", "Bash") {
		t.Fatalf("no allowance yet")
	}
	pr := m.createPending("s1", "Bash", nil, "", "p1")
	_ = pr
	if !m.resolvePending("p1", PermissionDecision{Behavior: "allow_tool_session"}) {
		t.Fatalf("resolve failed")
	}
	if !m.permHookAllowed("s1", "Bash") {
		t.Fatalf("allowance not recorded")
	}
	if m.resolvePending("p1", PermissionDecision{Behavior: "allow"}) {
		t.Fatalf("double resolve should fail")
	}
}

func TestResolvePendingAnswers(t *testing.T) {
	m := NewManager(config.NewManager(filepath.Join(t.TempDir(), "config.yaml"), &config.Config{DataDir: t.TempDir()}), nil)
	ans := map[string]string{"部署到哪里？": "用现有方案"}
	m.createPending("s9", "AskUserQuestion", map[string]any{"questions": "q"}, "", "pa1")
	ch := m.PermissionWaitChannel("pa1")
	if ch == nil {
		t.Fatalf("wait channel missing")
	}
	if !m.resolvePending("pa1", PermissionDecision{Behavior: "allow", Answers: ans}) {
		t.Fatalf("resolve failed")
	}
	select {
	case d := <-ch:
		if d.Answers["部署到哪里？"] != "用现有方案" {
			t.Fatalf("answers lost: %v", d.Answers)
		}
	default:
		t.Fatalf("decision not delivered")
	}
	// AskUserQuestion must never gain a session allowance, even on crafted input.
	m.createPending("s9", "AskUserQuestion", nil, "", "pa3")
	if !m.resolvePending("pa3", PermissionDecision{Behavior: "allow_tool_session", Answers: ans}) {
		t.Fatalf("resolve failed")
	}
	if m.permHookAllowed("s9", "AskUserQuestion") {
		t.Fatalf("AskUserQuestion must not gain an allowance")
	}
}
