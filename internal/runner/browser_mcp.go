package runner

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// appendBrowserMCP adds a per-session MCP config file. The file contains the
// gateway token, so it is 0600; the agent process only sees it as a path and
// the token is never logged.
func (m *Manager) appendBrowserMCP(args []string, sessionID string) []string {
	if m.browserEnabled == nil || !m.browserEnabled() {
		return args
	}
	cfg := m.cfg()
	if cfg == nil || sessionID == "" {
		return args
	}
	exe, err := os.Executable()
	if err != nil {
		log.Printf("browser mcp: locate executable: %v", err)
		return args
	}
	dir := filepath.Join(cfg.DataDir, "browser", "mcp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("browser mcp: create dir: %v", err)
		return args
	}
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
	config := map[string]any{
		"mcpServers": map[string]any{
			"browser": map[string]any{
				"command": exe,
				"args":    []string{"-browser-mcp"},
				"env": map[string]string{
					"CC_WEBUI_BROWSER_SESSION": sessionID,
					"CC_WEBUI_BROWSER_ADDR":    permLoopbackAddr(cfg.Listen),
					"CC_WEBUI_BROWSER_TOKEN":   cfg.GatewayToken,
				},
			},
		},
	}
	raw, err := json.Marshal(config)
	if err != nil {
		log.Printf("browser mcp: marshal: %v", err)
		return args
	}
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		log.Printf("browser mcp: write config: %v", err)
		return args
	}
	return append(args, "--mcp-config", path)
}
