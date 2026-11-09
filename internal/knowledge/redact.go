package knowledge

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// Finding is one sensitive-looking span detected before storage. Type is a
// short label safe to log and show in the UI; Match is never persisted.
type Finding struct {
	Type  string
	Match string
}

var sensitivePatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"private-key", regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----`)},
	{"bearer", regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._\-]{20,}`)},
	{"credential", regexp.MustCompile(`(?i)\b(api[_-]?key|secret|token|password|passwd|pwd)\s*[:=]\s*[^\s,;]{8,}`)},
	{"cookie", regexp.MustCompile(`(?i)\b(set-)?cookie\s*:\s*\S{20,}`)},
	{"token-prefix", regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_\-]{16,}|ghp_[A-Za-z0-9]{30,}|AKID[A-Za-z0-9]{13,}|xox[baprs]-[A-Za-z0-9\-]{10,})`)},
	// No slash/dot: URLs and file paths are not credentials. The mixed-case +
	// digit requirement in highEntropyWorthy filters commit SHAs, task ids and
	// lowercase slugs, which are common in board timelines.
	{"high-entropy", regexp.MustCompile(`[A-Za-z0-9+=_-]{32,128}`)},
}

// ScanSensitive returns findings for credential-like content. The high-entropy
// rule is intentionally conservative: only strings above the entropy threshold
// count, so ordinary long words/identifiers do not quarantine an entry.
func ScanSensitive(text string) []Finding {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []Finding
	seen := map[string]bool{}
	add := func(typ, match string) {
		key := typ + "\x00" + match
		if match == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, Finding{Type: typ, Match: match})
	}
	for _, p := range sensitivePatterns {
		for _, match := range p.re.FindAllString(text, -1) {
			if p.name == "high-entropy" && !highEntropyWorthy(match) {
				continue
			}
			add(p.name, match)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Match) != len(out[j].Match) {
			return len(out[i].Match) > len(out[j].Match)
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// RedactSensitive replaces every finding with [REDACTED:<type>] and returns the
// redacted text. Overlapping/longest matches are handled first.
func RedactSensitive(text string) (string, []Finding) {
	findings := ScanSensitive(text)
	if len(findings) == 0 {
		return text, nil
	}
	out := text
	for _, f := range findings {
		out = strings.ReplaceAll(out, f.Match, "[REDACTED:"+f.Type+"]")
	}
	return out, findings
}

func findingTypes(findings []Finding) string {
	if len(findings) == 0 {
		return ""
	}
	seen := map[string]bool{}
	var types []string
	for _, f := range findings {
		if !seen[f.Type] {
			seen[f.Type] = true
			types = append(types, f.Type)
		}
	}
	sort.Strings(types)
	return strings.Join(types, ",")
}

func highEntropyWorthy(s string) bool {
	if len(s) < 32 || len(s) > 128 {
		return false
	}
	var upper, lower, digit bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= '0' && r <= '9':
			digit = true
		}
	}
	if !upper || !lower || !digit {
		return false
	}
	return shannonEntropy(s) >= 4.3
}

func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	counts := map[rune]int{}
	for _, r := range s {
		counts[r]++
	}
	total := float64(len([]rune(s)))
	var entropy float64
	for _, n := range counts {
		p := float64(n) / total
		entropy -= p * math.Log2(p)
	}
	return entropy
}
