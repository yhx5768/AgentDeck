package runner

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type browserMCPConfig struct {
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

// browserMCPInfo describes the stdio MCP server every engine spawns. It is
// only available while the Settings browser switch is on.
func (m *Manager) browserMCPInfo(sessionID string) (browserMCPConfig, bool) {
	if m.browserEnabled == nil || !m.browserEnabled() || sessionID == "" {
		return browserMCPConfig{}, false
	}
	cfg := m.cfg()
	if cfg == nil {
		return browserMCPConfig{}, false
	}
	exe, err := os.Executable()
	if err != nil {
		log.Printf("browser mcp: locate executable: %v", err)
		return browserMCPConfig{}, false
	}
	return browserMCPConfig{
		Command: exe,
		Args:    []string{"-browser-mcp"},
		Env: map[string]string{
			"CC_WEBUI_BROWSER_SESSION": sessionID,
			"CC_WEBUI_BROWSER_ADDR":    permLoopbackAddr(cfg.Listen),
			"CC_WEBUI_BROWSER_TOKEN":   cfg.GatewayToken,
		},
	}, true
}

func (m *Manager) browserSessionFor(sessionID string) string {
	if m.browserEnabled != nil && m.browserEnabled() {
		return sessionID
	}
	return ""
}

// appendBrowserMCP adds a per-session MCP config file for Claude/qoder. The
// file contains the gateway token, so it is 0600; the agent process only sees
// it as a path and the token is never logged.
func (m *Manager) appendBrowserMCP(args []string, sessionID string) []string {
	info, ok := m.browserMCPInfo(sessionID)
	if !ok {
		return args
	}
	cfg := m.cfg()
	dir := filepath.Join(cfg.DataDir, "browser", "mcp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("browser mcp: create dir: %v", err)
		return args
	}
	config := map[string]any{
		"mcpServers": map[string]any{
			"browser": map[string]any{
				"command": info.Command,
				"args":    info.Args,
				"env":     info.Env,
			},
		},
	}
	raw, err := json.Marshal(config)
	if err != nil {
		log.Printf("browser mcp: marshal: %v", err)
		return args
	}
	path := filepath.Join(dir, safeSessionName(sessionID)+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		log.Printf("browser mcp: write config: %v", err)
		return args
	}
	return append(args, "--mcp-config", path)
}

// appendBrowserMCPCodex adds Codex-style `-c mcp_servers.*` overrides. Codex
// parses each value as TOML; strings are quoted and the env is an inline table.
func (m *Manager) appendBrowserMCPCodex(args []string, sessionID string) ([]string, bool) {
	info, ok := m.browserMCPInfo(sessionID)
	if !ok {
		return args, false
	}
	// Codex only accepts literal values in -c, so putting the token in env
	// would expose it in argv/ps. Use a 0700 wrapper that reads a 0600 env file
	// and execs the MCP server; argv contains only the wrapper path.
	cfg := m.cfg()
	dir := filepath.Join(cfg.DataDir, "browser", "mcp", "codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("browser mcp codex: create dir: %v", err)
		return args, false
	}
	name := safeSessionName(sessionID)
	envPath := filepath.Join(dir, name+".env")
	var env strings.Builder
	for _, key := range []string{"CC_WEBUI_BROWSER_SESSION", "CC_WEBUI_BROWSER_ADDR", "CC_WEBUI_BROWSER_TOKEN"} {
		env.WriteString("export " + key + "=" + shellQuote(info.Env[key]) + "\n")
	}
	if err := os.WriteFile(envPath, []byte(env.String()), 0o600); err != nil {
		log.Printf("browser mcp codex: write env: %v", err)
		return args, false
	}
	wrapper := filepath.Join(dir, name+".sh")
	script := "#!/bin/sh\nset -eu\n. " + shellQuote(envPath) + "\nexec " + shellQuote(info.Command)
	for _, arg := range info.Args {
		script += " " + shellQuote(arg)
	}
	script += "\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		log.Printf("browser mcp codex: write wrapper: %v", err)
		return args, false
	}
	args = append(args,
		"-c", "mcp_servers.browser.command="+strconv.Quote(wrapper),
		"-c", "mcp_servers.browser.args=[]",
	)
	return args, true
}
