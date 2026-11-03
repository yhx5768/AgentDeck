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

func boardTestManager(t *testing.T, browser bool) *Manager {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cm := config.NewManager(filepath.Join(dir, "config.yaml"), &config.Config{
		Listen:       "127.0.0.1:8321",
		GatewayToken: "test-token",
		DataDir:      dir,
	})
	if err := st.Upsert(&store.Session{ID: "main", Name: "main", MainAgent: true}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert(&store.Session{ID: "worker", Name: "worker"}, nil); err != nil {
		t.Fatal(err)
	}
	return NewManager(cm, st, func() bool { return browser })
}

func TestBoardMCPOnlyForMainAgent(t *testing.T) {
	m := boardTestManager(t, false)
	info, ok := m.boardMCPInfo("main")
	if !ok {
		t.Fatal("main agent should get board MCP")
	}
	if len(info.Args) != 3 || info.Args[0] != "-board-mcp" || info.Args[1] != "-config" {
		t.Fatalf("args = %#v", info.Args)
	}
	if info.Env != nil {
		t.Fatalf("board MCP must not pass a token env: %#v", info.Env)
	}
	if _, ok := m.boardMCPInfo("worker"); ok {
		t.Fatal("worker should not get board MCP")
	}
}

func TestClaudeMCPConfigIncludesBoard(t *testing.T) {
	m := boardTestManager(t, true)
	args := m.appendBrowserMCP([]string{"--x"}, "main")
	if len(args) != 3 || args[1] != "--mcp-config" {
		t.Fatalf("args = %#v", args)
	}
	raw, err := os.ReadFile(args[2])
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	board, ok := cfg.MCPServers["board"]
	if !ok || len(board.Args) < 1 || board.Args[0] != "-board-mcp" {
		t.Fatalf("board config = %#v (%s)", board, raw)
	}
	if _, ok := cfg.MCPServers["browser"]; !ok {
		t.Fatalf("browser config missing: %s", raw)
	}
	boardRaw, _ := json.Marshal(board)
	if strings.Contains(string(boardRaw), "test-token") || board.Env != nil {
		t.Fatal("board MCP entry leaked token")
	}

	workerArgs := m.appendBrowserMCP(nil, "worker")
	if len(workerArgs) != 2 {
		t.Fatalf("worker args = %#v", workerArgs)
	}
	workerRaw, err := os.ReadFile(workerArgs[1])
	if err != nil {
		t.Fatal(err)
	}
	var workerCfg struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(workerRaw, &workerCfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := workerCfg.MCPServers["board"]; ok {
		t.Fatal("worker should not get board MCP")
	}
	if _, ok := workerCfg.MCPServers["browser"]; !ok {
		t.Fatal("worker should still get browser MCP when the toggle is on")
	}
}

func TestCodexMCPArgsIncludeBoard(t *testing.T) {
	m := boardTestManager(t, false)
	args, ok := m.appendBrowserMCPCodex([]string{"exec"}, "main")
	if !ok {
		t.Fatal("board codex args missing")
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "mcp_servers.board.command=") {
		t.Fatalf("board missing: %s", joined)
	}
	if strings.Contains(joined, "test-token") {
		t.Fatalf("token leaked into argv: %s", joined)
	}
}

func TestOpencodeMCPConfigIncludesBoard(t *testing.T) {
	m := boardTestManager(t, false)
	dir := m.ensureOpencodeRuleFor("main")
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
				Type    string   `json:"type"`
				Command []string `json:"command"`
			} `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	board, ok := cfg.MCP.Servers["board"]
	if !ok || board.Type != "local" || len(board.Command) < 2 || board.Command[1] != "-board-mcp" {
		t.Fatalf("board config = %#v (%s)", board, raw)
	}
}
