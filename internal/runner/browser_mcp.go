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

// browserMCPInfo describes the stdio MCP server every engine spawns. It is
// only available while the Settings browser switch is on.
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

func (m *Manager) browserSessionFor(sessionID string) string {
	if m.browserEnabled != nil && m.browserEnabled() {
		return sessionID
	}
	return ""
}

// appendBrowserMCP adds a per-session MCP config file for Claude/qoder. It
// contains browser env (including its token) and the board server for
// main-agent sessions, so the file is 0600. The agent process only sees the
// path; tokens are never logged.
func (m *Manager) appendBrowserMCP(args []string, sessionID string) []string {
	servers := m.sessionMCPServers(sessionID)
	if len(servers) == 0 {
		return args
	}
	cfg := m.cfg()
	dir := filepath.Join(cfg.DataDir, "mcp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("mcp: create dir: %v", err)
		return args
	}
	mcpServers := map[string]any{}
	for name, info := range servers {
		entry := map[string]any{"command": info.Command, "args": info.Args}
		if len(info.Env) > 0 {
			entry["env"] = info.Env
		}
		mcpServers[name] = entry
	}
	config := map[string]any{"mcpServers": mcpServers}
	raw, err := json.Marshal(config)
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

// appendBrowserMCPCodex adds Codex-style `-c mcp_servers.*` overrides for
// browser and/or board. Codex only accepts literal values in -c, so tokens are
// kept in 0600 env files read by 0700 wrappers; argv contains only paths.
func (m *Manager) appendBrowserMCPCodex(args []string, sessionID string) ([]string, bool) {
	servers := m.sessionMCPServers(sessionID)
	if len(servers) == 0 {
		return args, false
	}
	cfg := m.cfg()
	dir := filepath.Join(cfg.DataDir, "mcp", "codex", safeSessionName(sessionID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("mcp codex: create dir: %v", err)
		return args, false
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		info := servers[name]
		script := "#!/bin/sh\nset -eu\n"
		if len(info.Env) > 0 {
			envPath := filepath.Join(dir, name+".env")
			var env strings.Builder
			keys := make([]string, 0, len(info.Env))
			for key := range info.Env {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				env.WriteString("export " + key + "=" + shellQuote(info.Env[key]) + "\n")
			}
			if err := os.WriteFile(envPath, []byte(env.String()), 0o600); err != nil {
				log.Printf("mcp codex: write env: %v", err)
				return args, false
			}
			script += ". " + shellQuote(envPath) + "\n"
		}
		wrapper := filepath.Join(dir, name+".sh")
		script += "exec " + shellQuote(info.Command)
		for _, arg := range info.Args {
			script += " " + shellQuote(arg)
		}
		script += "\n"
		if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
			log.Printf("mcp codex: write wrapper: %v", err)
			return args, false
		}
		args = append(args,
			"-c", "mcp_servers."+name+".command="+strconv.Quote(wrapper),
			"-c", "mcp_servers."+name+".args=[]",
		)
	}
	return args, true
}
