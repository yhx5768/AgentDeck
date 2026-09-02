package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const minimalYaml = `
clis:
  - name: claude
    path: claude
providers:
  - name: default
    models:
      - name: opus
`

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestManagerReloadSwapsModels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, minimalYaml)
	boot, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(path, boot)

	writeFile(t, path, minimalYaml+`      - name: sonnet
`)
	cfg, err := m.Reload()
	if err != nil {
		t.Fatal(err)
	}
	prov := cfg.ProviderByName("default")
	if prov == nil || prov.ModelByName("sonnet") == nil {
		t.Fatalf("sonnet not present after reload: %+v", cfg.Providers)
	}
}

func TestManagerReloadKeepsStartupFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, `
gateway_token: tok1
listen: ":9000"
data_dir: /tmp/boot-data
`+minimalYaml)
	boot, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(path, boot)

	// File without a token (would regenerate one) and different listen/data_dir.
	writeFile(t, path, minimalYaml+`
listen: ":9001"
data_dir: /tmp/other-data
`)
	cfg, err := m.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GatewayToken != "tok1" {
		t.Fatalf("token changed: %q", cfg.GatewayToken)
	}
	if cfg.Listen != ":9000" || cfg.DataDir != "/tmp/boot-data" {
		t.Fatalf("startup fields not preserved: listen=%q data_dir=%q", cfg.Listen, cfg.DataDir)
	}
}

func TestManagerReloadTokenInFileTakesEffect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, minimalYaml)
	boot, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(path, boot)
	if !boot.TokenGenerated {
		t.Fatal("expected a generated boot token")
	}

	writeFile(t, path, "gateway_token: explicit\n"+minimalYaml)
	cfg, err := m.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GatewayToken != "explicit" {
		t.Fatalf("explicit token not honored: %q", cfg.GatewayToken)
	}
}

func TestManagerReloadParseErrorKeepsOld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, `gateway_token: tok1
`+minimalYaml)
	boot, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(path, boot)

	writeFile(t, path, "gateway_token: [unclosed")
	if _, err := m.Reload(); err == nil {
		t.Fatal("expected parse error")
	}
	if got := m.Get(); got.GatewayToken != "tok1" {
		t.Fatalf("old config not kept after parse error: %q", got.GatewayToken)
	}
}

func TestManagerWatchReloadsOnChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, minimalYaml)
	boot, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(path, boot)

	changed := make(chan struct{}, 1)
	go m.Watch(20*time.Millisecond, func() { changed <- struct{}{} })

	time.Sleep(40 * time.Millisecond) // let the watch baseline the initial file
	writeFile(t, path, `
clis:
  - name: claude
    path: claude
providers:
  - name: default
    models:
      - name: haiku
`)
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not call onChange")
	}
	if cfg := m.Get(); cfg.ProviderByName("default").ModelByName("haiku") == nil {
		t.Fatal("config not reloaded after file change")
	}
}
