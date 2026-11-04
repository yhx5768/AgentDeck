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
	args := m.appendMCP([]string{"--x"}, "s1")
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
	servers, _ := cfg["mcpServers"].(map[string]any)
	if _, ok := servers["browser"]; !ok {
		t.Fatalf("browser server missing: %s", raw)
	}
	// The agentdeck server rides the same config (on by default).
	if _, ok := servers["agentdeck"]; !ok {
		t.Fatalf("agentdeck server missing: %s", raw)
	}
	infoFile, _ := os.Stat(args[2])
	if infoFile.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", infoFile.Mode().Perm())
	}
}

func TestBrowserMCPCodexArgs(t *testing.T) {
	m := browserTestManager(t)
	args, ok := m.appendMCPCodex([]string{"exec"}, "s1")
	if !ok {
		t.Fatal("codex mcp args missing")
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"mcp_servers.browser.command=",
		"mcp_servers.browser.args=[]",
		"mcp_servers.agentdeck.command=",
		"mcp_servers.agentdeck.args=[]",
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
	ad, ok := cfg.MCP.Servers["agentdeck"]
	if !ok || len(ad.Command) < 2 || ad.Command[1] != "-agentdeck-mcp" {
		t.Fatalf("agentdeck config = %#v (%s)", ad, raw)
	}
	if ad.Environment["CC_WEBUI_A2A_SESSION"] != "s1" {
		t.Fatalf("agentdeck env = %#v", ad.Environment)
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

func TestAgentdeckMCPInfo(t *testing.T) {
	m := browserTestManager(t)
	info, ok := m.agentdeckMCPInfo("s1")
	if !ok || info.Command == "" {
		t.Fatalf("info = %#v ok=%v", info, ok)
	}
	if info.Env["CC_WEBUI_A2A_SESSION"] != "s1" || info.Env["CC_WEBUI_A2A_TOKEN"] != "test-token" {
		t.Fatalf("env = %#v", info.Env)
	}
	// The shared (no-session) config omits the caller identity so send_message
	// falls back to an explicit `from`.
	base, _ := m.agentdeckMCPInfo("")
	if _, ok := base.Env["CC_WEBUI_A2A_SESSION"]; ok {
		t.Fatalf("base env leaked session: %#v", base.Env)
	}
}

func TestAgentdeckMCPDisabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	off := false
	cm := config.NewManager("", &config.Config{
		Listen: "127.0.0.1:8321", GatewayToken: "tok", DataDir: dir, AgentdeckMCP: &off,
	})
	m := NewManager(cm, nil)
	if _, ok := m.agentdeckMCPInfo("s1"); ok {
		t.Fatal("agentdeck MCP must be off when the config disables it")
	}
	if _, ok := m.mcpServersMap("s1")["agentdeck"]; ok {
		t.Fatal("mcpServersMap still injected agentdeck")
	}
}

func TestEnsureMcodeMCPMergesAndRemoves(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := t.TempDir()
	on := true
	cm := config.NewManager("", &config.Config{
		Listen: "127.0.0.1:8321", GatewayToken: "SECRETTOKEN123", DataDir: dataDir, AgentdeckMCP: &on,
	})
	m := NewManager(cm, nil)
	m.ensureMcodeMCP()

	path := filepath.Join(home, ".minimax", "mcp.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("mcp.json not written: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if _, ok := servers["agentdeck"]; !ok {
		t.Fatalf("agentdeck entry missing: %s", raw)
	}
	// The token must stay in cc-webui's wrapper env file, never in mcp.json.
	if strings.Contains(string(raw), "SECRETTOKEN123") {
		t.Fatalf("token leaked into mcp.json: %s", raw)
	}

	// Turning the feature off removes only our entry.
	off := false
	cm2 := config.NewManager("", &config.Config{
		Listen: "127.0.0.1:8321", GatewayToken: "SECRETTOKEN123", DataDir: t.TempDir(), AgentdeckMCP: &off,
	})
	NewManager(cm2, nil).ensureMcodeMCP()
	raw2, _ := os.ReadFile(path)
	if strings.Contains(string(raw2), "agentdeck") {
		t.Fatalf("entry not removed: %s", raw2)
	}
}

func TestServeKeyIncludesConfigScope(t *testing.T) {
	cli := &config.CLI{Name: "oc", Path: "oc", Kind: config.KindOpenCode}
	prov := &config.Provider{Name: "p", Kind: config.KindOpenCode}
	a := serveKey(cli, prov, "m")
	b := serveKey(cli, prov, "m", "s1")
	if a == b || !strings.Contains(b, "scope=s1") {
		t.Fatalf("config scope not in key: %q vs %q", a, b)
	}
}

func TestOpencodePerSessionRolePrompt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	cm := config.NewManager("", &config.Config{Listen: "127.0.0.1:8321", GatewayToken: "tok", DataDir: dir})
	m := NewManager(cm, st)
	if err := st.Upsert(&store.Session{
		ID: "s1", Name: "main", MainAgent: true, RolePrompt: "你是主 Agent", Cwd: dir,
	}, nil); err != nil {
		t.Fatal(err)
	}
	cfgDir := m.ensureOpencodeRuleFor("s1")
	if cfgDir == "" {
		t.Fatal("no per-session config dir")
	}
	raw, _ := os.ReadFile(filepath.Join(cfgDir, "opencode.json"))
	if !strings.Contains(string(raw), "ROLE_PROMPT.md") {
		t.Fatalf("role prompt not wired into instructions: %s", raw)
	}
	roleRaw, err := os.ReadFile(filepath.Join(cfgDir, "ROLE_PROMPT.md"))
	if err != nil || !strings.Contains(string(roleRaw), "你是主 Agent") {
		t.Fatalf("role file missing/wrong: %v %s", err, roleRaw)
	}
}

func TestOpencodeConfigScope(t *testing.T) {
	// Browser off: only coordinating sessions get their own config.
	m := browserTestManager(t)
	m.browserEnabled = nil // simulate browser disabled
	if s := m.opencodeConfigScope(&store.Session{ID: "plain"}); s != "" {
		t.Fatalf("plain session scope = %q, want empty", s)
	}
	if s := m.opencodeConfigScope(&store.Session{ID: "main", MainAgent: true}); s != "main" {
		t.Fatalf("main agent scope = %q, want main", s)
	}
	if s := m.opencodeConfigScope(&store.Session{ID: "w", PeerName: "worker-1"}); s != "w" {
		t.Fatalf("addressed session scope = %q, want w", s)
	}
}
