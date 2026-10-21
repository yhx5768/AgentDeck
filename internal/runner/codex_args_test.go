package runner

import (
	"strings"
	"testing"

	"cc-webui/internal/config"
	"cc-webui/internal/store"
)

func codexTestProfile(t *testing.T) (*config.CLI, *config.Provider) {
	t.Helper()
	cli := &config.CLI{
		Name: "ducx", Kind: config.KindCodex, Path: "ducx",
		PermissionMode: "bypassPermissions", ModelProvider: "oneapi",
	}
	prov := &config.Provider{
		Name: "codex", Kind: config.KindCodex,
		Models: []config.Model{{Name: "gpt-6.1-sol"}},
	}
	return cli, prov
}

func joined(args []string) string { return strings.Join(args, " ") }

func TestBuildCodexArgsNewSession(t *testing.T) {
	cli, prov := codexTestProfile(t)
	sess := &store.Session{ModelOverride: "gpt-6.1-sol", EffortOverride: "high"}
	msg := Message{
		Text: "hi",
		Files: []UploadedFile{
			{Name: "a.png", Path: "/uploads/a.png"},
			{Name: "b.pdf", Path: "/uploads/b.pdf"},
		},
	}
	args := (&Manager{}).buildCodexArgs(msg, sess, cli, prov)
	got := joined(args)
	for _, want := range []string{
		"exec --json --skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox",
		"-m gpt-6.1-sol",
		"-c model_provider=oneapi",
		"-c model_reasoning_effort=high",
		"-i /uploads/a.png",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("args missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "-i /uploads/b.pdf") {
		t.Errorf("non-image file must not use -i: %s", got)
	}
	if strings.Contains(got, " resume ") {
		t.Errorf("new session must not resume: %s", got)
	}
	if strings.Contains(got, "--sandbox") {
		t.Errorf("bypassPermissions must not also pass a sandbox: %s", got)
	}
}

func TestBuildCodexArgsResume(t *testing.T) {
	cli, prov := codexTestProfile(t)
	cli.PermissionMode = "default"
	sess := &store.Session{
		ModelOverride: "gpt-6.1-sol", ClaudeSessionID: "thread-1",
	}
	args := (&Manager{}).buildCodexArgs(Message{Text: "hi"}, sess, cli, prov)
	got := joined(args)
	if !strings.HasPrefix(got, "exec resume --json") {
		t.Errorf("resume args = %s", got)
	}
	if !strings.HasSuffix(got, "thread-1") {
		t.Errorf("session id must be last, got %s", got)
	}
	// exec resume rejects --sandbox; the thread inherits its policy.
	if strings.Contains(got, "--sandbox") {
		t.Errorf("resume must not pass --sandbox: %s", got)
	}
}

func TestBuildCodexArgsDefaultSandbox(t *testing.T) {
	cli, prov := codexTestProfile(t)
	cli.PermissionMode = "default"
	args := (&Manager{}).buildCodexArgs(Message{Text: "hi"}, &store.Session{}, cli, prov)
	got := joined(args)
	if !strings.Contains(got, "--sandbox workspace-write") {
		t.Errorf("default mode should edit the workspace: %s", got)
	}
}

// codex has no --append-system-prompt: the hint rides on `-c instructions=`,
// which codex renders as a developer message. It must be present on resumes
// too (developer_instructions would be ignored there).
func TestBuildCodexArgsHTMLHint(t *testing.T) {
	for _, sess := range []*store.Session{
		{},
		{ClaudeSessionID: "thread-1"},
	} {
		cli, prov := codexTestProfile(t)
		args := (&Manager{}).buildCodexArgs(Message{Text: "hi"}, sess, cli, prov)
		got := joined(args)
		if !strings.Contains(got, "-c "+codexInstructionsArg()) {
			t.Errorf("resume=%v: missing instructions override: %s", sess.ClaudeSessionID != "", got)
		}
		// Our -c makes ducx stop injecting its own provider, so the configured
		// id must ride along even when there is no effort override.
		if !strings.Contains(got, "-c model_provider=oneapi") {
			t.Errorf("resume=%v: missing provider pin: %s", sess.ClaudeSessionID != "", got)
		}
	}
}

func TestBuildCodexArgsHTMLHintDisabled(t *testing.T) {
	off := false
	cli, prov := codexTestProfile(t)
	args := hintManager(t, &off).buildCodexArgs(Message{Text: "hi"}, &store.Session{}, cli, prov)
	if got := joined(args); strings.Contains(got, "instructions=") {
		t.Errorf("html_render_hint=false must not inject: %s", got)
	}
}

func TestBuildCodexArgsHTMLHintRespectsUserOverride(t *testing.T) {
	cli, prov := codexTestProfile(t)
	cli.Args = []string{"-c", `instructions="mine"`}
	args := (&Manager{}).buildCodexArgs(Message{Text: "hi"}, &store.Session{}, cli, prov)
	if n := strings.Count(joined(args), "instructions="); n != 1 {
		t.Errorf("instructions override count = %d, want only the user's: %s", n, joined(args))
	}
}

func TestHasConfigOverride(t *testing.T) {
	cases := []struct {
		args []string
		key  string
		want bool
	}{
		{[]string{"-c", "instructions=\"x\""}, "instructions", true},
		{[]string{"--config", "instructions = \"x\""}, "instructions", true},
		{[]string{"--config=instructions=\"x\""}, "instructions", true},
		{[]string{"-c=instructions=\"x\""}, "instructions", true},
		{[]string{"-c", "model_reasoning_effort=high"}, "instructions", false},
		{[]string{"-c", "fake_instructions=\"x\""}, "instructions", false},
		{[]string{"-c"}, "instructions", false},
	}
	for _, c := range cases {
		if got := hasConfigOverride(c.args, c.key); got != c.want {
			t.Errorf("hasConfigOverride(%q, %q) = %v, want %v", c.args, c.key, got, c.want)
		}
	}
}
