package opencodev2

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// errNoModels guards against a layout change that parses to zero models:
// serving that would silently empty the picker and make every session of the
// provider unselectable.
var errNoModels = errors.New("opencode 未返回任何模型（CLI 是否已登录 / providerID 是否正确？）")

// Model is one model of a provider's v2 catalog.
type Model struct {
	ID    string   `json:"id"`
	Input []string `json:"input"`
}

// Catalog is one provider's v2 catalog: its models plus the reasoning-variant
// keys per model.
type Catalog struct {
	Models   []Model
	Variants map[string][]string
}

// ProviderCatalog reads one provider's catalog from the v2 HTTP surface
// (`opencode api model.list --standalone`).
//
// v2 dropped the `opencode models <provider> --verbose` output cc-webui used
// to parse; the model metadata now lives on the API and is richer (variants,
// input modalities, status). refresh is accepted for signature parity with
// opencodehist.ProviderCatalog and ignored — v2's model list is always live.
func ProviderCatalog(cliPath, providerID string, refresh bool) (*Catalog, error) {
	raw, err := runCLI(cliPath, "", "api", "model.list", "--standalone")
	if err != nil {
		return nil, err
	}
	cat, err := parseCatalog(raw, providerID)
	if err != nil {
		return nil, err
	}
	if len(cat.Models) == 0 {
		return nil, errNoModels
	}
	return cat, nil
}

func parseCatalog(raw []byte, providerID string) (*Catalog, error) {
	var doc struct {
		Data []struct {
			ID           string `json:"id"`
			ModelID      string `json:"modelID"`
			ProviderID   string `json:"providerID"`
			Status       string `json:"status"`
			Enabled      *bool  `json:"enabled"`
			Capabilities struct {
				Input []string `json:"input"`
			} `json:"capabilities"`
			Variants []struct {
				ID string `json:"id"`
			} `json:"variants"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse opencode model list: %w", err)
	}
	out := &Catalog{Variants: map[string][]string{}}
	for _, m := range doc.Data {
		if providerID != "" && m.ProviderID != providerID {
			continue
		}
		if m.Status == "deprecated" || (m.Enabled != nil && !*m.Enabled) {
			continue
		}
		id := m.ID
		if id == "" {
			id = m.ModelID
		}
		if id == "" {
			continue
		}
		input := m.Capabilities.Input
		if len(input) == 0 {
			input = []string{"text"}
		}
		out.Models = append(out.Models, Model{ID: id, Input: input})
		keys := make([]string, 0, len(m.Variants))
		for _, v := range m.Variants {
			if v.ID != "" {
				keys = append(keys, v.ID)
			}
		}
		sort.Slice(keys, func(i, j int) bool { return variantRank(keys[i]) < variantRank(keys[j]) })
		if len(keys) > 0 {
			out.Variants[id] = keys
		}
	}
	return out, nil
}

// Find reports whether the catalog declares a model under that id.
func (c *Catalog) Find(id string) bool {
	if c == nil {
		return false
	}
	for i := range c.Models {
		if c.Models[i].ID == id {
			return true
		}
	}
	return false
}

// variantRank orders variant keys by reasoning depth (none → max → thinking),
// with unknown keys last.
func variantRank(k string) int {
	switch k {
	case "none", "off", "disabled", "default":
		return 0
	case "minimal":
		return 1
	case "low":
		return 2
	case "medium":
		return 3
	case "high":
		return 4
	case "xhigh":
		return 5
	case "max":
		return 6
	case "thinking":
		return 7
	default:
		return 100
	}
}

// String makes a catalog loggable without dumping every model's metadata.
func (c *Catalog) String() string {
	if c == nil {
		return "<nil catalog>"
	}
	return fmt.Sprintf("opencode v2 catalog: %d models, %d with variants", len(c.Models), len(c.Variants))
}
