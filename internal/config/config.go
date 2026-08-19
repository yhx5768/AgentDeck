package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// CLI describes one claude-compatible binary.
type CLI struct {
	Name           string            `yaml:"name" json:"name"`
	Path           string            `yaml:"path" json:"path"`
	Args           []string          `yaml:"args" json:"args"`
	PermissionMode string            `yaml:"permission_mode" json:"permission_mode"`
	// PartialMessages passes --include-partial-messages to the CLI, enabling
	// token-level stream_event deltas in the output stream. Only enable for
	// recent Claude Code builds; compatible CLIs may not recognize the flag.
	PartialMessages bool              `yaml:"partial_messages" json:"partial_messages"`
	Env            map[string]string `yaml:"env" json:"-"` // never serialized to API clients
}

// Model is one model within a provider, with optional per-model env and
// declared input modalities ("text", "image"). An empty Input is treated
// as text-only by the UI.
type Model struct {
	Name  string            `yaml:"name" json:"name"`
	Env   map[string]string `yaml:"env" json:"-"` // never serialized to API clients
	Input []string          `yaml:"input" json:"input"`
}

// Provider groups an API backend (BASE_URL/TOKEN) and its selectable models.
type Provider struct {
	Name   string            `yaml:"name" json:"name"`
	Env    map[string]string `yaml:"env" json:"-"` // shared by all models
	Models []Model           `yaml:"models" json:"-"`
}

// ModelByName returns the model with the given name, or nil.
func (p *Provider) ModelByName(name string) *Model {
	for i := range p.Models {
		if p.Models[i].Name == name {
			return &p.Models[i]
		}
	}
	return nil
}

// DefaultModelName is used when a session has no explicit model selection.
func (p *Provider) DefaultModelName() string {
	if len(p.Models) > 0 {
		return p.Models[0].Name
	}
	return ""
}

// EffectiveEnv merges cli.env → provider.env → model.env (later wins).
func (c *CLI) EffectiveEnv(p *Provider, modelName string) map[string]string {
	merged := map[string]string{}
	for k, v := range c.Env {
		merged[k] = v
	}
	if p != nil {
		for k, v := range p.Env {
			merged[k] = v
		}
		if m := p.ModelByName(modelName); m != nil {
			for k, v := range m.Env {
				merged[k] = v
			}
		}
	}
	return merged
}

// EffectiveModel resolves a session's override to a concrete name.
func (p *Provider) EffectiveModel(override string) string {
	if override != "" {
		return override
	}
	return p.DefaultModelName()
}

// Config is the top-level config structure.
type Config struct {
	Listen       string     `yaml:"listen"`
	GatewayToken string     `yaml:"gateway_token"`
	DataDir      string     `yaml:"data_dir"`
	DefaultCwd   string     `yaml:"default_cwd"`
	CLIs         []CLI      `yaml:"clis"`
	Providers    []Provider `yaml:"providers"`

	// TokenGenerated is true when the token was auto-generated at startup.
	TokenGenerated bool `yaml:"-" json:"-"`
}

// DefaultPath returns the first config path that exists, or ./config.yaml.
func DefaultPath() string {
	candidates := []string{
		"config.yaml",
		filepath.Join(os.Getenv("HOME"), ".config", "cc-webui", "config.yaml"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return candidates[0]
}

// Load reads the config file, applies defaults and env overrides.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	if cfg.Listen == "" {
		cfg.Listen = ":8321"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "data"
	}
	if abs, err := filepath.Abs(cfg.DataDir); err == nil {
		cfg.DataDir = abs
	}
	if cfg.DefaultCwd == "" {
		cfg.DefaultCwd, _ = os.UserHomeDir()
	}

	if t := os.Getenv("GATEWAY_TOKEN"); t != "" {
		cfg.GatewayToken = t
	}
	if v := os.Getenv("CC_WEBUI_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("CC_WEBUI_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}

	if cfg.GatewayToken == "" {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		cfg.GatewayToken = hex.EncodeToString(b)
		cfg.TokenGenerated = true
	}

	if len(cfg.CLIs) == 0 {
		cfg.CLIs = []CLI{{Name: "claude", Path: "claude", PermissionMode: "bypassPermissions"}}
	}
	if len(cfg.Providers) == 0 {
		cfg.Providers = []Provider{{Name: "default", Models: []Model{{Name: "default"}}}}
	}
	for i := range cfg.CLIs {
		if cfg.CLIs[i].PermissionMode == "" {
			cfg.CLIs[i].PermissionMode = "bypassPermissions"
		}
	}
	return cfg, nil
}

func (c *Config) CLIByName(name string) *CLI {
	for i := range c.CLIs {
		if c.CLIs[i].Name == name {
			return &c.CLIs[i]
		}
	}
	return nil
}

func (c *Config) ProviderByName(name string) *Provider {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i]
		}
	}
	return nil
}