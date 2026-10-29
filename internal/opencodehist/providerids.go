package opencodehist

import "strings"

// ProviderIDs lists the provider ids the CLI currently exposes (logged-in
// integrations plus built-ins) from the plain `opencode models` output, one
// "<providerID>/<modelID>" line per model. Auto-provider discovery uses it so a
// newly authenticated provider shows up without a config.yaml entry.
//
// This is the v1 surface. v2 dropped the plain-text list and is served from
// `opencode api model.list --standalone` by opencodev2.ProviderIDs.
func ProviderIDs(cliPath string) ([]string, error) {
	raw, err := runCLI(cliPath, "models")
	if err != nil {
		return nil, err
	}
	return ParseProviderIDs(string(raw)), nil
}

// ParseProviderIDs extracts distinct provider ids from `opencode models`
// output, preserving first-appearance order. Lines without a plausible
// "<id>/" prefix (blank lines, help text, error messages) are ignored: the
// command also prints those when it cannot run, and treating them as provider
// ids would put junk in the picker.
func ParseProviderIDs(out string) []string {
	seen := map[string]bool{}
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		i := strings.IndexByte(line, '/')
		if i <= 0 {
			continue
		}
		id := line[:i]
		if !plausibleProviderID(id) || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// plausibleProviderID accepts the character set opencode uses for provider ids
// (letters, digits, dash, underscore, dot) so output lines like
// "https://example.com" or "error: failed to load /x" cannot slip through.
func plausibleProviderID(id string) bool {
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return id != ""
}
