package opencodehist

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// errOpencodeNoModels guards against a layout or auth change that parses to
// zero models. Serving that as "this provider has no models" would silently
// empty the picker and make every session of that provider unselectable, so a
// caller must treat it as "the catalog could not be read" and fall back to
// whatever it already had. Same reasoning as the qoder list.
var errOpencodeNoModels = errors.New("opencode models 未返回任何模型（CLI 是否已登录 / providerID 是否正确？）")

// Model is one model of a provider's runtime catalog.
//
// ID is the registry's own "id" field verbatim, NOT the "<providerID>/<id>"
// header line the CLI prints above the JSON. Those two differ whenever the
// model id already carries its provider prefix (cline-pass prints
// "cline-pass/cline-pass/deepseek-v4-pro" whose id is "cline-pass/deepseek-v4-pro"),
// and it is the id that opencode's own session/prompt API is addressed with —
// runner's openCodeModelRef passes config's model name straight through as
// ModelRef.ID. So the id is exactly what cc-webui's config must name.
type Model struct {
	ID    string   `json:"id"`
	Input []string `json:"input"`
}

// Catalog is one provider's runtime catalog: its models in the order the CLI
// printed them (the picker treats the first as the default), plus the variant
// keys per model.
type Catalog struct {
	Models   []Model
	Variants map[string][]string
}

// ProviderCatalog reads one provider's catalog with `opencode models
// <providerID> --verbose`.
//
// refresh adds the CLI's own --refresh, which is the only thing that re-reads
// models.dev: a plain `opencode models` is served from the on-disk cache at
// ~/.cache/opencode/models.json and does not touch the network, so a caller
// that wants genuinely new models has to ask for the refresh explicitly (and
// pay a models.dev round trip). The caller decides the cadence — see the
// server's stale-while-revalidate memo, which serves the cheap cached read on
// the hot path and pays for the refresh in the background.
func ProviderCatalog(cliPath, providerID string, refresh bool) (*Catalog, error) {
	args := []string{"models", providerID, "--verbose"}
	if refresh {
		args = append(args, "--refresh")
	}
	raw, err := runCLI(cliPath, args...)
	if err != nil {
		return nil, err
	}
	cat := parseCatalog(raw)
	if len(cat.Models) == 0 {
		return nil, errOpencodeNoModels
	}
	return cat, nil
}

// ModelVariants returns, for every model of one provider, the --variant keys
// `opencode run` accepts for it. Variants are opencode's reasoning-depth
// channel (the CLI flag help calls them "provider-specific reasoning effort,
// e.g. high, max, minimal") and the set is per model, not per provider —
// deepseek-v4-flash offers [low high max], gpt-5.6-luna [none low medium high
// xhigh max], kimi-k3 only [max], minimax-m3 [none thinking], and many models
// carry no variants at all (no reasoning depth control).
//
// Keys are the "<providerID>/<id>" header line, matching what the opencode
// session view already looks up.
func ModelVariants(cliPath, providerID string) (map[string][]string, error) {
	cat, err := ProviderCatalog(cliPath, providerID, false)
	if err != nil {
		return nil, err
	}
	return cat.Variants, nil
}

// Find reports whether the catalog declares a model under that id, either
// verbatim or as it appears in a "provider/id" qualified name.
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

// parseCatalog is the pure parser behind ProviderCatalog (split out for tests).
//
// `opencode models <p> --verbose` interleaves one bare "providerID/modelID"
// line per model with that model's JSON metadata block. Parsing is
// best-effort: a block that does not parse is skipped so one malformed entry
// cannot cost the whole provider, and the rest still load.
//
// A block lands in Models even when it declares no variants — the model list
// and the variant map are different questions, and a model with no reasoning
// depth is still selectable. It is only absent from Variants, where an empty
// key set means the picker must not offer anything.
func parseCatalog(raw []byte) *Catalog {
	out := &Catalog{Variants: map[string][]string{}}
	var cur string
	var buf []string
	flush := func() {
		if cur == "" || len(buf) == 0 {
			return
		}
		var m struct {
			ID           string `json:"id"`
			Capabilities struct {
				Input struct {
					Text  bool `json:"text"`
					Image bool `json:"image"`
				} `json:"input"`
			} `json:"capabilities"`
			Variants map[string]map[string]any `json:"variants"`
		}
		if err := json.Unmarshal([]byte(strings.Join(buf, "\n")), &m); err != nil {
			return // layout drift: skip the block, keep the rest
		}
		if m.ID == "" {
			return // unnameable model: it could not be selected anyway
		}
		// Input is declared explicitly rather than left empty: "text only" is
		// a real answer the picker acts on (it gates the image attach button),
		// and an empty list would read as "declares nothing".
		input := []string{"text"}
		if m.Capabilities.Input.Image {
			input = []string{"text", "image"}
		}
		out.Models = append(out.Models, Model{ID: m.ID, Input: input})
		keys := make([]string, 0, len(m.Variants))
		for k := range m.Variants {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return variantRank(keys[i]) < variantRank(keys[j]) })
		if len(keys) > 0 {
			out.Variants[cur] = keys
		}
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" && line[0] != ' ' && strings.Contains(line, "/") {
			flush()
			cur = line
			buf = buf[:0]
			continue
		}
		buf = append(buf, line)
	}
	flush()
	return out
}

// variantRank orders variant keys the way the model author listed them, not
// alphabetically: none/off first, then effort low→max, thinking last. Unknown
// keys (provider-specific future values) sort after the known ones.
func variantRank(k string) int {
	switch k {
	case "none", "off", "disabled":
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
	return fmt.Sprintf("opencode catalog: %d models, %d with variants", len(c.Models), len(c.Variants))
}
