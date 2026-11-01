package runner

import (
	"strings"
	"testing"

	"cc-webui/internal/config"
	"cc-webui/internal/store"
)

func countArg(args []string, want string) int {
	n := 0
	for _, a := range args {
		if a == want {
			n++
		}
	}
	return n
}

const testRolePrompt = "你是 cc-webui 的 worker，只做被指派的任务。"

// TestAppendSystemPromptMergesRoleAndHint pins the single-argument contract:
// the CLI accepts one --append-system-prompt, so the role prompt and the HTML
// hint must ride the same value (role first) instead of two flags. Two flags
// would make the last occurrence win and silently drop the other text.
func TestAppendSystemPromptMergesRoleAndHint(t *testing.T) {
	m := hintManager(t, nil)
	args := m.appendSystemPrompt([]string{"-p"}, &config.CLI{}, testRolePrompt)
	if n := countArg(args, "--append-system-prompt"); n != 1 {
		t.Fatalf("--append-system-prompt count = %d, want 1: %q", n, args)
	}
	v, _ := valueAfter(args, "--append-system-prompt")
	if !strings.Contains(v, testRolePrompt) {
		t.Fatalf("merged text lost the role prompt: %q", v)
	}
	if !strings.Contains(v, htmlRenderHint) {
		t.Fatalf("merged text lost the HTML hint: %q", v)
	}
	if strings.Index(v, testRolePrompt) > strings.Index(v, htmlRenderHint) {
		t.Fatalf("role prompt should come before the hint: %q", v)
	}
}

// TestAppendSystemPromptRoleOnlyWhenHintOff keeps the hint switch independent
// from the role prompt: html_render_hint=false must not disable the role.
func TestAppendSystemPromptRoleOnlyWhenHintOff(t *testing.T) {
	off := false
	m := hintManager(t, &off)
	args := m.appendSystemPrompt([]string{"-p"}, &config.CLI{}, testRolePrompt)
	v, ok := valueAfter(args, "--append-system-prompt")
	if !ok {
		t.Fatalf("role prompt was not injected with the hint off: %q", args)
	}
	if v != testRolePrompt {
		t.Fatalf("append text = %q, want only the role prompt", v)
	}
}

// TestAppendSystemPromptNothingToSay: hint off + no role prompt appends nothing
// (an empty flag value would be worse than no flag at all).
func TestAppendSystemPromptNothingToSay(t *testing.T) {
	off := false
	in := []string{"-p"}
	args := hintManager(t, &off).appendSystemPrompt(in, &config.CLI{}, "   ")
	if len(args) != len(in) {
		t.Fatalf("appended with nothing to say: %q", args)
	}
}

// TestAppendSystemPromptUserFlagWinsWithRolePrompt keeps the existing config
// override rule: a user's own --append-system-prompt in cli.Args wins wholesale,
// so our role+hint text is not injected at all.
func TestAppendSystemPromptUserFlagWinsWithRolePrompt(t *testing.T) {
	m := hintManager(t, nil)
	in := []string{"-p"}
	cli := &config.CLI{Args: []string{"--append-system-prompt=mine"}}
	got := m.appendSystemPrompt(in, cli, testRolePrompt)
	if len(got) != len(in) {
		t.Fatalf("injected despite the user's own flag: %q", got)
	}
}

// TestBuildStreamJSONArgsRolePrompt covers the non-resident claude/qoder spawn
// path: one append flag carrying both texts.
func TestBuildStreamJSONArgsRolePrompt(t *testing.T) {
	for _, d := range []streamJSONDialect{claudeDialect, qoderDialect} {
		m := hintManager(t, nil)
		sess := &store.Session{RolePrompt: testRolePrompt}
		args := m.buildStreamJSONArgs(sess, &config.CLI{Name: "claude"}, "m1", d, false)
		if n := countArg(args, "--append-system-prompt"); n != 1 {
			t.Fatalf("one-shot append count = %d, want 1: %q", n, args)
		}
		v, _ := valueAfter(args, "--append-system-prompt")
		if !strings.Contains(v, testRolePrompt) || !strings.Contains(v, htmlRenderHint) {
			t.Fatalf("one-shot append missing role or hint: %q", v)
		}
	}
}

// TestBuildResidentArgsRolePrompt covers the resident spawn path. A resident
// picks the role prompt up at process start, which is why a patch only takes
// effect on the next spawn.
func TestBuildResidentArgsRolePrompt(t *testing.T) {
	m := hintManager(t, nil)
	sess := &store.Session{RolePrompt: testRolePrompt, EffortOverride: "high"}
	args := m.buildResidentArgs(sess, &config.CLI{Name: "claude"}, "m1")
	if n := countArg(args, "--append-system-prompt"); n != 1 {
		t.Fatalf("resident append count = %d, want 1: %q", n, args)
	}
	v, _ := valueAfter(args, "--append-system-prompt")
	if !strings.Contains(v, testRolePrompt) || !strings.Contains(v, htmlRenderHint) {
		t.Fatalf("resident append missing role or hint: %q", v)
	}
}

// TestBuildMcodeArgsRolePrompt: mcode shares the append flag, so it rides the
// same merge.
func TestBuildMcodeArgsRolePrompt(t *testing.T) {
	m := hintManager(t, nil)
	sess := &store.Session{RolePrompt: testRolePrompt}
	cli := &config.CLI{Name: "mcode"}
	prov := &config.Provider{Name: "mcode-minimax", Models: []config.Model{{Name: "m"}}}
	args := m.buildMcodeArgs(Message{Text: "P"}, sess, cli, prov)
	if n := countArg(args, "--append-system-prompt"); n != 1 {
		t.Fatalf("mcode append count = %d, want 1: %q", n, args)
	}
	v, _ := valueAfter(args, "--append-system-prompt")
	if !strings.Contains(v, testRolePrompt) || !strings.Contains(v, htmlRenderHint) {
		t.Fatalf("mcode append missing role or hint: %q", v)
	}
}

// TestBuildCodexArgsRolePrompt: codex has no append flag — the role prompt and
// hint share one `-c instructions=` override, present on resume too, and the
// provider pin rides along.
func TestBuildCodexArgsRolePrompt(t *testing.T) {
	cli, prov := codexTestProfile(t)
	sess := &store.Session{RolePrompt: testRolePrompt, ClaudeSessionID: "thread-1"}
	args := (&Manager{}).buildCodexArgs(Message{Text: "hi"}, sess, cli, prov)
	got := joined(args)
	if n := strings.Count(got, "instructions="); n != 1 {
		t.Fatalf("instructions override count = %d, want 1: %s", n, got)
	}
	if !strings.Contains(got, testRolePrompt) || !strings.Contains(got, "HTML") {
		t.Fatalf("instructions lost role or hint: %s", got)
	}
	if !strings.Contains(got, "-c model_provider=oneapi") {
		t.Fatalf("missing provider pin: %s", got)
	}
}

// TestBuildCodexArgsRolePromptHintOff: disabling the HTML hint must not drop the
// role prompt (nor the provider pin that any -c requires).
func TestBuildCodexArgsRolePromptHintOff(t *testing.T) {
	off := false
	cli, prov := codexTestProfile(t)
	sess := &store.Session{RolePrompt: testRolePrompt}
	args := hintManager(t, &off).buildCodexArgs(Message{Text: "hi"}, sess, cli, prov)
	got := joined(args)
	if n := strings.Count(got, "instructions="); n != 1 {
		t.Fatalf("instructions override count = %d, want 1: %s", n, got)
	}
	if !strings.Contains(got, testRolePrompt) {
		t.Fatalf("instructions lost the role prompt: %s", got)
	}
	if strings.Contains(got, "HTML") {
		t.Fatalf("hint leaked with html_render_hint=false: %s", got)
	}
	if !strings.Contains(got, "-c model_provider=oneapi") {
		t.Fatalf("missing provider pin: %s", got)
	}
}
