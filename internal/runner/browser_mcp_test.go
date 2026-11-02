package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cc-webui/internal/config"
	"cc-webui/internal/store"
)

func browserTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	cm := config.NewManager("", &config.Config{
		Listen:       "127.0.0.1:8321",
		GatewayToken: "test-token",
		DataDir:      dir,
		Browser:      config.BrowserConfig{ProfileKey: "me"},
	})
	return NewManager(cm, nil, func() bool { return true })
}

func TestBrowserMCPInfoAndClaudeConfig(t *testing.T) {
	m := browserTestManager(t)
	info, ok := m.browserMCPInfo("s1")
	if !ok || info.Command == "" || info.Env["CC_WEBUI_BROWSER_SESSION"] != "s1" {
		t.Fatalf("info = %#v, ok=%v", info, ok)
	}
	args := m.appendBrowserMCP([]string{"--x"}, "s1")
	if len(args) != 3 || args[0] != "--x" || args[1] != "--mcp-config" {
		t.Fatalf("args = %#v", args)
	}
	raw, err := os.ReadFile(args[2])
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["mcpServers"].(map[string]any)["browser"]; !ok {
		t.Fatalf("config = %s", raw)
	}
	infoFile, _ := os.Stat(args[2])
	if infoFile.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", infoFile.Mode().Perm())
	}
}

func TestBrowserMCPCodexArgs(t *testing.T) {
	m := browserTestManager(t)
	args, ok := m.appendBrowserMCPCodex([]string{"exec"}, "s1")
	if !ok {
		t.Fatal("codex browser args missing")
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"mcp_servers.browser.command=",
		"mcp_servers.browser.args=[]",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "test-token") {
		t.Fatalf("codex argv leaked token: %s", joined)
	}
}

func TestOpencodeBrowserMCPConfig(t *testing.T) {
	m := browserTestManager(t)
	dir := m.ensureOpencodeRuleFor("s1")
	if dir == "" {
		t.Fatal("no opencode config dir")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCP struct {
			Servers map[string]struct {
				Type        string            `json:"type"`
				Command     []string          `json:"command"`
				Environment map[string]string `json:"environment"`
			} `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	browser := cfg.MCP.Servers["browser"]
	if browser.Type != "local" || len(browser.Command) < 2 || browser.Command[1] != "-browser-mcp" {
		t.Fatalf("browser config = %#v (%s)", browser, raw)
	}
	if browser.Environment["CC_WEBUI_BROWSER_SESSION"] != "s1" {
		t.Fatalf("env = %#v", browser.Environment)
	}
	info, _ := os.Stat(filepath.Join(dir, "opencode.json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestBuildCodexArgsAddsBrowserMCP(t *testing.T) {
	m := browserTestManager(t)
	sess := &store.Session{ID: "s1", CLI: "ducx", Provider: "codex", PermissionMode: "bypassPermissions"}
	cli := &config.CLI{Name: "ducx", Kind: config.KindCodex, Path: "ducx", ModelProvider: "oneapi", PermissionMode: "bypassPermissions"}
	prov := &config.Provider{Name: "codex", Kind: config.KindCodex}
	args := m.buildCodexArgs(Message{}, sess, cli, prov)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "mcp_servers.browser.command=") || !strings.Contains(joined, "model_provider=oneapi") {
		t.Fatalf("browser/model provider not injected: %s", joined)
	}
	if strings.Contains(joined, "test-token") {
		t.Fatalf("token leaked into argv: %s", joined)
	}
}

func TestServeKeyIncludesBrowserSession(t *testing.T) {
	cli := &config.CLI{Name: "oc", Path: "oc", Kind: config.KindOpenCode}
	prov := &config.Provider{Name: "p", Kind: config.KindOpenCode}
	a := serveKey(cli, prov, "m")
	b := serveKey(cli, prov, "m", "s1")
	if a == b || !strings.Contains(b, "browser=s1") {
		t.Fatalf("browser session not in key: %q vs %q", a, b)
	}
}
