package runner

import (
	"log"
	"os"
)

// boardMCPInfo describes the task-board stdio server injected into main-agent
// sessions. The MCP process reads URL/token from config.yaml itself, so the
// server config carries only the config path.
func (m *Manager) boardMCPInfo(sessionID string) (mcpServerConfig, bool) {
	if sessionID == "" || m.cm == nil || m.st == nil {
		return mcpServerConfig{}, false
	}
	sess := m.st.Get(sessionID)
	if sess == nil || !sess.MainAgent {
		return mcpServerConfig{}, false
	}
	exe, err := os.Executable()
	if err != nil {
		log.Printf("board mcp: locate executable: %v", err)
		return mcpServerConfig{}, false
	}
	path := m.cm.Path()
	if path == "" {
		return mcpServerConfig{}, false
	}
	return mcpServerConfig{
		Command: exe,
		Args:    []string{"-board-mcp", "-config", path},
	}, true
}

// sessionMCPServers returns every MCP server a session should see. Browser is
// gated by the Settings toggle; board is gated by the main-agent marker.
func (m *Manager) sessionMCPServers(sessionID string) map[string]mcpServerConfig {
	servers := map[string]mcpServerConfig{}
	if info, ok := m.browserMCPInfo(sessionID); ok {
		servers["browser"] = info
	}
	if info, ok := m.boardMCPInfo(sessionID); ok {
		servers["board"] = info
	}
	return servers
}
