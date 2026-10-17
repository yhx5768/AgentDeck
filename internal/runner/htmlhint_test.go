package runner

import (
	"path/filepath"
	"strings"
	"testing"

	"cc-webui/internal/config"
	"cc-webui/internal/store"
)

// hintManager builds a Manager whose config has html_render_hint set to the
// given tri-state: nil (unset), explicit true, explicit false.
func hintManager(t *testing.T, on *bool) *Manager {
	t.Helper()
	cfg := &config.Config{DataDir: t.TempDir(), HTMLRenderHint: on}
	return NewManager(config.NewManager(filepath.Join(cfg.DataDir, "config.yaml"), cfg), nil)
}

// valueAfter returns the token following flag in args, and whether it was found.
func valueAfter(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// TestAppendHTMLRenderHintDefaultOn pins the default: an unset
// html_render_hint means the hint IS injected. Getting this backwards would
// silently disable the feature on every existing config.yaml.
func TestAppendHTMLRenderHintDefaultOn(t *testing.T) {
	m := hintManager(t, nil)
	got := m.appendHTMLRenderHint([]string{"-p"}, &config.CLI{})
	v, ok := valueAfter(got, "--append-system-prompt")
	if !ok {
		t.Fatalf("no --append-system-prompt in %q", got)
	}
	if v != htmlRenderHint {
		t.Fatalf("injected text != htmlRenderHint")
	}
	if len(got) != 3 {
		t.Fatalf("arg count = %d (%q), want 3", len(got), got)
	}
}

// TestAppendHTMLRenderHintRespectsExplicitTrue covers the opt-in spelling.
func TestAppendHTMLRenderHintRespectsExplicitTrue(t *testing.T) {
	yes := true
	got := hintManager(t, &yes).appendHTMLRenderHint([]string{"-p"}, &config.CLI{})
	if _, ok := valueAfter(got, "--append-system-prompt"); !ok {
		t.Fatalf("explicit true did not inject: %q", got)
	}
}

// TestAppendHTMLRenderHintOptOut is the switch's whole purpose: false means the
// args come back byte-identical.
func TestAppendHTMLRenderHintOptOut(t *testing.T) {
	no := false
	in := []string{"-p", "--output-format", "stream-json"}
	got := hintManager(t, &no).appendHTMLRenderHint(in, &config.CLI{})
	if len(got) != len(in) {
		t.Fatalf("opt-out still injected: %q", got)
	}
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("opt-out mutated arg %d: %q vs %q", i, got[i], in[i])
		}
	}
}

// TestAppendHTMLRenderHintUserFlagWins pins the de-dup rule. CLIs take the last
// occurrence, so if we appended ours after a user's own --append-system-prompt
// theirs would be silently dropped. Both the space and the =value spellings
// must be detected, and cli.Args must be checked in its own right so the result
// does not depend on the call site having merged it first.
func TestAppendHTMLRenderHintUserFlagWins(t *testing.T) {
	m := hintManager(t, nil)
	for _, userFlag := range []string{"--append-system-prompt", "--append-system-prompt=mine"} {
		in := []string{"-p", userFlag, "my own prompt"}
		got := m.appendHTMLRenderHint(in, &config.CLI{})
		if len(got) != len(in) {
			t.Fatalf("%s: injected anyway: %q", userFlag, got)
		}
		v, ok := valueAfter(got, "--append-system-prompt")
		if ok && v == htmlRenderHint {
			t.Fatalf("%s: our hint was appended after the user's", userFlag)
		}
		// Same flag, but only in cli.Args and not yet merged into args.
		got2 := m.appendHTMLRenderHint([]string{"-p"}, &config.CLI{Args: []string{userFlag}})
		if len(got2) != 1 {
			t.Fatalf("%s via cli.Args: injected anyway: %q", userFlag, got2)
		}
	}
}

// TestAppendHTMLRenderHintNilManager guards the nil-config path: NewManager(nil, nil)
// appears throughout the runner tests, and a nil cm here would panic.
func TestAppendHTMLRenderHintNilManager(t *testing.T) {
	m := &Manager{}
	if _, ok := valueAfter(m.appendHTMLRenderHint([]string{"-p"}, nil), "--append-system-prompt"); !ok {
		t.Fatal("nil cm did not inject")
	}
}

// TestMcodeArgsOrdering pins runMcodeTurn's argv order. The positional prompt
// must precede `--file` (an array flag — it would eat a trailing positional and
// silently drop the user's message). The hint's position relative to cli.Args
// is deliberately NOT load-bearing: appendHTMLRenderHint inspects cli.Args
// itself precisely so the merge order can't break dedup, and flipping the two
// lines here still passes. What's left is that the hint lands before the
// positional, so it can't be mistaken for message text.
func TestMcodeArgsOrdering(t *testing.T) {
	m := hintManager(t, nil)
	sess := &store.Session{
		PermissionMode:  "bypassPermissions",
		EffortOverride:  "high",
		ClaudeSessionID: "sess-1",
	}
	cli := &config.CLI{Name: "mcode"}
	prov := &config.Provider{Name: "mcode-minimax", Models: []config.Model{{Name: "m"}}}
	args := m.buildMcodeArgs(Message{
		Text:  "PROMPT_POSITIONAL",
		Files: []UploadedFile{{Path: "/tmp/a.png"}},
	}, sess, cli, prov)

	idx := func(want string) int {
		for i, a := range args {
			if a == want {
				return i
			}
		}
		return -1
	}
	hint, pos, file := idx("--append-system-prompt"), idx("PROMPT_POSITIONAL"), idx("--file")
	if hint < 0 {
		t.Fatalf("no --append-system-prompt in %q", args)
	}
	if args[hint+1] != htmlRenderHint {
		t.Fatalf("hint text not directly after its flag")
	}
	if pos < 0 || file < 0 {
		t.Fatalf("missing positional or --file in %q", args)
	}
	if pos > file {
		t.Errorf("positional at %d must precede --file at %d: %q", pos, file, args)
	}
	if hint > pos {
		t.Errorf("hint at %d must precede the positional at %d: %q", hint, pos, args)
	}
	// cli.Args must already be merged, otherwise hasArg can't see a user's flag.
	withUser := m.buildMcodeArgs(Message{Text: "P"}, sess,
		&config.CLI{Name: "mcode", Args: []string{"--append-system-prompt=mine"}}, prov)
	if containsArg(withUser, htmlRenderHint) {
		t.Fatalf("user's own flag was not honoured: %q", withUser)
	}
}

func containsArg(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

// TestHTMLRenderHintContent pins the properties the hint must keep having for
// the renderer to work: it must name the fenced-html spelling (so the model
// emits ```html rather than a file), demand self-containment, and name the
// host CSS variables with a fallback. A rewrite that drops any of these
// silently degrades the feature in ways no test elsewhere would catch.
func TestHTMLRenderHintContent(t *testing.T) {
	for _, want := range []string{
		"三个反引号",            // the ```html fence spelling
		"html",             // the language tag
		"自包含",              // self-contained
		"--background",     // host CSS variables
		"--accent",         //
		"var(--background", // with a fallback, not bare var(...)
		"内联 SVG",           // charts must be inline SVG
		"640",              // chart sizing hint matches the frame cap
	} {
		if !strings.Contains(htmlRenderHint, want) {
			t.Errorf("hint lost required content %q", want)
		}
	}
	// A raw backtick inside the Go raw string literal would have been a compile
	// error, but the hint quotes the fence in prose — make sure it never grows
	// one that would break the literal.
	if strings.Contains(htmlRenderHint, "`") {
		t.Error("hint contains a raw backtick")
	}
}
