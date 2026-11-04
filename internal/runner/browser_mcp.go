package runner

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// mcpServerConfig describes one stdio MCP server an engine spawns. Both the
// browser server (browser.go) and the agentdeck server (a2a) are described this
// way and injected through the same per-engine plumbing.
type mcpServerConfig struct {
	Command string
	Args    []string
	Env     map[string]string
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func safeSessionName(sessionID string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, sessionID)
	if len(name) > 80 {
		name = name[:80]
	}
	if name == "" {
		name = "session"
	}
	return name
}

// browserMCPInfo describes the browser stdio MCP server, available only while
// the Settings browser switch is on.
func (m *Manager) browserMCPInfo(sessionID string) (mcpServerConfig, bool) {
	if m.browserEnabled == nil || !m.browserEnabled() || sessionID == "" {
		return mcpServerConfig{}, false
	}
	cfg := m.cfg()
	if cfg == nil {
		return mcpServerConfig{}, false
	}
	exe, err := os.Executable()
	if err != nil {
		log.Printf("browser mcp: locate executable: %v", err)
		return mcpServerConfig{}, false
	}
	return mcpServerConfig{
		Command: exe,
		Args:    []string{"-browser-mcp"},
		Env: map[string]string{
			"CC_WEBUI_BROWSER_SESSION": sessionID,
			"CC_WEBUI_BROWSER_ADDR":    permLoopbackAddr(cfg.Listen),
			"CC_WEBUI_BROWSER_TOKEN":   cfg.GatewayToken,
		},
	}, true
}

// agentdeckMCPInfo describes the agentdeck stdio MCP server (A2A + session
// management). It is injected into every engine so any agent can drive
// AgentDeck and talk to peer sessions. sessionID, when known, lets send_message
// default its `from` to the caller's address; engines whose MCP config is
// shared (opencode base, mcode) pass "" and rely on an explicit `from`.
func (m *Manager) agentdeckMCPInfo(sessionID string) (mcpServerConfig, bool) {
	if m.cm == nil || !m.cm.Get().AgentdeckMCPEnabled() {
		return mcpServerConfig{}, false
	}
	cfg := m.cfg()
	if cfg == nil {
		return mcpServerConfig{}, false
	}
	exe, err := os.Executable()
	if err != nil {
		log.Printf("agentdeck mcp: locate executable: %v", err)
		return mcpServerConfig{}, false
	}
	env := map[string]string{
		"CC_WEBUI_A2A_ADDR":  permLoopbackAddr(cfg.Listen),
		"CC_WEBUI_A2A_TOKEN": cfg.GatewayToken,
	}
	if sessionID != "" {
		env["CC_WEBUI_A2A_SESSION"] = sessionID
		// Board tools are main-agent only: the flag makes the proxy advertise
		// them, and the server re-checks the caller's main_agent flag on every
		// call. A worker's tool list therefore never carries the board tools.
		if m.st != nil {
			if sess := m.st.Get(sessionID); sess != nil && sess.MainAgent {
				env["CC_WEBUI_A2A_BOARD"] = "1"
			}
		}
	}
	return mcpServerConfig{
		Command: exe,
		Args:    []string{"-agentdeck-mcp"},
		Env:     env,
	}, true
}

// mcpServersMap collects every MCP server this session should get, keyed by the
// name the engine will show in the tool list.
func (m *Manager) mcpServersMap(sessionID string) map[string]mcpServerConfig {
	out := map[string]mcpServerConfig{}
	if info, ok := m.browserMCPInfo(sessionID); ok {
		out["browser"] = info
	}
	if info, ok := m.agentdeckMCPInfo(sessionID); ok {
		out["agentdeck"] = info
	}
	return out
}

// appendMCP adds a per-session `--mcp-config` file for Claude/qoder carrying
// every MCP server. The file contains the gateway token, so it is 0600; the
// agent process only sees it as a path and the token is never logged.
func (m *Manager) appendMCP(args []string, sessionID string) []string {
	servers := m.mcpServersMap(sessionID)
	if len(servers) == 0 {
		return args
	}
	cfg := m.cfg()
	dir := filepath.Join(cfg.DataDir, "mcp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("mcp: create dir: %v", err)
		return args
	}
	entries := map[string]any{}
	for name, info := range servers {
		entries[name] = map[string]any{
			"command": info.Command,
			"args":    info.Args,
			"env":     info.Env,
		}
	}
	raw, err := json.Marshal(map[string]any{"mcpServers": entries})
	if err != nil {
		log.Printf("mcp: marshal: %v", err)
		return args
	}
	path := filepath.Join(dir, safeSessionName(sessionID)+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		log.Printf("mcp: write config: %v", err)
		return args
	}
	return append(args, "--mcp-config", path)
}

// appendMCPCodex adds Codex-style `-c mcp_servers.*` overrides for every MCP
// server. Codex parses each value as TOML and only accepts literal values, so
// putting the token in env would expose it in argv/ps. Instead each server gets
// a 0700 wrapper that reads a 0600 env file and execs the MCP server; argv
// contains only the wrapper path.
func (m *Manager) appendMCPCodex(args []string, sessionID string) ([]string, bool) {
	servers := m.mcpServersMap(sessionID)
	if len(servers) == 0 {
		return args, false
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic argv order
	any := false
	for _, name := range names {
		wrapper, err := m.writeMCPWrapper(sessionID, name, servers[name])
		if err != nil {
			log.Printf("codex mcp %s: %v", name, err)
			continue
		}
		args = append(args,
			"-c", "mcp_servers."+name+".command="+strconv.Quote(wrapper),
			"-c", "mcp_servers."+name+".args=[]",
		)
		any = true
	}
	return args, any
}

// writeMCPWrapper writes a 0700 wrapper that reads a 0600 env file and execs an
// MCP server. Engines whose MCP config only accepts literal values (codex -c,
// mcode mcp.json) point at the wrapper so the gateway token never lands in
// argv/ps or in a config file the user might share.
func (m *Manager) writeMCPWrapper(sessionID, name string, info mcpServerConfig) (string, error) {
	dir := filepath.Join(m.cfg().DataDir, "mcp", "wrappers")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	base := safeSessionName(sessionID) + "-" + safeSessionName(name)
	envPath := filepath.Join(dir, base+".env")
	var env strings.Builder
	keys := make([]string, 0, len(info.Env))
	for k := range info.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env.WriteString("export " + key + "=" + shellQuote(info.Env[key]) + "\n")
	}
	if err := os.WriteFile(envPath, []byte(env.String()), 0o600); err != nil {
		return "", err
	}
	wrapper := filepath.Join(dir, base+".sh")
	script := "#!/bin/sh\nset -eu\n. " + shellQuote(envPath) + "\nexec " + shellQuote(info.Command)
	for _, arg := range info.Args {
		script += " " + shellQuote(arg)
	}
	script += "\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		return "", err
	}
	return wrapper, nil
}
