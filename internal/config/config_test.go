package config

import "testing"

func TestMainAgentAutocreateDefault(t *testing.T) {
	if (&Config{}).MainAgentAutocreateEnabled() {
		t.Fatal("unset autocreate should default off")
	}
	on := true
	cfg := &Config{MainAgent: &MainAgentConfig{Autocreate: &on}}
	if !cfg.MainAgentAutocreateEnabled() {
		t.Fatal("autocreate=true should enable")
	}
	off := false
	cfg.MainAgent.Autocreate = &off
	if cfg.MainAgentAutocreateEnabled() {
		t.Fatal("autocreate=false should disable")
	}
}

func TestDefaultUIMode(t *testing.T) {
	if got := (&Config{}).DefaultUIMode(); got != "agent" {
		t.Fatalf("empty config default_ui = %q, want agent", got)
	}
	cfg := &Config{MainAgent: &MainAgentConfig{DefaultUI: "chat"}}
	if got := cfg.DefaultUIMode(); got != "chat" {
		t.Fatalf("default_ui = %q, want chat", got)
	}
	cfg.MainAgent.DefaultUI = "nonsense"
	if got := cfg.DefaultUIMode(); got != "agent" {
		t.Fatalf("invalid default_ui = %q, want agent fallback", got)
	}
}
