package server

import (
	"reflect"
	"testing"

	"cc-webui/internal/config"
)

func TestMergeOpenCodeProviders(t *testing.T) {
	cfg := &config.Config{
		CLIs: []config.CLI{
			{Name: "oc", Kind: config.KindOpenCode},
			{Name: "claude", Kind: config.KindClaude},
			{Name: "oc2", Kind: config.KindOpenCode},
		},
		Providers: []config.Provider{
			{Name: "opencode-go", Kind: config.KindOpenCode},
		},
	}
	perCLI := map[string][]string{
		"oc":     {"cline-pass", "opencode-go", "cline-pass"},
		"oc2":    {"cline-pass", "ollama"},
		"claude": {"ignored"}, // not an opencode cli: its map entry is ignored
	}
	got := mergeOpenCodeProviders(cfg, perCLI)
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
		if p.Kind != config.KindOpenCode {
			t.Errorf("provider %q kind = %q, want opencode", p.Name, p.Kind)
		}
	}
	want := []string{"cline-pass", "ollama"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("discovered providers = %v, want %v", names, want)
	}
	if mergeOpenCodeProviders(nil, perCLI) != nil {
		t.Fatal("nil config must yield no providers")
	}
}

func TestMergeOpenCodeProvidersDeclaredWins(t *testing.T) {
	cfg := &config.Config{
		CLIs:      []config.CLI{{Name: "oc", Kind: config.KindOpenCode}},
		Providers: []config.Provider{{Name: "opencode-go", Kind: config.KindOpenCode}},
	}
	got := mergeOpenCodeProviders(cfg, map[string][]string{"oc": {"opencode-go"}})
	if len(got) != 0 {
		t.Fatalf("declared provider leaked into discovered list: %+v", got)
	}
}
