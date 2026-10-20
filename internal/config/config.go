package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Engine kinds for clis and providers. A session's engine is the kind of its
// cli, and cli/provider pairs must share a kind (validated on create/patch/
// import). kind "claude" covers Claude Code and compatible CLIs (stream-json
// stdout + ~/.claude jsonl transcript); kind "opencode" is the opencode
// engine (`opencode serve` events + `opencode export` transcript); kind
// "mcode" is MiniMax Code (exec --output-format stream-json stdout +
// ~/.minimax messages.jsonl transcript); kind "qoder" is Qoder CLI CN, a
// Claude Code fork that speaks the same stream-json protocol and writes the
// same transcript layout under ~/.qoder-cn; kind "codex" is the Codex CLI
// (codex exec --json stdout + $CODEX_HOME/sessions rollout files), which also
// covers wrappers such as Baidu's ducx.
const (
	KindClaude   = "claude"
	KindOpenCode = "opencode"
	KindMcode    = "mcode"
	KindQoder    = "qoder"
	KindCodex    = "codex"
)

// CLI describes one chat CLI binary.
type CLI struct {
	Name           string   `yaml:"name" json:"name"`
	Kind           string   `yaml:"kind" json:"kind"` // engine kind, defaults to KindClaude
	Path           string   `yaml:"path" json:"path"`
	Args           []string `yaml:"args" json:"args"`
	PermissionMode string   `yaml:"permission_mode" json:"permission_mode"`
	// PartialMessages passes --include-partial-messages to the CLI, enabling
	// token-level stream_event deltas in the output stream. Only enable for
	// recent Claude Code builds; compatible CLIs may not recognize the flag.
	PartialMessages bool `yaml:"partial_messages" json:"partial_messages"`
	// Home relocates a per-installation CLI home for engines that read their own
	// transcript from a fixed directory. Codex uses it as CODEX_HOME (Baidu's
	// ducx wrapper writes to ~/.baidu-cx and ignores the env var, so it must be
	// declared here); other engines ignore it.
	Home string `yaml:"home" json:"home,omitempty"`
	// ModelProvider is the codex `model_provider` id to pin when a config
	// override is passed. It matters for launcher wrappers: any `-c` flag makes
	// ducx drop the provider it would otherwise inject, so the id has to be
	// repeated. Standard codex with its own config.toml can leave it empty.
	ModelProvider string            `yaml:"model_provider" json:"model_provider,omitempty"`
	Env           map[string]string `yaml:"env" json:"-"` // never serialized to API clients
}

// Model is one model within a provider, with optional per-model env and
// declared input modalities ("text", "image"). An empty Input is treated
// as text-only by the UI.
type Model struct {
	Name  string            `yaml:"name" json:"name"`
	Env   map[string]string `yaml:"env" json:"-"` // never serialized to API clients
	Input []string          `yaml:"input" json:"input"`
}

// Provider groups a model backend and its selectable models. For claude-kind
// clis the backend is an Anthropic-compatible API (env = BASE_URL/TOKEN); for
// opencode-kind clis the provider name must equal the opencode CLI's provider
// id (auth comes from opencode's own auth.json, env is not consulted).
type Provider struct {
	Name   string            `yaml:"name" json:"name"`
	Kind   string            `yaml:"kind" json:"kind"` // engine kind, defaults to KindClaude
	Env    map[string]string `yaml:"env" json:"-"`     // shared by all models
	Models []Model           `yaml:"models" json:"-"`
}

// IsClaude reports whether this cli speaks the Claude Code protocol — the
// default kind, and the only one carrying the rewind/resident protocol surface.
func (c *CLI) IsClaude() bool { return c.Kind == KindClaude }

// IsOpenCode reports whether this cli speaks the opencode protocol.
func (c *CLI) IsOpenCode() bool { return c.Kind == KindOpenCode }

// IsOpenCode mirrors CLI.IsOpenCode for providers.
func (p *Provider) IsOpenCode() bool { return p.Kind == KindOpenCode }

// IsMcode reports whether this cli speaks the mcode protocol.
func (c *CLI) IsMcode() bool { return c.Kind == KindMcode }

// IsMcode mirrors CLI.IsMcode for providers.
func (p *Provider) IsMcode() bool { return p.Kind == KindMcode }

// IsQoder reports whether this cli speaks the Qoder CLI CN protocol.
func (c *CLI) IsQoder() bool { return c.Kind == KindQoder }

// IsQoder mirrors CLI.IsQoder for providers.
func (p *Provider) IsQoder() bool { return p.Kind == KindQoder }

// IsCodex reports whether this cli speaks the Codex CLI protocol.
func (c *CLI) IsCodex() bool { return c.Kind == KindCodex }

// IsCodex mirrors CLI.IsCodex for providers.
func (p *Provider) IsCodex() bool { return p.Kind == KindCodex }

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
//
// The model itself is NOT injected here, however empty the result: a model name
// only reaches the endpoint through vars the config declares. Filling in
// ANTHROPIC_MODEL from the session's model looks tidy but breaks the launcher
// CLIs — ducc canonicalizes the --model flag into its own settings ("Claude
// Sonnet 5" -> "Claude Sonnet 5[1m]", plus that model's context window and
// auth), and our settings file is merged last, so an env-pinned name would win
// with the raw, uncanonicalized string (verified: env-pinned model with no
// --model stalls, --model works). Providers whose endpoint serves one model
// declare these vars per model in config.yaml; that is the supported way.
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

	// HTMLRenderHint controls whether cc-webui appends its own "this UI can
	// render a whole HTML page" instruction to every turn's system prompt.
	// nil (unset) means ON — it is what enables the feature, so defaulting it
	// off would silently disable it on every existing config. Set it to false to
	// opt out. See runner/htmlhint.go for the text and the per-engine coverage.
	HTMLRenderHint *bool `yaml:"html_render_hint"`

	// TokenGenerated is true when the token was auto-generated at startup.
	TokenGenerated bool `yaml:"-" json:"-"`
}

// HTMLHintEnabled reports whether the HTML render hint should be injected.
// Unset means enabled — see the HTMLRenderHint doc comment.
func (c *Config) HTMLHintEnabled() bool {
	return c == nil || c.HTMLRenderHint == nil || *c.HTMLRenderHint
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
		if cfg.CLIs[i].Kind == "" {
			cfg.CLIs[i].Kind = KindClaude
		}
		if cfg.CLIs[i].PermissionMode == "" {
			cfg.CLIs[i].PermissionMode = "bypassPermissions"
		}
	}
	for i := range cfg.Providers {
		if cfg.Providers[i].Kind == "" {
			cfg.Providers[i].Kind = KindClaude
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
