package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cc-webui/internal/config"
)

// serveKey decides which `opencode serve` processes are shared. It must be a
// function of the credentials alone: anything volatile makes every launch spawn
// a new server and orphans the old one.
func TestServeKeyIgnoresAmbientEnvironment(t *testing.T) {
	cli := &config.CLI{Name: "opencode", Path: "/usr/local/bin/opencode"}
	prov := &config.Provider{Name: "sensenova-openai"}

	// Ambient shell state that changes between launches and must NOT split the
	// pool. Each is set to a distinct value so a leak into the key is visible.
	for k, v := range map[string]string{
		"SHLVL": "7", "PWD": "/tmp/elsewhere", "OLDPWD": "/tmp",
		"_": "/usr/bin/some-tool", "STY": "9999.xterm",
		"SECURITYSESSIONID": "deadbeef", "ITERM_SESSION_ID": "w1:abc",
		"TERM_SESSION_ID": "w1:abc", "WINDOW": "3",
		"LC_TERMINAL": "iTerm2", "TERM_PROGRAM": "iTerm.app",
		"TMPDIR": "/tmp/other", "LANG": "C",
	} {
		t.Setenv(k, v)
	}
	// A real credential, which MUST split the pool.
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-secret-1")

	k1 := serveKey(cli, prov, "m1")

	// Same credentials, completely different ambient shell state.
	for k, v := range map[string]string{
		"SHLVL": "1", "PWD": "/var/folders/zz", "OLDPWD": "/",
		"_": "/opt/homebrew/bin/brew", "STY": "1.ccwebui",
		"SECURITYSESSIONID": "cafebabe", "ITERM_SESSION_ID": "w9:zzz",
		"TERM_SESSION_ID": "w9:zzz", "WINDOW": "9",
		"LC_TERMINAL": "Apple_Terminal", "TERM_PROGRAM": "vscode",
		"TMPDIR": "/tmp/third", "LANG": "en_US.UTF-8",
	} {
		t.Setenv(k, v)
	}
	k2 := serveKey(cli, prov, "m1")

	if k1 != k2 {
		t.Fatalf("serveKey changed with ambient env only:\n  %q\n  %q", k1, k2)
	}
	// The credential itself is in there (it is the point of the key).
	if !strings.Contains(k1, "sk-secret-1") {
		t.Fatalf("credential missing from key: %q", k1)
	}
	// …but the ambient noise is not.
	for _, noisy := range []string{"SHLVL", "SECURITYSESSIONID", "ITERM_SESSION_ID", "STY", "OLDPWD"} {
		if strings.Contains(k1, noisy+"=") {
			t.Errorf("ambient var %s leaked into the serve key: %q", noisy, k1)
		}
	}
}

// Different credentials must NOT share a process.
func TestServeKeySplitsOnCredential(t *testing.T) {
	cli := &config.CLI{Name: "opencode", Path: "/usr/local/bin/opencode"}
	prov := &config.Provider{Name: "p"}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-one")
	k1 := serveKey(cli, prov, "m1")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-two")
	k2 := serveKey(cli, prov, "m1")
	if k1 == k2 {
		t.Fatal("different auth tokens must produce different keys")
	}
}

// …and so must a different model, provider, cli or binary.
func TestServeKeySplitsOnIdentity(t *testing.T) {
	base := &config.CLI{Name: "opencode", Path: "/usr/local/bin/opencode"}
	prov := &config.Provider{Name: "p"}
	k := serveKey(base, prov, "m1")

	if serveKey(base, prov, "m2") == k {
		t.Error("different model must split")
	}
	if serveKey(base, &config.Provider{Name: "q"}, "m1") == k {
		t.Error("different provider must split")
	}
	if serveKey(&config.CLI{Name: "other", Path: "/usr/local/bin/opencode"}, prov, "m1") == k {
		t.Error("different cli name must split")
	}
	if serveKey(&config.CLI{Name: "opencode", Path: "/opt/other/opencode"}, prov, "m1") == k {
		t.Error("different binary must split")
	}
	if serveKey(&config.CLI{Name: "opencode", Path: "/usr/local/bin/opencode", Args: []string{"--profile", "x"}}, prov, "m1") == k {
		t.Error("different cli args must split")
	}
}

// The key is logged, so it must not carry the whole terminal environment.
func TestServeKeyIsCompact(t *testing.T) {
	cli := &config.CLI{Name: "opencode", Path: "/usr/local/bin/opencode"}
	prov := &config.Provider{Name: "p"}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-x")
	// A deliberately huge ambient environment must not inflate the key.
	t.Setenv("SOME_NOISE", strings.Repeat("z", 4096))

	key := serveKey(cli, prov, "m1")
	if strings.Contains(key, "SOME_NOISE") {
		t.Error("non-credential var leaked into the key")
	}
	if n := len(strings.Split(key, "\x00")); n > 8 {
		t.Errorf("key has %d segments, expected a compact identity", n)
	}
}

// credentialEnv must accept the shapes real providers use.
func TestCredentialEnv(t *testing.T) {
	yes := []string{
		"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY",
		"ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL",
		"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENROUTER_API_KEY",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		"DEEPSEEK_API_KEY", "MOONSHOT_BASE_URL", "ZHIPUAI_API_KEY",
		"AWS_SECRET_ACCESS_KEY", "GOOGLE_API_KEY",
	}
	for _, k := range yes {
		if !credentialEnv(k) {
			t.Errorf("credentialEnv(%q) = false, want true", k)
		}
	}
	no := []string{
		"SHLVL", "PWD", "OLDPWD", "_", "STY", "TERM", "HOME", "USER",
		"SECURITYSESSIONID", "ITERM_SESSION_ID", "LC_TERMINAL", "PATH",
		"TMPDIR", "LANG", "WINDOW", "COLORTERM", "GOPATH",
	}
	for _, k := range no {
		if credentialEnv(k) {
			t.Errorf("credentialEnv(%q) = true, want false", k)
		}
	}
}

// serveLabel must never print a credential value.
func TestServeLabelHidesCredentials(t *testing.T) {
	cli := &config.CLI{Name: "opencode", Path: "/usr/local/bin/opencode"}
	prov := &config.Provider{Name: "sensenova-openai"}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-do-not-log-me")

	label := serveLabel(serveKey(cli, prov, "sensenova-6.8-flash-lite"))
	if strings.Contains(label, "sk-do-not-log-me") {
		t.Fatalf("serveLabel leaked a credential: %q", label)
	}
	if !strings.Contains(label, "sensenova-openai") {
		t.Errorf("serveLabel should still identify the provider: %q", label)
	}
	// And it must not dump the ambient environment either.
	for _, noisy := range []string{"SECURITYSESSIONID", "ITERM_SESSION_ID", "SHLVL", "STY"} {
		if strings.Contains(label, noisy) {
			t.Errorf("serveLabel leaked ambient var %s: %q", noisy, label)
		}
	}
	if _, ok := os.LookupEnv("ANTHROPIC_AUTH_TOKEN"); !ok {
		t.Skip("credential env unexpectedly absent")
	}
}

// Orphan reaping must never signal a pid it cannot positively identify as an
// `opencode serve`: a recycled pid could otherwise be an unrelated process.
func TestIsOpenCodeServeCmd(t *testing.T) {
	yes := []string{
		"/Users/x/.opencode/bin/opencode serve --port 63470 --hostname 127.0.0.1",
		"/opt/homebrew/bin/opencode serve",
	}
	for _, c := range yes {
		if !isOpenCodeServeCmd(c) {
			t.Errorf("isOpenCodeServeCmd(%q) = false, want true", c)
		}
	}
	// A user's TUI or one-shot run must survive a cc-webui restart.
	no := []string{
		"/Users/x/.opencode/bin/opencode -s ses_abc", // TUI resume
		"/Users/x/.opencode/bin/opencode run --format json",
		"/usr/local/bin/claude --resume x",
		"/usr/local/bin/opencode --version",
		"/bin/zsh",
		"",
	}
	for _, c := range no {
		if isOpenCodeServeCmd(c) {
			t.Errorf("isOpenCodeServeCmd(%q) = true, want false", c)
		}
	}
}

// reapOrphanServes on a missing/empty file must be a no-op, never an error.
func TestReapOrphanServesToleratesMissingFile(t *testing.T) {
	reapOrphanServes(filepath.Join(t.TempDir(), "does-not-exist.pids"))
	reapOrphanServes("") // empty path: must not panic
}
