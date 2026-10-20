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
