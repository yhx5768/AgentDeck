// Model catalog discovery for the codex engine.
//
// Codex ships its model metadata inside the binary. `codex debug models
// --bundled` dumps it as JSON without touching the network, including each
// model's slug, display name, supported reasoning levels, input modalities and
// context window. That is the same list the TUI's picker uses, so it is the
// only source that cannot go stale — the model set changes with every CLI
// update and account tier.
//
// `ducx` (Baidu's wrapper around codex) exposes the same subcommand; its
// catalog additionally carries the internal models the wrapper's gateway
// serves.
package codexhist

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Model is one codex model.
type Model struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"default_effort,omitempty"`
	Input         []string `json:"input"`
	ContextWindow int      `json:"context_window,omitempty"`
}

// Catalog is the model list reported by one CLI binary.
type Catalog struct {
	Models       []Model `json:"models"`
	DefaultModel string  `json:"default_model,omitempty"`
}

// ModelIDs returns the model ids in catalog order.
func (c *Catalog) ModelIDs() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.Models))
	for _, m := range c.Models {
		out = append(out, m.ID)
	}
	return out
}

// Find returns the model with the given id, or nil.
func (c *Catalog) Find(id string) *Model {
	if c == nil {
		return nil
	}
	for i := range c.Models {
		if c.Models[i].ID == id {
			return &c.Models[i]
		}
	}
	return nil
}

// EffortOK reports whether effort is one of the model's supported levels.
func (c *Catalog) EffortOK(id, effort string) bool {
	m := c.Find(id)
	if m == nil || effort == "" {
		return m == nil
	}
	for _, e := range m.Efforts {
		if e == effort {
			return true
		}
	}
	return false
}

// ---- CLI invocation + memo ----

const catalogTTL = 10 * time.Minute

var (
	catalogMu    sync.Mutex
	catalogCache = map[string]catalogEntry{}
)

type catalogEntry struct {
	cat  *Catalog
	when time.Time
}

// LoadCatalog returns the model catalog of the given CLI, memoized for
// catalogTTL. fresh bypasses the memo.
func LoadCatalog(cliPath string, fresh bool) (*Catalog, error) {
	if strings.TrimSpace(cliPath) == "" {
		cliPath = "codex"
	}
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if e, ok := catalogCache[cliPath]; ok && !fresh && time.Since(e.when) < catalogTTL {
		return e.cat, nil
	}
	cat, err := fetchCatalog(cliPath)
	if err != nil {
		// Keep serving a good previous catalog when a refresh fails.
		if e, ok := catalogCache[cliPath]; ok && e.cat != nil {
			return e.cat, nil
		}
		return nil, err
	}
	if len(catalogCache) > 16 {
		catalogCache = map[string]catalogEntry{}
	}
	catalogCache[cliPath] = catalogEntry{cat: cat, when: time.Now()}
	return cat, nil
}

func fetchCatalog(cliPath string) (*Catalog, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cliPath, "debug", "models", "--bundled")
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("读取 codex 模型目录失败（%s）: %w", cliPath, err)
	}
	return ParseModels(raw)
}

// bundledOutput is the subset of `debug models --bundled` we read. The real
// document is ~650 KB of model metadata (base instructions included); defining
// only the keys we need keeps the parse cheap.
type bundledOutput struct {
	Models []bundledModel `json:"models"`
}

type bundledModel struct {
	Slug             string `json:"slug"`
	DisplayName      string `json:"display_name"`
	Visibility       string `json:"visibility"`
	SupportedInAPI   *bool  `json:"supported_in_api"`
	DefaultReasoning string `json:"default_reasoning_level"`
	ReasoningLevels  []struct {
		Effort string `json:"effort"`
	} `json:"supported_reasoning_levels"`
	InputModalities []string `json:"input_modalities"`
	ContextWindow   int      `json:"context_window"`
}

// ParseModels interprets the bundled catalog JSON.
func ParseModels(raw []byte) (*Catalog, error) {
	var out bundledOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		// Some wrappers print a banner before the JSON document; retry from
		// the first '{'.
		i := strings.Index(string(raw), "{")
		if i < 0 {
			return nil, fmt.Errorf("解析 codex 模型目录失败: %w", err)
		}
		if err2 := json.Unmarshal(raw[i:], &out); err2 != nil {
			return nil, fmt.Errorf("解析 codex 模型目录失败: %w", err)
		}
	}
	catalog := &Catalog{Models: []Model{}}
	for _, m := range out.Models {
		if m.Slug == "" {
			continue
		}
		if m.Visibility != "" && m.Visibility != "list" {
			continue
		}
		if m.SupportedInAPI != nil && !*m.SupportedInAPI {
			continue
		}
		efforts := make([]string, 0, len(m.ReasoningLevels))
		for _, lv := range m.ReasoningLevels {
			if lv.Effort != "" {
				efforts = append(efforts, lv.Effort)
			}
		}
		input := m.InputModalities
		if len(input) == 0 {
			input = []string{"text"}
		}
		name := m.DisplayName
		if name == "" {
			name = m.Slug
		}
		catalog.Models = append(catalog.Models, Model{
			ID:            m.Slug,
			Name:          name,
			Efforts:       efforts,
			DefaultEffort: m.DefaultReasoning,
			Input:         input,
			ContextWindow: m.ContextWindow,
		})
	}
	if len(catalog.Models) == 0 {
		return nil, fmt.Errorf("codex 模型目录为空（CLI 是否支持 debug models --bundled？）")
	}
	return catalog, nil
}
