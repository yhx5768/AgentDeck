package opencodehist

import (
	"encoding/json"
	"sort"
	"strings"
)

// ModelVariants returns, for every model of one provider, the --variant keys
// `opencode run` accepts for it. Variants are opencode's reasoning-depth
// channel (the CLI flag help calls them "provider-specific reasoning effort,
// e.g. high, max, minimal") and the set is per model, not per provider —
// deepseek-v4-flash offers [low high max], gpt-5.6-luna [none low medium high
// xhigh max], kimi-k3 only [max], minimax-m3 [none thinking], and many models
// carry no variants at all (no reasoning depth control).
//
// Source is `opencode models <provider> --verbose`, whose stdout interleaves
// one bare "providerID/modelID" line per model with that model's JSON metadata
// block. Parsing is best-effort: a block that does not parse is skipped, an
// empty result with nil error just means no model carried a variants map.
func ModelVariants(cliPath, providerID string) (map[string][]string, error) {
	raw, err := runCLI(cliPath, "models", providerID, "--verbose")
	if err != nil {
		return nil, err
	}
	return parseModelsOutput(raw), nil
}

// parseModelsOutput is the pure parser behind ModelVariants (split out for
// tests). Models without a variants map are absent from the result.
func parseModelsOutput(raw []byte) map[string][]string {
	out := map[string][]string{}
	var cur string
	var buf []string
	flush := func() {
		if cur == "" || len(buf) == 0 {
			return
		}
		var m struct {
			Variants map[string]map[string]any `json:"variants"`
		}
		if err := json.Unmarshal([]byte(strings.Join(buf, "\n")), &m); err != nil {
			return // layout drift: skip the block, keep the rest
		}
		keys := make([]string, 0, len(m.Variants))
		for k := range m.Variants {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return variantRank(keys[i]) < variantRank(keys[j]) })
		if len(keys) > 0 {
			out[cur] = keys
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
