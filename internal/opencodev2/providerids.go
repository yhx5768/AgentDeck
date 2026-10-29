package opencodev2

import (
	"encoding/json"
	"fmt"
)

// ProviderIDs lists the provider ids the CLI currently exposes: its logged-in
// integrations plus the built-ins. cc-webui's provider auto-discovery uses it
// so a newly authenticated provider shows up without a config.yaml entry.
//
// v2 keeps this list on the API, so one `api model.list --standalone` answers it
// for every provider (the same call ProviderCatalog makes for a single one).
func ProviderIDs(cliPath string) ([]string, error) {
	raw, err := runCLI(cliPath, "", "api", "model.list", "--standalone")
	if err != nil {
		return nil, err
	}
	return ParseProviderIDs(raw)
}

// ParseProviderIDs extracts the distinct provider ids from a v2 model.list
// document, skipping deprecated/disabled entries the same way parseCatalog
// does. First-appearance order is preserved (the CLI lists integrations first).
func ParseProviderIDs(raw []byte) ([]string, error) {
	var doc struct {
		Data []struct {
			ProviderID string `json:"providerID"`
			Status     string `json:"status"`
			Enabled    *bool  `json:"enabled"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse opencode model list: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range doc.Data {
		id := m.ProviderID
		if id == "" || seen[id] {
			continue
		}
		if m.Status == "deprecated" || (m.Enabled != nil && !*m.Enabled) {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}
