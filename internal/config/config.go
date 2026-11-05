package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

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
	ModelProvider string `yaml:"model_provider" json:"model_provider,omitempty"`
	// ModelsCommand, when set, makes the client's model list come from running
	// the CLI with these arguments instead of the static provider list. Both
	// Baidu launchers print "Available Models:" followed by a comma list
	// (`ducc models`, `ducx models`); the gateway catalogue changes with the
	// account and CLI build, so a static list goes stale (ducc recently
	// stopped recognizing the [1m] aliases it used to accept).
	ModelsCommand []string `yaml:"models_command" json:"models_command,omitempty"`
	// OpenCodeAPI forces which opencode protocol generation this binary speaks
	// ("auto" / "v1" / "v2"). Empty means auto-detect by running
	// `<path> --version`; v2 is a breaking rewrite with a different server API
	// and CLI surface, so the version has to be known before any call.
	OpenCodeAPI string            `yaml:"opencode_api" json:"opencode_api,omitempty"`
	Env         map[string]string `yaml:"env" json:"-"` // never serialized to API clients
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

// BrowserConfig controls the optional browser runtime. Enabled is a pointer so
// an unset value means off; the runtime setting written by the Settings toggle
// can override it without rewriting config.yaml.
type BrowserConfig struct {
	Enabled    *bool  `yaml:"enabled" json:"enabled,omitempty"`
	ChromePath string `yaml:"chrome_path" json:"chrome_path,omitempty"`
	// ProfileKey, when set, makes every AgentDeck session use one stable Chrome
	// profile (for example "me"), so a login survives switching sessions. Empty
	// keeps the default per-session profile. Only one browser may use a shared
	// profile at a time.
	ProfileKey  string `yaml:"profile_key" json:"profile_key,omitempty"`
	MaxBrowsers int    `yaml:"max_browsers" json:"max_browsers,omitempty"`
	// MaxWindows limits sessions sharing one profile (one target/window each).
	// It only applies when the shared-profile switch is on; MaxBrowsers still
	// limits independent Chrome processes in per-session mode.
	MaxWindows  int      `yaml:"max_windows" json:"max_windows,omitempty"`
	IdleMinutes int      `yaml:"idle_minutes" json:"idle_minutes,omitempty"`
	FPS         int      `yaml:"fps" json:"fps,omitempty"`
	Quality     int      `yaml:"quality" json:"quality,omitempty"`
	AllowLocal  bool     `yaml:"allow_local" json:"allow_local,omitempty"`
	AllowHosts  []string `yaml:"allow_hosts" json:"allow_hosts,omitempty"`
	// TrustedOrigins lists extra origins (scheme://host[:port]) allowed to
	// post browser input. This is for deployments behind a reverse proxy that
	// does not preserve Host or set X-Forwarded-Host/Proto. Prefer fixing the
	// proxy; keep this empty otherwise.
	TrustedOrigins []string `yaml:"trusted_origins" json:"trusted_origins,omitempty"`
	// TrustForwardedHeaders makes the browser input check trust
	// X-Forwarded-Host/Proto from a loopback peer. Enable only when the local
	// proxy overwrites those headers; nginx's default configuration forwards
	// client-supplied values, which would make this spoofable.
	TrustForwardedHeaders bool `yaml:"trust_forwarded_headers" json:"trust_forwarded_headers,omitempty"`
}

// BrowserOn reports the config-file default. The live switch is owned by the
// browser manager, which persists a runtime override under data/browser.
func (c *Config) BrowserOn() bool {
	return c != nil && c.Browser.Enabled != nil && *c.Browser.Enabled
}

// Config is the top-level config structure.
type Config struct {
	Listen       string `yaml:"listen"`
	GatewayToken string `yaml:"gateway_token"`
	DataDir      string `yaml:"data_dir"`
	DefaultCwd   string `yaml:"default_cwd"`
	// AgentBoardMockBusy turns the board drawer composer into a queue test
	// harness: the UI keeps messages in the local pending queue instead of
	// posting them. Test previews set this; production leaves it false.
	AgentBoardMockBusy bool          `yaml:"agent_board_mock_busy"`
	Browser            BrowserConfig `yaml:"browser" json:"browser"`
	CLIs               []CLI         `yaml:"clis"`
	Providers          []Provider    `yaml:"providers"`

	// DiscoveredProviders are runtime entries for engines whose providers live
	// outside config.yaml — currently the provider ids an opencode cli reports
	// as logged in/configured. The server's discovery loop fills this in, the
	// manager carries it across hot reloads, and a config-declared provider of
	// the same name shadows the discovered one. Never read from the file.
	DiscoveredProviders []Provider `yaml:"-" json:"-"`

	// HTMLRenderHint controls whether cc-webui appends its own "this UI can
	// render a whole HTML page" instruction to every turn's system prompt.
	// nil (unset) means ON — it is what enables the feature, so defaulting it
	// off would silently disable it on every existing config. Set it to false to
	// opt out. See runner/htmlhint.go for the text and the per-engine coverage.
	HTMLRenderHint *bool `yaml:"html_render_hint"`

	// AgentdeckMCP controls whether cc-webui injects its agentdeck MCP server
	// (list_sessions / send_message / new_session / get_session / stop_session)
	// into every engine's tool list, so any engine's agent can drive AgentDeck
	// and talk to peer sessions over the A2A bus. nil (unset) means ON. Set it
	// to false to opt out (e.g. to keep every session's tool list minimal).
	AgentdeckMCP *bool `yaml:"agentdeck_mcp"`

	// MainAgent configures the preloaded main-agent role prompt and the default
	// worker profile that prompt interpolates. nil = built-in defaults.
	MainAgent *MainAgentConfig `yaml:"main_agent" json:"main_agent,omitempty"`

	// TokenGenerated is true when the token was auto-generated at startup.
	TokenGenerated bool `yaml:"-" json:"-"`
}

// MainAgentConfig is the `main_agent` section of config.yaml.
type MainAgentConfig struct {
	// Worker is the default profile the main agent uses when it creates a
	// worker via new_session. Its values are interpolated into the built-in
	// role prompt so the model knows the house default; the model can still
	// override any field per task.
	Worker WorkerDefaults `yaml:"worker" json:"worker"`
	// Prompt replaces the whole built-in role prompt when non-empty. Leave it
	// unset to use the built-in template (with Worker interpolated).
	Prompt string `yaml:"prompt" json:"prompt,omitempty"`
	// Notify tunes the runtime's worker notifications to the main agent
	// (incidents: a worker turn errored or stalled; escalations: a worker parked
	// on a human-only gate). See internal/server/incidents.go.
	Notify NotifyConfig `yaml:"notify" json:"notify"`
}

// NotifyConfig is the `main_agent.notify` section of config.yaml: the policy for
// the runtime's worker-to-main-agent notifications.
type NotifyConfig struct {
	// Enabled turns the notifications on/off. nil (unset) means ON — without a
	// main agent nothing is notified anyway, so opting in per-session is enough.
	Enabled *bool `yaml:"enabled" json:"enabled,omitempty"`
	// StallMinutes is how long a running turn may produce no output at all
	// before it counts as stalled. Defaults to 15 (a huge-context first token
	// can take a few minutes; total silence for 15 is a hang).
	StallMinutes int `yaml:"stall_minutes" json:"stall_minutes,omitempty"`
	// WorkerCooldownMinutes is the minimum gap between two notifications for the
	// same worker, so a crash loop cannot become a notification storm. Defaults
	// to 5.
	WorkerCooldownMinutes int `yaml:"worker_cooldown_minutes" json:"worker_cooldown_minutes,omitempty"`
	// HourlyCap is the global ceiling of notifications per hour. Defaults to 20.
	HourlyCap int `yaml:"hourly_cap" json:"hourly_cap,omitempty"`
}

// NotifySettings is the normalized worker-notification policy the runtime and
// the server consume (defaults applied).
type NotifySettings struct {
	Enabled        bool
	StallTimeout   time.Duration
	WorkerCooldown time.Duration
	HourlyCap      int
}

// Notify returns the normalized notification policy. It works with or without a
// `main_agent` section: the defaults are the policy, and a session can be marked
// main agent at runtime (the UI toggle) without any config entry.
func (c *Config) Notify() NotifySettings {
	s := NotifySettings{
		Enabled:        true,
		StallTimeout:   15 * time.Minute,
		WorkerCooldown: 5 * time.Minute,
		HourlyCap:      20,
	}
	if c != nil && c.MainAgent != nil {
		n := c.MainAgent.Notify
		if n.Enabled != nil {
			s.Enabled = *n.Enabled
		}
		if n.StallMinutes > 0 {
			s.StallTimeout = time.Duration(n.StallMinutes) * time.Minute
		}
		if n.WorkerCooldownMinutes > 0 {
			s.WorkerCooldown = time.Duration(n.WorkerCooldownMinutes) * time.Minute
		}
		if n.HourlyCap > 0 {
			s.HourlyCap = n.HourlyCap
		}
	}
	return s
}

// WorkerDefaults is the default profile for a new worker session.
type WorkerDefaults struct {
	CLI      string `yaml:"cli" json:"cli,omitempty"`
	Provider string `yaml:"provider" json:"provider,omitempty"`
	Model    string `yaml:"model" json:"model,omitempty"`
	Effort   string `yaml:"effort" json:"effort,omitempty"`
	// Resident keeps a long-lived process online (claude only). nil = unset
	// (the prompt then omits the 常驻/非常驻 hint).
	Resident *bool `yaml:"resident" json:"resident,omitempty"`
}

// AgentdeckMCPEnabled reports whether the agentdeck MCP server is injected.
// Unset means enabled — see the AgentdeckMCP doc comment.
func (c *Config) AgentdeckMCPEnabled() bool {
	return c == nil || c.AgentdeckMCP == nil || *c.AgentdeckMCP
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
	if cfg.Browser.MaxBrowsers <= 0 {
		cfg.Browser.MaxBrowsers = 2
	}
	if cfg.Browser.MaxBrowsers > 4 {
		cfg.Browser.MaxBrowsers = 4
	}
	if cfg.Browser.MaxWindows <= 0 {
		cfg.Browser.MaxWindows = 4
	}
	if cfg.Browser.MaxWindows > 8 {
		cfg.Browser.MaxWindows = 8
	}
	if cfg.Browser.IdleMinutes <= 0 {
		cfg.Browser.IdleMinutes = 10
	}
	if cfg.Browser.IdleMinutes > 60 {
		cfg.Browser.IdleMinutes = 60
	}
	if cfg.Browser.FPS <= 0 {
		cfg.Browser.FPS = 8
	}
	if cfg.Browser.FPS > 15 {
		cfg.Browser.FPS = 15
	}
	if cfg.Browser.Quality <= 0 {
		cfg.Browser.Quality = 60
	}
	if cfg.Browser.Quality < 20 {
		cfg.Browser.Quality = 20
	}
	if cfg.Browser.Quality > 90 {
		cfg.Browser.Quality = 90
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
	if c == nil {
		return nil
	}
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i]
		}
	}
	// Runtime-discovered providers (opencode's logged-in integrations) come
	// after the declared ones: config always wins on a name collision.
	for i := range c.DiscoveredProviders {
		if c.DiscoveredProviders[i].Name == name {
			return &c.DiscoveredProviders[i]
		}
	}
	return nil
}

// AllProviders returns config-declared providers followed by runtime-discovered
// ones. Provider pickers render this list; ProviderByName already consults both.
// A discovered entry whose name is also declared is skipped — config wins, and
// showing both would duplicate the row (a reload can leave such an entry in the
// manager's discovered set until the next discovery pass).
func (c *Config) AllProviders() []Provider {
	if c == nil {
		return nil
	}
	out := make([]Provider, 0, len(c.Providers)+len(c.DiscoveredProviders))
	out = append(out, c.Providers...)
	if len(c.DiscoveredProviders) == 0 {
		return out
	}
	declared := make(map[string]bool, len(c.Providers))
	for i := range c.Providers {
		declared[c.Providers[i].Name] = true
	}
	for i := range c.DiscoveredProviders {
		if !declared[c.DiscoveredProviders[i].Name] {
			out = append(out, c.DiscoveredProviders[i])
		}
	}
	return out
}
