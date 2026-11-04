package runner

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

const mcodeAgentdeckKey = "agentdeck"

// mcodeMCPPath resolves mcode's user-scope MCP config file. mcode reads
// <MCODE_CONFIG_DIR>/mcp.json (default ~/.minimax/mcp.json); the wrapper object
// only allows the $schema and mcpServers keys.
func mcodeMCPPath() (string, error) {
	base := os.Getenv("MCODE_CONFIG_DIR")
	if base == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return "", fmt.Errorf("HOME is not set")
		}
		base = filepath.Join(home, ".minimax")
	}
	return filepath.Join(base, "mcp.json"), nil
}

// ensureMcodeMCP merges (or, when the agentdeck MCP is switched off, removes)
// cc-webui's agentdeck MCP server in mcode's user-scope mcp.json.
//
// mcode has no per-session MCP config, so this file is global: the entry is
// namespaced under mcpServers.agentdeck and points at a 0700 wrapper that reads
// a 0600 env file, so the gateway token never lands in the user's config. Any
// failure only logs — losing the tool degrades the feature, it must not fail
// the turn.
func (m *Manager) ensureMcodeMCP() {
	if m.cm == nil {
		return
	}
	path, err := mcodeMCPPath()
	if err != nil {
		log.Printf("mcode mcp: %v", err)
		return
	}
	if !m.cm.Get().AgentdeckMCPEnabled() {
		m.removeMcodeMCP(path)
		return
	}
	info, ok := m.agentdeckMCPInfo("")
	if !ok {
		return
	}
	wrapper, err := m.writeMCPWrapper("", mcodeAgentdeckKey, info)
	if err != nil {
		log.Printf("mcode mcp: write wrapper: %v", err)
		return
	}
	doc := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &doc)
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers[mcodeAgentdeckKey] = map[string]any{"command": wrapper, "args": []string{}}
	doc["mcpServers"] = servers
	if _, ok := doc["$schema"]; !ok {
		doc["$schema"] = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		log.Printf("mcode mcp: marshal: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("mcode mcp: mkdir: %v", err)
		return
	}
	if err := writeIfChangedMode(path, append(raw, '\n'), 0o600); err != nil {
		log.Printf("mcode mcp: write %s: %v", path, err)
	}
}

func (m *Manager) removeMcodeMCP(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	doc := map[string]any{}
	if json.Unmarshal(raw, &doc) != nil {
		return
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		return
	}
	if _, ok := servers[mcodeAgentdeckKey]; !ok {
		return
	}
	delete(servers, mcodeAgentdeckKey)
	doc["mcpServers"] = servers
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	_ = writeIfChangedMode(path, append(out, '\n'), 0o600)
}
