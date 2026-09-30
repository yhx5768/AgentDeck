// Model catalog discovery for the mcode engine.
//
// mcode exposes no CLI surface for listing its models: `mcode provider list
// --json` returns a `models: []` array for every provider, because the active
// set is resolved at run time from the credential source (`minimaxModelSource:
// token_plan`). The ACP protocol's session/new would carry the list, but it
// only answers after a full JSON-RPC handshake against a live agent and does
// not respond at all without one, which is far too heavy for a dropdown.
//
// The resolved list does exist on disk: mcode keeps it in its runtime config
// (~/.minimax/config.yaml), which holds every provider's model definitions,
// their `model_order`, per-model reasoning variants and the `effortOptions`
// the CLI itself validates `--effort` against. So the catalog is read from
// that file — the same relationship claudehist has with ~/.claude and mcode
// has with its own session store.
package mcodehist

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// configEnv overrides the mcode runtime config location (tests, relocated
// profiles). Same spirit as CLAUDE_CONFIG_DIR for the claude engine.
const configEnv = "MINIMAX_CONFIG_DIR"

// defaultConfigRel is the config path relative to the user's home.
const defaultConfigRel = ".minimax/config.yaml"

// Model is one model mcode can run, as declared in its runtime config.
type Model struct {
	// ID is the bare model key, e.g. "MiniMax-M3.1-Flash-Preview".
	ID string `json:"id"`
	// Name is the display label from the config, falling back to ID.
	Name string `json:"name"`
	// Efforts are the reasoning-depth levels --effort accepts for this
	// model. Empty means the model has no switchable depth and the UI hides
	// the dropdown (mirroring how opencode hides --variant for models with
	// no variants).
	Efforts []string `json:"efforts"`
	// DefaultEffort is the level mcode uses when none is given.
	DefaultEffort string `json:"default_effort,omitempty"`
	// Input lists the accepted input modalities ("text", "image", "video").
	// The composer uses it to decide whether an image can be attached.
	Input []string `json:"input"`
	// Attachment mirrors the config's own attachment flag (whether the
	// model accepts `--file` attachments at all).
	Attachment bool `json:"attachment"`
	// ContextWindow is the declared context limit, for display only.
	ContextWindow int `json:"context_window,omitempty"`
}

// Catalog is the resolved model list of one mcode provider.
type Catalog struct {
	// ProviderID is the config key ("minimax"), the prefix of every
	// --model argument ("minimax/<modelId>").
	ProviderID string `json:"provider_id"`
	// Models are ordered by the config's model_order where declared, then
	// alphabetically, so the dropdown matches the TUI's own ordering.
	Models []Model `json:"models"`
	// DefaultModel is the config's defaultModel id, empty when unset.
	DefaultModel string `json:"default_model,omitempty"`
}

// ModelIDs returns the bare model ids in catalog order.
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

// Find returns the model with the given id (bare id, or a
// "provider/model" string), or nil.
func (c *Catalog) Find(id string) *Model {
	if c == nil {
		return nil
	}
	bare := BareModelID(id)
	for i := range c.Models {
		if c.Models[i].ID == bare {
			return &c.Models[i]
		}
	}
	return nil
}

// BareModelID strips a "provider/" prefix from a model argument, so
// "minimax/MiniMax-M3" and "MiniMax-M3" both resolve. Model ids may contain
// slashes themselves (they are keys in a nested config map), so only the
// first segment before a slash is treated as a provider when it matches the
// catalog's provider id.
func BareModelID(id string) string {
	if i := strings.Index(id, "/"); i > 0 {
		return id[i+1:]
	}
	return id
}

// ---- on-disk config shapes (only the keys we read) ----

type fileConfig struct {
	DefaultModel string                  `yaml:"defaultModel"`
	Provider     map[string]fileProvider `yaml:"provider"`
}

type fileProvider struct {
	Models     map[string]fileModel `yaml:"models"`
	ModelOrder []string             `yaml:"model_order"`
	Whitelist  []string             `yaml:"whitelist"`
}

type fileModel struct {
	Name       string `yaml:"name"`
	Attachment bool   `yaml:"attachment"`
	Reasoning  bool   `yaml:"reasoning"`
	Modalities struct {
		Input []string `yaml:"input"`
	} `yaml:"modalities"`
	Limit struct {
		Context int `yaml:"context"`
	} `yaml:"limit"`
	// variants maps a reasoning-mode variant to its settings; its keys are
	// the depth choices for models that switch between modes rather than
	// levels (M3: none-thinking / thinking).
	Variants map[string]yaml.Node `yaml:"variants"`
	// thinking.effortOptions is the explicit --effort level list. Present
	// only on models whose depth is a real level (M3.1: default, low,
	// medium, high, xhigh, max).
	Thinking struct {
		EffortOptions []string `yaml:"effortOptions"`
		DefaultEffort string   `yaml:"defaultEffort"`
	} `yaml:"thinking"`
}

// configPath returns the mcode runtime config file to read.
func configPath() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(configEnv)); dir != "" {
		return filepath.Join(dir, "config.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("mcode config dir: %w", err)
	}
	return filepath.Join(home, filepath.FromSlash(defaultConfigRel)), nil
}

// LoadCatalog reads the mcode runtime config and returns the model catalog of
// its first declared provider, or the one named by providerID (empty = the
// config's defaultModel's provider, else the sole/first provider key).
//
// The cache keeps the dropdown cheap: the frontend polls this per model
// selection, and the file only changes when the user runs `mcode provider
// add` or a CLI update rewrites it. TTL matches the opencode variants cache
// (10 minutes) so a newly installed model shows up on its own; `fresh`
// bypasses it for an explicit refresh.
var (
	catalogMu    sync.Mutex
	catalogCache = map[string]cachedCatalog{}
)

type cachedCatalog struct {
	cat  *Catalog
	when time.Time
	// sig is the file's mtime+size, so an in-place edit invalidates the
	// entry even inside the TTL.
	sig string
}

const catalogTTL = 10 * time.Minute

// LoadCatalog returns the catalog for providerID, memoized for catalogTTL.
func LoadCatalog(providerID string) (*Catalog, error) {
	return loadCatalog(providerID, false)
}

// LoadCatalogFresh skips the memo and re-reads the config file.
func LoadCatalogFresh(providerID string) (*Catalog, error) {
	return loadCatalog(providerID, true)
}

func loadCatalog(providerID string, fresh bool) (*Catalog, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	sig := fileSig(path)

	catalogMu.Lock()
	defer catalogMu.Unlock()
	key := providerID + "|" + sig
	if c, ok := catalogCache[key]; ok && !fresh && time.Since(c.when) < catalogTTL {
		return c.cat, nil
	}
	cat, err := parseCatalog(path, providerID)
	if err != nil {
		return nil, err
	}
	// Rebind the map so a changed signature never leaves a stale entry
	// reachable, then store under the fresh key.
	if len(catalogCache) > 16 {
		catalogCache = map[string]cachedCatalog{}
	}
	catalogCache[key] = cachedCatalog{cat: cat, when: time.Now(), sig: sig}
	return cat, nil
}

// parseCatalog reads and interprets the config file. Split from loadCatalog
// so the interpretation is testable without touching the cache or a real
// user home.
func parseCatalog(path, providerID string) (*Catalog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 mcode 配置失败（%s）: %w", path, err)
	}
	var fc fileConfig
	if err := yaml.Unmarshal(raw, &fc); err != nil {
		return nil, fmt.Errorf("解析 mcode 配置失败（%s）: %w", path, err)
	}
	pid, prov, err := pickProvider(&fc, providerID)
	if err != nil {
		return nil, err
	}

	ids := orderModels(prov)
	out := make([]Model, 0, len(ids))
	for _, id := range ids {
		fm := prov.Models[id]
		m := Model{
			ID:            id,
			Name:          fm.Name,
			Efforts:       effortLevels(fm),
			DefaultEffort: fm.Thinking.DefaultEffort,
			Input:         fm.Modalities.Input,
			Attachment:    fm.Attachment,
			ContextWindow: fm.Limit.Context,
		}
		if m.Name == "" {
			m.Name = id
		}
		out = append(out, m)
	}
	return &Catalog{ProviderID: pid, Models: out, DefaultModel: BareModelID(fc.DefaultModel)}, nil
}

// pickProvider chooses which provider's models to report: the requested one,
// else the one owning the config's defaultModel, else the sole provider, else
// the first in map order (sorted, so the choice is deterministic).
//
// A requested name that no provider matches falls back to the defaultModel's
// provider rather than erroring: the caller passes a cc-webui provider name,
// which only has to be unique within cc-webui and need not equal mcode's own
// provider id. Reporting the default provider's catalog is the useful answer
// whenever there is just one; error only when the config declares no provider
// at all.
func pickProvider(fc *fileConfig, want string) (string, fileProvider, error) {
	if len(fc.Provider) == 0 {
		return "", fileProvider{}, fmt.Errorf("mcode 配置里没有 provider 段，无法获取模型列表")
	}
	if want != "" {
		if p, ok := fc.Provider[want]; ok {
			return want, p, nil
		}
	}
	if i := strings.Index(fc.DefaultModel, "/"); i > 0 {
		if p, ok := fc.Provider[fc.DefaultModel[:i]]; ok {
			return fc.DefaultModel[:i], p, nil
		}
	}
	names := make([]string, 0, len(fc.Provider))
	for n := range fc.Provider {
		names = append(names, n)
	}
	sortStrings(names)
	return names[0], fc.Provider[names[0]], nil
}

// orderModels returns model ids in the order the TUI would list them:
// model_order first, then anything else alphabetically. A whitelist, when
// declared, filters out models the user has not enabled.
func orderModels(p fileProvider) []string {
	allowed := map[string]bool{}
	if len(p.Whitelist) > 0 {
		for _, w := range p.Whitelist {
			allowed[w] = true
		}
	}
	seen := map[string]bool{}
	var ids []string
	for _, id := range p.ModelOrder {
		if _, ok := p.Models[id]; !ok || seen[id] {
			continue
		}
		if len(allowed) > 0 && !allowed[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	rest := make([]string, 0, len(p.Models))
	for id := range p.Models {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	sortStrings(rest)
	ids = append(ids, rest...)
	return ids
}

// effortLevels returns the --effort levels a model accepts. The explicit
// `thinking.effortOptions` list wins; otherwise the model's variant keys
// serve as the choices (M3 switches none-thinking/thinking instead of named
// levels). A model with neither (forced-on thinking, e.g. M2.7) has no
// switchable depth and yields nil, which the UI renders as no dropdown.
//
// Verified against the CLI's own rejection message, which lists exactly this
// set: `--effort bogus-level is not available for
// minimax/MiniMax-M3.1-Flash-Preview. Available levels: default, low, medium,
// high, xhigh, max.`
func effortLevels(fm fileModel) []string {
	if len(fm.Thinking.EffortOptions) > 0 {
		return append([]string(nil), fm.Thinking.EffortOptions...)
	}
	if len(fm.Variants) > 0 {
		out := make([]string, 0, len(fm.Variants))
		for k := range fm.Variants {
			out = append(out, k)
		}
		sortStrings(out)
		return out
	}
	return nil
}

// effortOK reports whether level is a valid --effort value for model. An
// empty level means "inherit" and is always allowed. Unlike claude (a fixed
// low..max set) and opencode (any key shape, validated against the variant
// list the API serves), mcode's levels are per-model and short, so anything
// not in the model's declared list is rejected — the CLI itself fails the
// run on an unknown level, and a run that dies after spawn is worse than a
// 400 at save time.
func (c *Catalog) effortOK(modelID, level string) bool {
	if level == "" {
		return true
	}
	m := c.Find(modelID)
	if m == nil {
		// Unknown model: accept the shape and let the CLI decide, rather
		// than blocking a session over a stale catalog.
		return true
	}
	for _, e := range m.Efforts {
		if e == level {
			return true
		}
	}
	return false
}

// EffortOK is the exported check used by the API layer.
func EffortOK(cat *Catalog, modelID, level string) bool {
	if cat == nil {
		return true
	}
	return cat.effortOK(modelID, level)
}

func fileSig(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return fmt.Sprintf("%d:%d", st.ModTime().UnixNano(), st.Size())
}

// sortStrings sorts in place; a tiny helper so this file needs no extra
// import for the handful of small slices it sorts.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
