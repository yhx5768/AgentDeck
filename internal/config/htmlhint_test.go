package config

import (
	"path/filepath"
	"testing"
)

// TestHTMLRenderHintTriState pins the yaml tag and the tri-state semantics.
// The tag name is the whole contract with config.yaml — renaming it would
// leave every existing `html_render_hint: false` silently reading back as
// "unset", i.e. the hint stays on and the user's opt-out is a no-op.
func TestHTMLRenderHintTriState(t *testing.T) {
	load := func(t *testing.T, yaml string) *Config {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		writeFile(t, path, minimalYaml+yaml)
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	t.Run("unset", func(t *testing.T) {
		c := load(t, "\n")
		if c.HTMLRenderHint != nil {
			t.Fatalf("want nil pointer, got %v", *c.HTMLRenderHint)
		}
		if !c.HTMLHintEnabled() {
			t.Fatal("unset must mean enabled")
		}
	})

	t.Run("false", func(t *testing.T) {
		c := load(t, "html_render_hint: false\n")
		if c.HTMLRenderHint == nil || *c.HTMLRenderHint {
			t.Fatalf("want pointer to false, got %v", c.HTMLRenderHint)
		}
		if c.HTMLHintEnabled() {
			t.Fatal("false must mean disabled")
		}
	})

	t.Run("true", func(t *testing.T) {
		c := load(t, "html_render_hint: true\n")
		if c.HTMLRenderHint == nil || !*c.HTMLRenderHint {
			t.Fatalf("want pointer to true, got %v", c.HTMLRenderHint)
		}
		if !c.HTMLHintEnabled() {
			t.Fatal("true must mean enabled")
		}
	})

	t.Run("nil receiver", func(t *testing.T) {
		var c *Config
		if !c.HTMLHintEnabled() {
			t.Fatal("nil Config must not panic and must mean enabled")
		}
	})
}
