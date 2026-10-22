// Model catalog discovery for the codex engine.
//
// Native codex ships its model metadata inside the binary: `codex debug models
// --bundled` dumps it as JSON without touching the network (slug, display
// name, supported reasoning levels, input modalities, context window).
//
// A launcher wrapper is different. `ducx` (Baidu's codex fork) serves its own
// gateway models — GLM-5.3, DeepSeek-V4, … — and those are NOT in the bundled
// catalog; `debug models --bundled` only reports the upstream gpt models. The
// wrapper keeps the real list in
// <CODEX_HOME>/model-catalogs/<model_provider>-models.json (the same file
// `ducx models` prints from), with the same metadata shape. So the catalog
// loader prefers that file when a home/provider is known, then falls back to
// the bundled dump, then to `ducx models` (ids only) for short-lived wrapper
// installs that have not materialized the file yet.
package codexhist

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// LoadCatalog returns the catalog of a native codex CLI (no wrapper home).
func LoadCatalog(cliPath string, fresh bool) (*Catalog, error) {
	return LoadCatalogFor(cliPath, "", "", fresh)
}

// LoadCatalogFor returns a CLI's model catalog, memoized for catalogTTL.
// fresh bypasses the memo. home and modelProvider identify a launcher
// wrapper's gateway catalog; both may be empty for native codex.
func LoadCatalogFor(cliPath, home, modelProvider string, fresh bool) (*Catalog, error) {
	if strings.TrimSpace(cliPath) == "" {
		cliPath = "codex"
	}
	key := cliPath + "|" + home + "|" + modelProvider
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if e, ok := catalogCache[key]; ok && !fresh && time.Since(e.when) < catalogTTL {
		return e.cat, nil
	}
	cat, err := fetchCatalog(cliPath, home, modelProvider)
	if err != nil {
		// Keep serving a good previous catalog when a refresh fails.
		if e, ok := catalogCache[key]; ok && e.cat != nil {
			return e.cat, nil
		}
		return nil, err
	}
	if len(catalogCache) > 16 {
		catalogCache = map[string]catalogEntry{}
	}
	catalogCache[key] = catalogEntry{cat: cat, when: time.Now()}
	return cat, nil
}

// fetchCatalog tries the wrapper's own catalog first, then the bundled dump,
// then the wrapper's id-only `models` command.
func fetchCatalog(cliPath, home, modelProvider string) (*Catalog, error) {
	if cat, err := loadWrapperCatalog(home, modelProvider); err == nil {
		return cat, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cliPath, "debug", "models", "--bundled")
	if raw, err := cmd.Output(); err == nil {
		if cat, perr := ParseModels(raw); perr == nil {
			return cat, nil
		}
	}
	// Native codex has no `models` subcommand — passing it would run a prompt
	// with that text. Only launcher wrappers implement it.
	if isCodexWrapper(cliPath) {
		cmd = exec.CommandContext(ctx, cliPath, "models")
		if raw, err := cmd.Output(); err == nil {
			if cat, perr := parseModelsCommand(raw); perr == nil {
				return cat, nil
			}
		}
	}
	return nil, fmt.Errorf("读取 codex 模型目录失败（%s）: 既没有 wrapper catalog，也没有 bundled catalog", cliPath)
}

// loadWrapperCatalog reads a launcher's materialized gateway catalog:
// <home>/model-catalogs/<provider>-models.json. With no provider it accepts
// the sole *-models.json in that directory. Native codex homes do not have
// this directory, so the call fails and the bundled dump is used.
func loadWrapperCatalog(home, modelProvider string) (*Catalog, error) {
	if strings.TrimSpace(home) == "" {
		return nil, fmt.Errorf("no codex home")
	}
	dir := filepath.Join(home, "model-catalogs")
	path := ""
	if modelProvider != "" {
		path = filepath.Join(dir, modelProvider+"-models.json")
	} else {
		matches, _ := filepath.Glob(filepath.Join(dir, "*-models.json"))
		if len(matches) == 1 {
			path = matches[0]
		}
	}
	if path == "" {
		return nil, fmt.Errorf("no wrapper model catalog")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseModels(raw)
}

// isCodexWrapper recognizes the launcher binaries that intercept a `models`
// subcommand. A native codex with an unknown name must never be probed that
// way (it would start a real turn).
func isCodexWrapper(cliPath string) bool {
	base := strings.ToLower(filepath.Base(cliPath))
	return base == "ducx" || base == "baidu-codex"
}

// parseModelsCommand reads the plain-text `ducx models` output
// ("Available Models:\nA, B, C"). It carries ids only: efforts and input
// modalities are unknown, so those models inherit defaults and hide the
// effort dropdown. This is only a fallback for a wrapper whose catalog file
// has not been written yet.
func parseModelsCommand(raw []byte) (*Catalog, error) {
	text := string(raw)
	i := strings.Index(text, "Available Models:")
	if i < 0 {
		return nil, fmt.Errorf("unrecognized models output")
	}
	line := strings.SplitN(strings.TrimSpace(text[i+len("Available Models:"):]), "\n", 2)[0]
	catalog := &Catalog{Models: []Model{}}
	seen := map[string]bool{}
	for _, part := range strings.Split(line, ",") {
		id := strings.TrimSpace(part)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		catalog.Models = append(catalog.Models, Model{
			ID: id, Name: id, Input: []string{"text"},
		})
	}
	if len(catalog.Models) == 0 {
		return nil, fmt.Errorf("empty models output")
	}
	return catalog, nil
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
