package server

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"cc-webui/internal/claudehist"
	"cc-webui/internal/config"
	"cc-webui/internal/mcodehist"
	"cc-webui/internal/opencodehist"
	"cc-webui/internal/runner"
	"cc-webui/internal/store"
)

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// validEffort reports whether s is an accepted --effort level (claude engine).
func validEffort(s string) bool {
	for _, v := range []string{"low", "medium", "high", "xhigh", "max"} {
		if s == v {
			return true
		}
	}
	return false
}

// validQoderEffort reports whether s is an accepted --reasoning-effort level.
// The CLI is the authority and rejects anything else at spawn time with
// "Invalid reasoning effort: <s>. Valid values are: auto, none, low, medium,
// high, xhigh, max, ultracode" (verified on 1.1.65) — this set is that list,
// and /api/qoder-models serves it to the UI so both sides agree.
func validQoderEffort(s string) bool {
	for _, v := range qoderEfforts {
		if s == v {
			return true
		}
	}
	return false
}

// qoderEfforts is the reasoning-level set Qoder CLI CN accepts. It is a fixed
// list on the CLI's side (not per-model metadata like opencode's --variant
// keys), so it lives here as a constant and is served to the frontend by
// /api/qoder-models.
var qoderEfforts = []string{"auto", "none", "low", "medium", "high", "xhigh", "max", "ultracode"}

// effortOKFor validates a session effort/推理 override per engine kind.
// Claude sessions take the fixed --effort level set above; opencode sessions
// take a --variant key, whose values are provider- and model-specific (e.g.
// low/high/max, none/thinking, or nothing at all) — only the shape is checked
// here, the UI offers the actual keys from the model metadata. Qoder takes its
// own fixed --reasoning-effort set (qoderEfforts).
func effortOKFor(kind, s string) bool {
	if s == "" {
		return true // empty = default / clear the override
	}
	switch kind {
	case config.KindOpenCode:
		if s == "" || len(s) > 32 {
			return false
		}
		for _, r := range s {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
				return false
			}
		}
		return true
	case config.KindQoder:
		return validQoderEffort(s)
	}
	return validEffort(s)
}

const cookieName = "ccgw_token"

type Server struct {
	cm     *config.Manager
	st     *store.Store
	mgr    *runner.Manager
	static fs.FS
	mux    *http.ServeMux

	// variantsCache memoizes `opencode models --verbose` per cli+provider.
	// The metadata (reasoning-effort variant keys per model) is static until
	// the user runs `opencode models --refresh`, so a slow TTL suffices.
	variantsMu    sync.Mutex
	variantsCache map[string]variantsEntry
}

type variantsEntry struct {
	ts   time.Time
	data map[string][]string // model id -> --variant keys
}

const variantsTTL = 10 * time.Minute

func New(cm *config.Manager, st *store.Store, mgr *runner.Manager, static fs.FS) *Server {
	s := &Server{cm: cm, st: st, mgr: mgr, static: static, mux: http.NewServeMux(), variantsCache: map[string]variantsEntry{}}
	s.routes()
	return s
}

// cfg returns the live config (hot-reloadable via config.Manager.Watch).
func (s *Server) cfg() *config.Config { return s.cm.Get() }

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("POST /api/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/logout", s.handleLogout)

	// Authenticated API.
	s.mux.Handle("GET /api/me", s.auth(s.handleMe))
	s.mux.Handle("GET /api/profiles", s.auth(s.handleProfiles))
	s.mux.Handle("GET /api/clis", s.auth(s.handleCLIs))
	s.mux.Handle("GET /api/providers", s.auth(s.handleProviders))
	s.mux.Handle("GET /api/config", s.auth(s.handleConfig))
	s.mux.Handle("GET /api/sessions", s.auth(s.handleListSessions))
	s.mux.Handle("POST /api/sessions", s.auth(s.handleCreateSession))
	s.mux.Handle("GET /api/sessions/{id}", s.auth(s.handleGetSession))
	s.mux.Handle("PATCH /api/sessions/{id}", s.auth(s.handlePatchSession))
	s.mux.Handle("DELETE /api/sessions/{id}", s.auth(s.handleDeleteSession))
	s.mux.Handle("POST /api/sessions/{id}/messages", s.auth(s.handlePostMessage))
	s.mux.Handle("POST /api/sessions/{id}/uploads", s.auth(s.handleUpload))
	s.mux.Handle("POST /api/sessions/{id}/stop", s.auth(s.handleStop))
	s.mux.Handle("GET /api/sessions/{id}/events", s.auth(s.handleEvents))
	s.mux.Handle("GET /api/claude-sessions", s.auth(s.handleClaudeSessions))
	s.mux.Handle("GET /api/claude-sessions/preview", s.auth(s.handleClaudeSessionPreview))
	s.mux.Handle("GET /api/opencode-sessions", s.auth(s.handleOpenCodeSessions))
	s.mux.Handle("GET /api/opencode-sessions/preview", s.auth(s.handleOpenCodeSessionPreview))
	s.mux.Handle("GET /api/opencode-model-variants", s.auth(s.handleOpenCodeModelVariants))
	s.mux.Handle("GET /api/mcode-models", s.auth(s.handleMcodeModels))
	s.mux.Handle("GET /api/mcode-sessions", s.auth(s.handleMcodeSessions))
	s.mux.Handle("GET /api/mcode-sessions/preview", s.auth(s.handleMcodeSessionPreview))
	s.mux.Handle("GET /api/qoder-models", s.auth(s.handleQoderModels))
	s.mux.Handle("GET /api/qoder-sessions", s.auth(s.handleQoderSessions))
	s.mux.Handle("GET /api/qoder-sessions/preview", s.auth(s.handleQoderSessionPreview))
	s.mux.Handle("POST /api/sessions/import", s.auth(s.handleImport))
	s.mux.Handle("GET /api/stats", s.auth(s.handleStats))
	s.mux.Handle("GET /api/notes", s.auth(s.handleGetNotes))
	s.mux.Handle("PUT /api/notes", s.auth(s.handlePutNotes))
	s.mux.Handle("POST /api/sessions/{id}/fork", s.auth(s.handleFork))
	s.mux.Handle("POST /api/sessions/{id}/rewind", s.auth(s.handleRewind))
	s.mux.Handle("POST /api/sessions/{id}/permissions/request", s.auth(s.handlePermissionRequest))
	s.mux.Handle("POST /api/sessions/{id}/permissions/{pid}", s.auth(s.handlePermissionResolve))
	s.mux.Handle("GET /api/sessions/{id}/permissions/pending", s.auth(s.handlePermissionPending))
	s.mux.Handle("GET /api/groups", s.auth(s.handleListGroups))
	s.mux.Handle("POST /api/groups", s.auth(s.handleCreateGroup))
	s.mux.Handle("GET /api/groups/{id}", s.auth(s.handleGetGroup))
	s.mux.Handle("DELETE /api/groups/{id}", s.auth(s.handleDeleteGroup))
	s.mux.Handle("POST /api/groups/{id}/messages", s.auth(s.handleGroupMessage))
	s.mux.Handle("POST /api/groups/{id}/stop", s.auth(s.handleGroupStop))
	s.mux.Handle("GET /api/groups/{id}/events", s.auth(s.handleGroupEvents))
	s.mux.Handle("GET /api/files", s.auth(s.handleReadFile))

	// Static frontend (index redirects to login when unauthorized; the SPA
	// also guards itself via /api/me).
	s.mux.Handle("GET /", s.serveStatic())
}

func (s *Server) tokenOK(r *http.Request) bool {
	token := ""
	if c, err := r.Cookie(cookieName); err == nil {
		token = c.Value
	}
	if token == "" {
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimPrefix(h, "Bearer ")
		}
	}
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg().GatewayToken)) == 1
}

func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.tokenOK(r) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	})
}

// sortedKeys returns m's keys sorted. Map iteration order is randomized, and
// /api/clis / /api/providers render env_keys from maps, so sorting keeps the
// payloads byte-stable — the frontend diffs them to detect config hot reloads.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// ---- auth ----

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Token), []byte(s.cfg().GatewayToken)) != 1 {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    body.Token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   30 * 24 * 3600,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- clis / providers / config ----

func (s *Server) handleCLIs(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Name            string   `json:"name"`
		Kind            string   `json:"kind"`
		Path            string   `json:"path"`
		Args            []string `json:"args"`
		PermissionMode  string   `json:"permission_mode"`
		PartialMessages bool     `json:"partial_messages"`
		EnvKeys         []string `json:"env_keys"`
	}
	out := []item{}
	for _, c := range s.cfg().CLIs {
		it := item{Name: c.Name, Kind: c.Kind, Path: c.Path, Args: c.Args, PermissionMode: c.PermissionMode, PartialMessages: c.PartialMessages}
		it.EnvKeys = sortedKeys(c.Env)
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	type model struct {
		Name    string   `json:"name"`
		EnvKeys []string `json:"env_keys"`
		Input   []string `json:"input"`
	}
	type prov struct {
		Name    string   `json:"name"`
		Kind    string   `json:"kind"`
		EnvKeys []string `json:"env_keys"`
		Models  []model  `json:"models"`
	}
	out := []prov{}
	for _, p := range s.cfg().Providers {
		// Pre-initialize to an empty slice, not nil: a nil slice marshals to
		// JSON `null`, and the frontend indexes into this list unguarded
		// (provider.models[0], provider.models.find). A provider with no
		// models in config is normal for the mcode engine — its model list
		// is resolved by the CLI at run time and fetched from
		// /api/mcode-models instead — so an empty `models: []` in config
		// would otherwise hand the UI a null and blank the whole chat view on
		// open.
		item := prov{Name: p.Name, Kind: p.Kind, Models: []model{}}
		item.EnvKeys = sortedKeys(p.Env)
		for _, m := range p.Models {
			mm := model{Name: m.Name, Input: m.Input}
			if mm.Input == nil {
				mm.Input = []string{"text"}
			}
			mm.EnvKeys = sortedKeys(m.Env)
			item.Models = append(item.Models, mm)
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// Legacy alias for older UIs: returns {clis, providers} in one payload.
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	type model struct {
		Name    string   `json:"name"`
		EnvKeys []string `json:"env_keys"`
		Input   []string `json:"input"`
	}
	type prov struct {
		Name    string   `json:"name"`
		Kind    string   `json:"kind"`
		EnvKeys []string `json:"env_keys"`
		Models  []model  `json:"models"`
	}
	type cli struct {
		Name           string   `json:"name"`
		Kind           string   `json:"kind"`
		Path           string   `json:"path"`
		Args           []string `json:"args"`
		PermissionMode string   `json:"permission_mode"`
		EnvKeys        []string `json:"env_keys"`
	}
	out := struct {
		CLIs      []cli  `json:"clis"`
		Providers []prov `json:"providers"`
	}{}
	for _, c := range s.cfg().CLIs {
		it := cli{Name: c.Name, Kind: c.Kind, Path: c.Path, Args: c.Args, PermissionMode: c.PermissionMode}
		it.EnvKeys = sortedKeys(c.Env)
		out.CLIs = append(out.CLIs, it)
	}
	for _, p := range s.cfg().Providers {
		// Empty slice, not nil — see handleProviders: a nil marshals to
		// JSON null and the UI indexes into models unguarded.
		item := prov{Name: p.Name, Kind: p.Kind, Models: []model{}}
		item.EnvKeys = sortedKeys(p.Env)
		for _, m := range p.Models {
			mm := model{Name: m.Name, Input: m.Input}
			if mm.Input == nil {
				mm.Input = []string{"text"}
			}
			mm.EnvKeys = sortedKeys(m.Env)
			item.Models = append(item.Models, mm)
		}
		out.Providers = append(out.Providers, item)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	home, _ := os.UserHomeDir()
	writeJSON(w, http.StatusOK, map[string]any{
		"default_cwd": s.cfg().DefaultCwd,
		"data_dir":    s.cfg().DataDir,
		"home":        home,
	})
}

// ---- sessions ----

type sessionView struct {
	*store.Session
	Engine        string        `json:"engine"` // cli kind: claude | opencode | mcode | qoder
	Status        runner.Status `json:"status"`
	ResumeCommand string        `json:"resume_command,omitempty"`
}

func (s *Server) view(sess *store.Session) sessionView {
	return sessionView{Session: sess, Engine: s.engineOf(sess), Status: s.mgr.GetStatus(sess.ID), ResumeCommand: s.resumeCommand(sess)}
}

// engineOf resolves a session's engine kind from its cli config entry
// (config.KindClaude when the cli is unknown).
func (s *Server) engineOf(sess *store.Session) string {
	if cli := s.cfg().CLIByName(sess.CLI); cli != nil {
		return cli.Kind
	}
	return config.KindClaude
}

// cliProfile picks the on-disk transcript layout for a cli: the CLIs sharing
// Claude Code's format differ only in home directory (claudehist.Profile). A
// nil cli falls back to Claude Code, matching engineOf.
func cliProfile(cli *config.CLI) claudehist.Profile {
	if cli != nil && cli.IsQoder() {
		return claudehist.QoderProfile
	}
	return claudehist.ClaudeProfile
}

// firstProviderOfKind returns the first configured provider of the engine
// kind, or nil.
func (s *Server) firstProviderOfKind(kind string) *config.Provider {
	for i := range s.cfg().Providers {
		if s.cfg().Providers[i].Kind == kind {
			return &s.cfg().Providers[i]
		}
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// resumeCommand builds the shell command that resumes this session from a
// terminal. claude engine: cli --resume <sid> --model <m> --effort <level>,
// plus --settings only when the config carries env the terminal would not
// otherwise have (endpoint/token) — a provider with no env at all, ducc-models,
// gets its endpoint and per-model setup from the CLI itself, so pointing the
// command at our materialized file would only add a path into this checkout's
// data dir that breaks as soon as the command travels. opencode engine:
// opencode -s <sid> (its own store/auth). qoder engine: same shape as claude
// but spelled with qoder's own flags (--reasoning-effort, and no --verbose,
// which the CLI rejects).
func (s *Server) resumeCommand(sess *store.Session) string {
	if sess.ClaudeSessionID == "" {
		return ""
	}
	cli := s.cfg().CLIByName(sess.CLI)
	cliPath := ""
	if cli != nil {
		cliPath = cli.Path
	}
	base := "cd " + shellQuote(sess.Cwd) + " && "
	engine := s.engineOf(sess)
	effortFlag := "--effort"
	switch engine {
	case config.KindOpenCode:
		if cliPath == "" {
			cliPath = "opencode"
		}
		return base + shellQuote(cliPath) + " -s " + sess.ClaudeSessionID
	case config.KindMcode:
		// mcode resumes with --session <mvs_…>. The TUI form takes no
		// --model/--effort (it reads the session's own recorded model), and
		// passing them to a TUI launch is not supported — so the command
		// stays minimal and picks up the session's model by itself.
		if cliPath == "" {
			cliPath = "mcode"
		}
		return base + shellQuote(cliPath) + " --session " + sess.ClaudeSessionID
	case config.KindQoder:
		effortFlag = "--reasoning-effort"
	}
	prov := s.cfg().ProviderByName(sess.Provider)
	var settings string
	if cliPath == "" {
		cliPath = "claude"
		if engine == config.KindQoder {
			cliPath = "qoderclicn"
		}
	}
	model := ""
	if prov != nil {
		model = prov.EffectiveModel(sess.ModelOverride)
	}
	if cli != nil && prov != nil {
		if env := cli.EffectiveEnv(prov, model); len(env) > 0 {
			// Session effort is part of the file name (see runner.SettingsPath):
			// hand out the same file the runner passes to the spawn, materializing
			// it if this effort level has not run a turn yet — else the copied
			// command dies on "Settings file not found".
			settings = s.mgr.EnsureSettingsFile(cli.Name, prov.Name, model, sess.EffortOverride, env)
		}
	}
	cmd := base + shellQuote(cliPath) + " --resume " + sess.ClaudeSessionID
	if model != "" {
		// --model, not env: a launcher CLI resolves the flag into its own
		// settings and canonicalizes the name (ducc turns "Claude Sonnet 5"
		// into "Claude Sonnet 5[1m]" + that model's context window and auth),
		// while our settings file is merged last and would instead pin the
		// raw config name. Providers whose env does name a model get both,
		// exactly as the spawn does.
		cmd += " --model " + shellQuote(model)
	}
	if sess.EffortOverride != "" {
		cmd += " " + effortFlag + " " + shellQuote(sess.EffortOverride)
	}
	if settings != "" {
		cmd += " --settings " + shellQuote(settings)
	}
	return cmd
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	showArchived := q.Get("archived") == "true"
	out := []sessionView{}
	for _, sess := range s.st.List() {
		// Shadow group-member sessions live under the group view, never in
		// the sidebar (they are still openable directly by id).
		if sess.GroupID != "" {
			continue
		}
		if sess.Archived != showArchived {
			continue
		}
		out = append(out, s.view(sess))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name           string `json:"name"`
		CLI            string `json:"cli"`
		Provider       string `json:"provider"`
		Cwd            string `json:"cwd"`
		Model          string `json:"model"`
		Effort         string `json:"effort"`
		PermissionMode string `json:"permission_mode"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if body.CLI == "" && len(s.cfg().CLIs) > 0 {
		body.CLI = s.cfg().CLIs[0].Name
	}
	cli := s.cfg().CLIByName(body.CLI)
	if cli == nil {
		writeErr(w, http.StatusBadRequest, "unknown cli: "+body.CLI)
		return
	}
	if body.Provider == "" {
		// Default to the first provider of the cli's engine kind, so the
		// opencode cli doesn't silently pair with an Anthropic-env provider.
		if p := s.firstProviderOfKind(cli.Kind); p != nil {
			body.Provider = p.Name
		}
	}
	prov := s.cfg().ProviderByName(body.Provider)
	if prov == nil {
		writeErr(w, http.StatusBadRequest, "unknown provider: "+body.Provider)
		return
	}
	if prov.Kind != cli.Kind {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("engine mismatch: cli %q (%s) with provider %q (%s)", cli.Name, cli.Kind, prov.Name, prov.Kind))
		return
	}
	if body.Model != "" && !s.modelKnown(cli, prov, body.Model) {
		writeErr(w, http.StatusBadRequest, "unknown model for provider "+body.Provider+": "+body.Model)
		return
	}
	if body.Cwd == "" {
		writeErr(w, http.StatusBadRequest, "workspace (cwd) is required")
		return
	}
	resolvedCwd, err := runner.ResolveCwd(body.Cwd)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	body.Cwd = resolvedCwd
	if !effortOKFor(cli.Kind, body.Effort) {
		writeErr(w, http.StatusBadRequest, "invalid effort: "+body.Effort)
		return
	}
	now := time.Now()
	sess := &store.Session{
		ID:             newID(),
		Name:           body.Name,
		CLI:            body.CLI,
		Provider:       body.Provider,
		Cwd:            body.Cwd,
		ModelOverride:  body.Model,
		EffortOverride: body.Effort,
		PermissionMode: body.PermissionMode,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if sess.Name == "" {
		sess.Name = "session-" + sess.ID[:8]
	}
	if err := s.st.Upsert(sess, nil); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.view(sess))
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sess := s.st.Get(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, s.view(sess))
}

func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	sess := s.st.Get(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	var body struct {
		Name           *string `json:"name"`
		CLI            *string `json:"cli"`
		Provider       *string `json:"provider"`
		Model          *string `json:"model"`
		Effort         *string `json:"effort"`
		PermissionMode *string `json:"permission_mode"`
		Archived       *bool   `json:"archived"`
		SortOrder      *int    `json:"sort_order"`
		Resident       *bool   `json:"resident"`
		PeerName       *string `json:"peer_name"`
		Cwd            *string `json:"cwd"`
		Unread         *bool   `json:"unread"`
		Todo           *bool   `json:"todo"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	// Unread/Todo are lightweight display flags toggled by the sidebar (open a
	// session → clear unread; mark/clear todo). Route a flags-only patch on a
	// fast path that skips the full validation and — crucially — does not bump
	// UpdatedAt, so opening a session or toggling a flag never reorders the
	// list. A patch that also changes real fields falls through to the normal
	// path below (where the flags fold into the same Upsert).
	if (body.Unread != nil || body.Todo != nil) &&
		body.Name == nil && body.CLI == nil && body.Provider == nil &&
		body.Model == nil && body.Effort == nil && body.PermissionMode == nil &&
		body.Archived == nil && body.SortOrder == nil && body.Resident == nil &&
		body.PeerName == nil && body.Cwd == nil {
		if err := s.st.SetFlags(sess.ID, body.Unread, body.Todo); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.view(s.st.Get(sess.ID)))
		return
	}
	if body.CLI != nil && s.cfg().CLIByName(*body.CLI) == nil {
		writeErr(w, http.StatusBadRequest, "unknown cli: "+*body.CLI)
		return
	}
	if body.Provider != nil && s.cfg().ProviderByName(*body.Provider) == nil {
		writeErr(w, http.StatusBadRequest, "unknown provider: "+*body.Provider)
		return
	}
	if body.Model != nil && *body.Model != "" {
		// Validate the model against the (possibly newly set) provider, under
		// the (possibly newly set) cli — modelKnown is engine-aware, so the
		// cli that will actually run the turn is what decides.
		provName := sess.Provider
		if body.Provider != nil {
			provName = *body.Provider
		}
		patchCliName := sess.CLI
		if body.CLI != nil {
			patchCliName = *body.CLI
		}
		if prov := s.cfg().ProviderByName(provName); prov != nil && !s.modelKnown(s.cfg().CLIByName(patchCliName), prov, *body.Model) {
			writeErr(w, http.StatusBadRequest, "unknown model for provider "+provName+": "+*body.Model)
			return
		}
	}
	// The cli and provider of a session must share an engine kind.
	cliName := sess.CLI
	if body.CLI != nil {
		cliName = *body.CLI
	}
	provName := sess.Provider
	if body.Provider != nil {
		provName = *body.Provider
	}
	cli := s.cfg().CLIByName(cliName)
	prov := s.cfg().ProviderByName(provName)
	if cli != nil && prov != nil && cli.Kind != prov.Kind {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("engine mismatch: cli %q (%s) with provider %q (%s)", cli.Name, cli.Kind, prov.Name, prov.Kind))
		return
	}
	// Effort levels are engine-specific: fixed --effort set for claude, any
	// well-formed --variant key for opencode (validated against the final cli
	// kind after possible cli/provider switches above).
	if body.Effort != nil {
		kind := config.KindClaude
		if cli != nil {
			kind = cli.Kind
		}
		if !effortOKFor(kind, *body.Effort) {
			writeErr(w, http.StatusBadRequest, "invalid effort: "+*body.Effort)
			return
		}
		// mcode's --effort levels are per-model and short (default/low/…/max,
		// none-thinking/thinking, or none at all) — the shape check above
		// cannot tell a valid level from a typo, and the CLI only rejects an
		// unknown one after the run has started. Validate against the model
		// catalog here so a bad level is a 400 instead of a dead turn.
		if kind == config.KindMcode && prov != nil && *body.Effort != "" {
			model := prov.EffectiveModel(sess.ModelOverride)
			if body.Model != nil {
				model = prov.EffectiveModel(*body.Model)
			}
			cat, err := mcodehist.LoadCatalog(mcodeProviderIDFromModel(model, prov.Name))
			if m := cat.Find(mcodehist.BareModelID(model)); err == nil && m != nil && len(m.Efforts) > 0 &&
				!mcodehist.EffortOK(cat, mcodehist.BareModelID(model), *body.Effort) {
				writeErr(w, http.StatusBadRequest,
					fmt.Sprintf("模型 %s 不支持推理档位 %q（可选：%s）", model, *body.Effort, strings.Join(m.Efforts, ", ")))
				return
			}
		}
	}
	// Resident/peer messaging is a Claude Code protocol feature; no other engine
	// may enable it (the cli kind that applies after this patch decides).
	if (body.Resident != nil || body.PeerName != nil) && cli != nil && !cli.IsClaude() {
		writeErr(w, http.StatusBadRequest, cli.Kind+" 引擎不支持常驻/peer 消息")
		return
	}
	// Only the engines that keep a plain transcript file inside the workspace
	// (~/.claude/projects/<slug>/, ~/.qoder-cn/projects/<slug>/) have files to
	// relocate when the workspace changes. opencode and mcode both key their own
	// store by session id, and their run is pinned to the session's cwd at spawn.
	transcriptInWorkspace := cli == nil || cli.IsClaude() || cli.IsQoder()
	// Changing the workspace path. Resolve + validate up front (pure, no side
	// effects) so a rejected patch never moves files or stops the resident;
	// the actual relocation happens after every other check passes.
	effCwd := sess.Cwd
	if body.Cwd != nil {
		resolved, err := runner.ResolveCwd(*body.Cwd)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		effCwd = resolved
	}
	cwdChanged := effCwd != sess.Cwd
	// A live turn writes its transcript under the old cwd; migrating the files
	// underneath a running process can fork the log (the CLI may recreate the
	// old path on its next append). Refuse like rewind does.
	if cwdChanged && s.mgr.Busy(sess.ID) {
		writeErr(w, http.StatusConflict, "会话正在运行，请先停止或等本轮结束再修改工作区")
		return
	}
	// Peer-name changes write the claude transcript (custom-title). Validate
	// up front; the transcript write happens below, at the final cwd. A
	// session without a claude id just stores the value; it's applied at the
	// first resident spawn.
	peerName := ""
	if body.PeerName != nil {
		cleaned := runner.CleanPeerName(*body.PeerName)
		if strings.TrimSpace(*body.PeerName) != "" && cleaned == "" {
			writeErr(w, http.StatusBadRequest, "地址名无效：不能以 / 开头")
			return
		}
		peerName = cleaned
	}
	// A live resident is bound to the old transcript; stop it before moving
	// the files underneath it (it is respawned under the new cwd in the
	// side-effects section below).
	if cwdChanged && transcriptInWorkspace && sess.Resident {
		s.mgr.TeardownResident(sess.ID)
	}
	// Workspace migration: relocate the session's on-disk claude transcript
	// (main jsonl + subagents dir) to the new project's slug so --resume and
	// history replay keep working from the migrated directory. opencode's
	// conversation lives in its own store keyed by session id — nothing to
	// move. A missing source transcript (e.g. imported from another machine)
	// is fine: the change still applies, the session just replays its stored
	// events until new turns write a fresh transcript.
	if cwdChanged && transcriptInWorkspace && sess.ClaudeSessionID != "" {
		moved, err := claudehist.MoveSessionIn(cliProfile(cli), sess.Cwd, effCwd, sess.ClaudeSessionID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "迁移会话历史文件失败："+err.Error())
			return
		}
		if moved {
			log.Printf("session %s: workspace %s -> %s (transcript relocated)", sess.ID, sess.Cwd, effCwd)
		}
	}
	// Peer-name rename writes to the transcript at its final location.
	if body.PeerName != nil && sess.ClaudeSessionID != "" {
		name := peerName
		if name == "" {
			name = runner.CleanPeerName(sess.Name)
		}
		if name != "" {
			if err := claudehist.RenameSessionIn(cliProfile(cli), effCwd, sess.ClaudeSessionID, name); err != nil {
				writeErr(w, http.StatusBadGateway, "写入会话地址名失败："+err.Error())
				return
			}
		}
	}
	if body.PeerName != nil {
		if err := s.st.Upsert(sess, func(cur *store.Session) { cur.PeerName = peerName }); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if body.Resident != nil {
		if err := s.st.Upsert(sess, func(cur *store.Session) { cur.Resident = *body.Resident }); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	err := s.st.Upsert(sess, func(cur *store.Session) {
		if body.Name != nil {
			cur.Name = *body.Name
		}
		if body.CLI != nil {
			cur.CLI = *body.CLI
		}
		if body.Provider != nil {
			cur.Provider = *body.Provider
			// The stored model override may not exist in the new provider.
			if cur.ModelOverride != "" {
				if prov := s.cfg().ProviderByName(cur.Provider); prov != nil && prov.ModelByName(cur.ModelOverride) == nil {
					cur.ModelOverride = ""
				}
			}
		}
		if body.Model != nil {
			cur.ModelOverride = *body.Model
		}
		if body.Effort != nil {
			cur.EffortOverride = *body.Effort
		}
		if body.PermissionMode != nil {
			cur.PermissionMode = *body.PermissionMode
		}
		if body.Archived != nil {
			cur.Archived = *body.Archived
		}
		if body.SortOrder != nil {
			cur.SortOrder = *body.SortOrder
		}
		if body.Cwd != nil {
			cur.Cwd = effCwd
		}
		if body.Unread != nil {
			cur.Unread = *body.Unread
		}
		if body.Todo != nil {
			cur.Todo = *body.Todo
		}
		cur.UpdatedAt = time.Now()
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Process-level side effects, after all metadata is stored so the manager
	// sees the final values.
	cur := s.st.Get(sess.ID)
	if body.Resident != nil {
		if *body.Resident {
			if err := s.mgr.EnsureResident(sess.ID); err != nil {
				writeErr(w, http.StatusBadGateway, "常驻进程启动失败："+err.Error())
				return
			}
		} else {
			s.mgr.TeardownResident(sess.ID)
		}
	} else if body.PeerName != nil {
		// Rename a live resident under the new address (restart). Skipped when
		// resident was toggled in the same request — EnsureResident's spawn
		// already registered the new name.
		if err := s.mgr.ApplyPeerName(cur); err != nil {
			log.Printf("apply peer name for %s: %v", sess.ID, err)
		}
	}
	// A workspace change stopped the resident above (if it was live) — bring
	// it back under the new cwd unless the same request explicitly turned it
	// off.
	if cwdChanged && cur.Resident && body.Resident == nil {
		if err := s.mgr.EnsureResident(sess.ID); err != nil {
			log.Printf("restart resident after workspace change %s: %v", sess.ID, err)
		}
	}
	writeJSON(w, http.StatusOK, s.view(s.st.Get(sess.ID)))
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mgr.TeardownResident(id)
	s.mgr.Stop(id)
	s.mgr.ClearPermissions(id)
	// Uploaded attachments live under data/uploads/<sid>/ — drop them with
	// the session. Best-effort: the store is the source of truth, a leftover
	// uploads dir is only wasted space.
	_ = os.RemoveAll(filepath.Join(s.cfg().DataDir, "uploads", id))
	if err := s.st.Delete(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.st.Get(id)
	if sess == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	var body struct {
		Text   string                `json:"text"`
		Images []runner.Image        `json:"images,omitempty"`
		Files  []runner.UploadedFile `json:"files,omitempty"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if strings.TrimSpace(body.Text) == "" && len(body.Images) == 0 && len(body.Files) == 0 {
		writeErr(w, http.StatusBadRequest, "empty message")
		return
	}
	// The opencode and mcode headless runs have no inline-multimodal channel:
	// image blocks would be silently dropped, so refuse them up front. File
	// uploads still work (they ride as path mentions in the text, plus the
	// engine's own attachment flag, and a vision model reads them that way).
	if len(body.Images) > 0 {
		if eng := s.engineOf(sess); eng == config.KindOpenCode || eng == config.KindMcode {
			writeErr(w, http.StatusBadRequest, eng+" 引擎暂不支持图片消息（文件附件可用）")
			return
		}
	}
	// Files must have come from this session's uploads dir — the paths are
	// interpolated into the turn's text, so a crafted payload must not be
	// able to point (or inject) outside what the upload endpoint wrote.
	upDir := filepath.Join(s.cfg().DataDir, "uploads", id) + string(os.PathSeparator)
	for _, f := range body.Files {
		if !strings.HasPrefix(f.Path, upDir) {
			writeErr(w, http.StatusBadRequest, "invalid file path")
			return
		}
	}
	// Workspace gate: refuse turns until the session has a cwd.
	if sess.Cwd == "" {
		writeErr(w, http.StatusBadRequest, "select a workspace before sending messages")
		return
	}
	if err := s.mgr.Enqueue(id, runner.Message{Text: body.Text, Images: body.Images, Files: body.Files}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// maxUploadBytes caps a single uploaded attachment. Uploads are persisted
// raw to disk (not embedded in the message), so the cap can be generous
// without bloating the event stream.
const maxUploadBytes = 48 << 20 // 48 MiB

// handleUpload persists one uploaded file under data/uploads/<sid>/ and
// returns its on-disk path. The agent is handed the path in the message text
// (see runner.turnText) and reads it with its own tools. Single raw-body
// request per file; the original name travels in the ?name= query param.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.st.Get(id) == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	name, err := sanitizeUploadName(r.URL.Query().Get("name"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid file name")
		return
	}
	dir := filepath.Join(s.cfg().DataDir, "uploads", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxUploadBytes)
	data, err := io.ReadAll(body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("文件超过 %dMB 上限", maxUploadBytes>>20))
		} else {
			writeErr(w, http.StatusBadRequest, "read upload body: "+err.Error())
		}
		return
	}
	if len(data) == 0 {
		writeErr(w, http.StatusBadRequest, "empty file")
		return
	}
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name,
		"path": dst,
		"mime": mime.TypeByExtension(filepath.Ext(name)),
		"size": len(data),
	})
}

// sanitizeUploadName keeps the client-supplied name to a bare single file
// name (no separators, no control chars) so nothing escapes the upload dir.
func sanitizeUploadName(raw string) (string, error) {
	name := strings.TrimSpace(filepath.Base(strings.ReplaceAll(raw, "\\", "/")))
	if name == "" || name == "." || name == ".." {
		return "", errors.New("invalid upload name")
	}
	if strings.IndexFunc(name, func(r rune) bool { return unicode.IsControl(r) }) >= 0 {
		return "", errors.New("invalid upload name")
	}
	if runes := []rune(name); len(runes) > 128 {
		name = string(runes[:128])
	}
	return name, nil
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.mgr.Stop(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- SSE event stream ----

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.st.Get(id)
	if sess == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Ask nginx (and other reverse proxies) not to buffer the SSE stream.
	// Buffering delays events and makes live messages appear missing.
	w.Header().Set("X-Accel-Buffering", "no")

	// Replay history: stored events (user/status only — stream events are
	// sourced live from Claude's on-disk jsonl) + parsed Claude history
	// for stream events. Merge by ts ascending.
	stored, err := s.st.Events(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var merged []*store.Event
	if sess.ClaudeSessionID != "" {
		// Replay source depends on the engine: claude and qoder sessions are
		// parsed from the CLI's own jsonl on disk (same format, different home
		// directory — see claudehist.Profile), opencode sessions are exported
		// via the CLI and mcode sessions are parsed from their own
		// messages.jsonl (both keep their transcript inside the CLI's own
		// store).
		var extEvents []*store.Event
		switch s.engineOf(sess) {
		case config.KindOpenCode:
			if cli := s.cfg().CLIByName(sess.CLI); cli != nil {
				if oc, err := opencodehist.FetchSession(cli.Path, sess.ClaudeSessionID); err == nil {
					extEvents = oc.ToEvents()
				} else {
					log.Printf("opencode replay %s: %v", sess.ClaudeSessionID, err)
				}
			}
		case config.KindMcode:
			if mc, err := mcodehist.FetchSession(sess.ClaudeSessionID); err == nil {
				extEvents = mc.ToEvents()
			} else {
				log.Printf("mcode replay %s: %v", sess.ClaudeSessionID, err)
			}
		default:
			// claude and qoder share the transcript reader, differing only in
			// which home directory it reads.
			prof := claudehist.ClaudeProfile
			if s.engineOf(sess) == config.KindQoder {
				prof = claudehist.QoderProfile
			}
			extEvents, _ = claudehist.ConvertIn(prof, sess.Cwd, sess.ClaudeSessionID)
			// 子代理 transcript 单独解析（subagents/agent-*.jsonl），补齐回放侧
			// 与实时流（parent_tool_use_id 透传）同形的子代理消息事件。
			if subEvents, err := claudehist.ConvertSubagentsIn(prof, sess.Cwd, sess.ClaudeSessionID); err == nil {
				extEvents = append(extEvents, subEvents...)
			}
		}
		merged = mergeStoredAndClaude(stored, extEvents)
	} else {
		for _, e := range stored {
			merged = append(merged, e)
		}
	}
	// If the log ends mid-turn but no turn is actually in flight — the backend
	// was killed while a turn was running and restarted, so the persisted
	// turn_start has no closing turn_end/interrupted/error — close it with a
	// transient interrupted so the replay converges to the real idle state.
	// Without this, the frontend replays the dangling turn_start on every
	// connect and stays stuck on "running": stop is a no-op server-side and
	// new messages only accumulate in the pending chip. The repair is
	// per-connection and never persisted; it applies only when the manager
	// reports no live turn activity (a genuine mid-run reconnect keeps the
	// dangling turn_start, which is the only signal that the turn is live).
	if !s.mgr.Busy(id) {
		merged = closeStaleTurnStart(merged)
	}

	ch, unsubscribe := s.mgr.Subscribe(id)
	defer unsubscribe()

	// Outgoing seqs are renumbered with a connection-local monotonic counter:
	// merged replay events are ts-ordered (stored seqs and synthetic claude
	// seqs come from different namespaces and are NOT ts-monotonic), and the
	// frontend dedups by seq, which requires monotonic seqs.
	var sendSeq int64
	send := func(ev *store.Event) bool {
		c := *ev
		sendSeq++
		c.Seq = sendSeq
		raw, err := json.Marshal(&c)
		if err != nil {
			return true
		}
		if _, err := w.Write([]byte("data: " + string(raw) + "\n\n")); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	for _, ev := range merged {
		if !send(ev) {
			return
		}
	}
	// Mark the end of the history replay: the frontend buffers replayed
	// events and only renders the latest page once this marker arrives
	// (everything after it is a live event, rendered immediately).
	if !send(&store.Event{Kind: "replay_end", Ts: time.Now(), Data: json.RawMessage(`{}`)}) {
		return
	}

	keepAlive := time.NewTicker(25 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if !send(ev) {
				return
			}
		case <-keepAlive.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// closeStaleTurnStart appends a transient interrupted status event when the
// last status event in the replay is an unclosed turn_start. Status events are
// only ever persisted by the runner, which pairs every turn_start with a
// closing turn_end/interrupted/error — except when the backend dies mid-turn,
// leaving the dangling turn_start in the log forever (replay-side status
// events only ever come from the stored log; the claude/opencode converters
// emit user/stream kinds only). The appended event is returned in the slice
// passed in and sent to this connection alone. No-op when the log is empty or
// already closed by a later status event.
func closeStaleTurnStart(events []*store.Event) []*store.Event {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "status" {
			continue
		}
		var d struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(events[i].Data, &d)
		if d.State != "turn_start" {
			return events // last status event already closes the turn
		}
		return append(events, &store.Event{
			Kind: "status",
			Ts:   time.Now(),
			Data: json.RawMessage(`{"state":"interrupted"}`),
		})
	}
	return events
}

// mergeStoredAndClaude returns a single ts-ordered event list combining the
// persisted user/status events with parsed Claude stream events. Stream
// events are assigned synthetic seqs starting at max(stored seq) + 1.
func mergeStoredAndClaude(stored []*store.Event, claude []*store.Event) []*store.Event {
	maxSeq := int64(0)
	for _, e := range stored {
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	out := make([]*store.Event, 0, len(stored)+len(claude))
	out = append(out, stored...)

	seen := make(map[string]struct{}, len(stored))
	for _, e := range stored {
		seen[eventFingerprint(e)] = struct{}{}
	}
	// Stored user-text events — a multiset used to suppress Claude's echo
	// of the same prompts in its own jsonl. Match by count, not timestamp:
	// the jsonl echo can land arbitrarily later than the enqueue-time
	// stored event (process startup, queue wait), which used to defeat the
	// old 1s window and duplicate the user bubble on history replay.
	storedUserText := map[string]int{} // trimmed text -> remaining count
	for _, e := range stored {
		if e.Kind != "user" {
			continue
		}
		var d struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(e.Data, &d)
		storedUserText[strings.TrimSpace(d.Text)]++
	}

	nextSeq := maxSeq + 1
	for _, e := range claude {
		// Suppress Claude's echo of the user prompt (we already have it).
		if e.Kind == "user" {
			var d struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(e.Data, &d)
			if t := strings.TrimSpace(d.Text); storedUserText[t] > 0 {
				storedUserText[t]--
				continue
			}
		}
		if _, ok := seen[eventFingerprint(e)]; ok {
			continue
		}
		seen[eventFingerprint(e)] = struct{}{}
		c := *e
		c.Seq = nextSeq
		nextSeq++
		out = append(out, &c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ts.Before(out[j].Ts) })
	return out
}

// eventFingerprint produces a stable identity for an event used to dedupe
// across sources. For Claude events with a message id, include it; for tool
// results, use tool_use_id + first content fragment; for status, just kind.
func eventFingerprint(e *store.Event) string {
	if e.Kind != "stream" {
		return string(e.Kind) + "|" + e.Ts.Format(time.RFC3339Nano)
	}
	var d map[string]any
	if err := json.Unmarshal(e.Data, &d); err != nil {
		return string(e.Kind) + "|" + e.Ts.Format(time.RFC3339Nano)
	}
	switch d["type"] {
	case "assistant":
		if msg, ok := d["message"].(map[string]any); ok {
			// Include the block signature so split lines of one API step
			// (thinking / tool_use blocks share a message id) survive dedup,
			// while genuine duplicates (same id + same blocks) still collapse.
			id, _ := msg["id"].(string)
			var sig string
			if content, ok := msg["content"].([]any); ok {
				parts := make([]string, 0, len(content))
				for _, b := range content {
					if bm, ok := b.(map[string]any); ok {
						t, _ := bm["type"].(string)
						bid, _ := bm["id"].(string)
						parts = append(parts, t+":"+bid)
					}
				}
				sig = strings.Join(parts, ",")
			}
			return "assistant|" + id + "|" + sig
		}
	case "user":
		if msg, ok := d["message"].(map[string]any); ok {
			if content, ok := msg["content"].([]any); ok && len(content) > 0 {
				if first, ok := content[0].(map[string]any); ok {
					if id, ok := first["tool_use_id"].(string); ok {
						return "tool_result|" + id + "|" + e.Ts.Format(time.RFC3339Nano)
					}
				}
			}
		}
	case "system":
		if sub, ok := d["subtype"].(string); ok {
			if sub == "init" {
				if sid, ok := d["session_id"].(string); ok {
					return "init|" + sid
				}
			}
		}
	}
	return string(e.Kind) + "|" + e.Ts.Format(time.RFC3339Nano)
}

// ---- claude history import ----

func (s *Server) handleClaudeSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cwd := q.Get("cwd")
	prefix := strings.ToLower(strings.TrimSpace(q.Get("prefix")))
	list, err := claudehist.List(cwd)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if prefix != "" {
		filtered := list[:0]
		for _, item := range list {
			if strings.HasPrefix(strings.ToLower(item.SessionID), prefix) {
				filtered = append(filtered, item)
			}
		}
		list = filtered
	}
	if list == nil {
		list = []*claudehist.SessionInfo{}
	}
	// Cap to keep the UI snappy.
	if len(list) > 200 {
		list = list[:200]
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClaudeSessionID string `json:"claude_session_id"`
		Cwd             string `json:"cwd"`
		CLI             string `json:"cli"`
		Provider        string `json:"provider"`
		Name            string `json:"name"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if body.ClaudeSessionID == "" || body.Cwd == "" {
		writeErr(w, http.StatusBadRequest, "claude_session_id and cwd are required")
		return
	}
	if body.CLI == "" && len(s.cfg().CLIs) > 0 {
		body.CLI = s.cfg().CLIs[0].Name
	}
	cli := s.cfg().CLIByName(body.CLI)
	if cli == nil {
		writeErr(w, http.StatusBadRequest, "unknown cli: "+body.CLI)
		return
	}
	if body.Provider == "" {
		// Default to the first provider of the cli's engine kind.
		if p := s.firstProviderOfKind(cli.Kind); p != nil {
			body.Provider = p.Name
		}
	}
	prov := s.cfg().ProviderByName(body.Provider)
	if prov == nil {
		writeErr(w, http.StatusBadRequest, "unknown provider: "+body.Provider)
		return
	}
	if prov.Kind != cli.Kind {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("engine mismatch: cli %q (%s) with provider %q (%s)", cli.Name, cli.Kind, prov.Name, prov.Kind))
		return
	}
	now := time.Now()
	sess := &store.Session{
		ID:              newID(),
		Name:            body.Name,
		CLI:             body.CLI,
		Provider:        body.Provider,
		Cwd:             body.Cwd,
		ClaudeSessionID: body.ClaudeSessionID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if sess.Name == "" {
		sess.Name = "imported-" + body.ClaudeSessionID[:8]
	}
	if cli.IsMcode() {
		// mcode import: the transcript lives in mcode's own dated session
		// store. Reading it doubles as the readability check (SSE re-parses on
		// connect) and yields the session's model, aligned onto the chosen
		// provider when the model id exists there. mcode does not record the
		// session's working directory outside its runtime sqlite (which this
		// project deliberately does not link), so the workspace comes from the
		// request — the session editor can change it afterwards.
		if _, err := runner.ResolveCwd(body.Cwd); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		mc, err := mcodehist.FetchSession(body.ClaudeSessionID)
		if err != nil {
			writeErr(w, http.StatusNotFound, "mcode session not found: "+err.Error())
			return
		}
		if mc.ModelID != "" && prov.ModelByName(mcodehist.BareModelID(mc.ModelID)) != nil {
			sess.ModelOverride = mcodehist.BareModelID(mc.ModelID)
		}
		if err := s.st.Upsert(sess, nil); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.view(sess))
		return
	}
	if cli.IsOpenCode() {
		// opencode import: the transcript lives in opencode's own store. The
		// export doubles as the readability check (SSE re-exports on connect)
		// and as the source of the session's model, aligned onto the chosen
		// provider when the model id exists there.
		if _, err := runner.ResolveCwd(body.Cwd); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		oc, err := opencodehist.FetchSession(cli.Path, body.ClaudeSessionID)
		if err != nil {
			writeErr(w, http.StatusNotFound, "opencode session not found: "+err.Error())
			return
		}
		if oc.ModelID != "" && prov.ModelByName(oc.ModelID) != nil {
			sess.ModelOverride = oc.ModelID
		}
		if err := s.st.Upsert(sess, nil); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.view(sess))
		return
	}
	// Validate that the source file exists; the SSE handler will read it
	// live on demand, so we don't need to copy/convert it into our store.
	// claude and qoder share the reader, differing only in home directory.
	prof := claudehist.ClaudeProfile
	if cli.IsQoder() {
		prof = claudehist.QoderProfile
	}
	if _, err := os.Stat(claudehist.HistoryPathIn(prof, body.Cwd, body.ClaudeSessionID)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusNotFound, cli.Kind+" session history not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	events, err := claudehist.ConvertIn(prof, body.Cwd, body.ClaudeSessionID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusNotFound, cli.Kind+" session history not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = events // used only to ensure the file is readable; SSE will parse on connect
	if err := s.st.Upsert(sess, nil); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.view(sess))
}

// handleOpenCodeModelVariants reports the --variant (reasoning effort) keys
// opencode accepts per model of one provider (`opencode models <provider>
// --verbose`, memoized). The effort dropdowns query this so an opencode
// session only ever offers the variant keys its model really supports.
// ?fresh=1 bypasses the memo, which would otherwise keep a model list from
// before a new model appeared for up to variantsTTL.
func (s *Server) handleOpenCodeModelVariants(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cli := s.cfg().CLIByName(strings.TrimSpace(q.Get("cli")))
	if cli == nil || !cli.IsOpenCode() {
		writeErr(w, http.StatusBadRequest, "an opencode kind cli is required")
		return
	}
	prov := s.cfg().ProviderByName(strings.TrimSpace(q.Get("provider")))
	if prov == nil || !prov.IsOpenCode() {
		writeErr(w, http.StatusBadRequest, "an opencode kind provider is required")
		return
	}
	key := cli.Name + "\x00" + prov.Name
	s.variantsMu.Lock()
	defer s.variantsMu.Unlock()
	if e, ok := s.variantsCache[key]; ok && time.Since(e.ts) < variantsTTL && q.Get("fresh") != "1" {
		writeJSON(w, http.StatusOK, map[string]any{"models": e.data})
		return
	}
	data, err := opencodehist.ModelVariants(cli.Path, prov.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.variantsCache[key] = variantsEntry{ts: time.Now(), data: data}
	writeJSON(w, http.StatusOK, map[string]any{"models": data})
}

// handleOpenCodeSessions lists sessions from the opencode engine's own store,
// the import source for the opencode tab. Requires the cli name so the backend
// knows which binary to ask; cwd/prefix filter like the claude history endpoint.
//
// `session list` is scoped to the project bucket of the process cwd (git repo
// root, or "global" for anything outside one) and opencode has no CLI surface
// that enumerates the other buckets, so an empty cwd lists every project
// through the read-only `opencode db` SELECT of opencodehist.ListAll — the same
// enumeration the stats surface uses — and falls back to the server's own
// bucket when that fails. That mirrors the claude/qoder tabs, where an empty
// cwd already lists every project instead of just the server's directory.
func (s *Server) handleOpenCodeSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cli := s.cfg().CLIByName(strings.TrimSpace(q.Get("cli")))
	if cli == nil || !cli.IsOpenCode() {
		writeErr(w, http.StatusBadRequest, "an opencode kind cli is required")
		return
	}
	cwd := q.Get("cwd")
	prefix := strings.ToLower(strings.TrimSpace(q.Get("prefix")))
	// `session list` 只返回进程 cwd 所属 opencode 项目桶的会话；给了 cwd 参数就在
	// 那个目录里跑（git 仓库根 = 该仓库桶，非 git 目录 = global 桶），空则跨项目枚举。
	listDir := ""
	if cwd != "" {
		abs, err := runner.ResolveCwd(cwd)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		listDir = abs
		cwd = abs
	}
	var (
		list []opencodehist.SessionInfo
		err  error
	)
	if cwd == "" {
		list, err = opencodehist.ListAll(cli.Path)
		if err != nil {
			log.Printf("opencode 跨项目枚举失败，回落到服务进程所在项目桶：%v", err)
			list, err = opencodehist.List(cli.Path, "")
		}
	} else {
		list, err = opencodehist.List(cli.Path, listDir)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	filtered := list[:0]
	for _, item := range list {
		if cwd != "" && item.Cwd != cwd {
			continue
		}
		if prefix != "" && !strings.HasPrefix(strings.ToLower(item.SessionID), prefix) {
			continue
		}
		filtered = append(filtered, item)
	}
	list = filtered
	if list == nil {
		list = []opencodehist.SessionInfo{}
	}
	if len(list) > 200 {
		list = list[:200]
	}
	writeJSON(w, http.StatusOK, list)
}

// handleOpenCodeSessionPreview returns a single opencode session's import
// preview: metadata + the most recent messages.
func (s *Server) handleOpenCodeSessionPreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cli := s.cfg().CLIByName(strings.TrimSpace(q.Get("cli")))
	if cli == nil || !cli.IsOpenCode() {
		writeErr(w, http.StatusBadRequest, "an opencode kind cli is required")
		return
	}
	id := strings.TrimSpace(q.Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	oc, err := opencodehist.FetchSession(cli.Path, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "opencode session not found: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": oc.ID,
		"cwd":        oc.Directory,
		"branch":     gitBranch(oc.Directory),
		"updated_at": oc.UpdatedAt,
		"messages":   oc.Preview(8),
	})
}

// handleMcodeModels reports the model catalog mcode resolves from its own
// runtime config (~/.minimax/config.yaml), including each model's reasoning
// levels and input modalities. mcode has no CLI command that lists its models
// (`mcode provider list --json` returns an empty `models` array because the
// set comes from the credential source), so the dropdowns are fed from that
// file — memoized for variantsTTL, with ?fresh=1 to bypass the memo right
// after `mcode provider add` or a CLI update.
func (s *Server) handleMcodeModels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cli := s.cfg().CLIByName(strings.TrimSpace(q.Get("cli")))
	if cli == nil || !cli.IsMcode() {
		writeErr(w, http.StatusBadRequest, "a mcode kind cli is required")
		return
	}
	prov := s.cfg().ProviderByName(strings.TrimSpace(q.Get("provider")))
	if prov == nil || !prov.IsMcode() {
		writeErr(w, http.StatusBadRequest, "a mcode kind provider is required")
		return
	}
	// mcode's own provider id is the prefix of --model ("minimax/…"); it
	// need not equal the cc-webui provider name, so prefer the id recorded
	// in the session's model when one was ever chosen.
	providerID := prov.Name
	if m := prov.ModelByName(prov.DefaultModelName()); m != nil {
		providerID = mcodeProviderIDFromModel(prov.DefaultModelName(), prov.Name)
	}
	cat, err := mcodehist.LoadCatalog(providerID)
	if fresh := q.Get("fresh") == "1"; err == nil && fresh {
		cat, err = mcodehist.LoadCatalogFresh(providerID)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cat)
}

// modelKnown reports whether a model id is selectable on this provider.
//
// claude and opencode providers declare their models in config.yaml, so
// membership in that list is the whole answer. mcode and qoder are different:
// their usable set is resolved at run time by the CLI (mcode from its own
// catalog, qoder from `qoderclicn --list-models`) and is deliberately NOT
// written into config — it would go stale on the next CLI update or account
// change — so a config-declared list is normally empty and membership would
// reject every real model. Those models are checked against the CLI's own list
// instead; when that list cannot be read (CLI not installed, not logged in) the
// check is skipped rather than blocking a session — a bad model name surfaces
// immediately as the CLI's own error on the first turn, which is a better
// failure than refusing to create the session at all.
func (s *Server) modelKnown(cli *config.CLI, prov *config.Provider, model string) bool {
	if prov.ModelByName(model) != nil {
		return true
	}
	if cli == nil || prov == nil {
		return false
	}
	switch {
	case cli.IsMcode():
		bare := mcodehist.BareModelID(model)
		cat, err := mcodehist.LoadCatalog(mcodeProviderIDFromModel(model, prov.Name))
		if err != nil {
			log.Printf("mcode model catalog unavailable, skipping model check for %q: %v", model, err)
			return true
		}
		// An unknown bare id is rejected: the catalog was readable, so this is a
		// genuine "this model does not exist" rather than "we cannot tell".
		return cat.Find(bare) != nil
	case cli.IsQoder():
		models, err := qoderModels(cli.Path, false)
		if err != nil {
			log.Printf("qoder model list unavailable, skipping model check for %q: %v", model, err)
			return true
		}
		for _, m := range models {
			if m.Name == model {
				return true
			}
		}
		return false
	}
	return false
}

// mcodeProviderIDFromModel recovers the mcode provider id from a model name
// that may already be qualified ("minimax/MiniMax-M3"), falling back to the
// configured provider name.
func mcodeProviderIDFromModel(model, fallback string) string {
	if i := strings.Index(model, "/"); i > 0 {
		return model[:i]
	}
	return fallback
}

// handleMcodeSessions lists the sessions mcode stored on disk, the import
// source for the mcode tab. mcode's dated session tree does not record each
// session's working directory, so `cwd` cannot filter the result — the prefix
// filter and the newest-first cap still apply.
func (s *Server) handleMcodeSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cli := s.cfg().CLIByName(strings.TrimSpace(q.Get("cli")))
	if cli == nil || !cli.IsMcode() {
		writeErr(w, http.StatusBadRequest, "a mcode kind cli is required")
		return
	}
	prefix := strings.ToLower(strings.TrimSpace(q.Get("prefix")))
	list, err := mcodehist.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	filtered := list[:0]
	for _, item := range list {
		if prefix != "" && !strings.HasPrefix(strings.ToLower(item.SessionID), prefix) {
			continue
		}
		filtered = append(filtered, item)
	}
	list = filtered
	if list == nil {
		list = []mcodehist.SessionInfo{}
	}
	if len(list) > 200 {
		list = list[:200]
	}
	writeJSON(w, http.StatusOK, list)
}

// handleMcodeSessionPreview returns a single mcode session's import preview:
// metadata plus its most recent messages.
func (s *Server) handleMcodeSessionPreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cli := s.cfg().CLIByName(strings.TrimSpace(q.Get("cli")))
	if cli == nil || !cli.IsMcode() {
		writeErr(w, http.StatusBadRequest, "a mcode kind cli is required")
		return
	}
	id := strings.TrimSpace(q.Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	mc, err := mcodehist.FetchSession(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "mcode session not found: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": mc.ID,
		"cwd":        mc.Cwd,
		"branch":     gitBranch(mc.Cwd),
		"updated_at": mc.UpdatedAt,
		"messages":   mc.Preview(8),
	})
}

// forkClaudeHistory duplicates the on-disk claude transcript of oldID under a
// freshly generated session id, rewriting every occurrence of the old id.
// This mirrors what `claude --resume <id> --fork-session` does, but without
// having to run a model turn.
func forkClaudeHistory(cwd, oldID string) (string, error) {
	return forkHistoryIn(claudehist.ClaudeProfile, cwd, oldID)
}

// forkHistoryIn duplicates a session transcript under a fresh id, rewriting
// every occurrence of the old id so the fork resumes independently. The home
// directory is the only thing that varies between the CLIs sharing this format.
func forkHistoryIn(p claudehist.Profile, cwd, oldID string) (string, error) {
	srcPath := claudehist.HistoryPathIn(p, cwd, oldID)
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		return "", fmt.Errorf("read %s history: %w", p.Engine, err)
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	newID := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	forked := bytes.ReplaceAll(raw, []byte(oldID), []byte(newID))
	if err := os.WriteFile(claudehist.HistoryPathIn(p, cwd, newID), forked, 0o600); err != nil {
		return "", fmt.Errorf("write forked history: %w", err)
	}
	return newID, nil
}

// handleFork creates a new session as a real branch of the source: the
// on-disk transcript is duplicated under a new ClaudeSessionID (the equivalent
// of `claude --resume <id> --fork-session`), so the fork and the source diverge
// from this point on and resume independently. Supported by the engines whose
// transcript is a plain file we can copy (claude, qoder).
func (s *Server) handleFork(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	src := s.st.Get(id)
	if src == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	engine := s.engineOf(src)
	prof := claudehist.ClaudeProfile
	switch engine {
	case config.KindQoder:
		prof = claudehist.QoderProfile
	case config.KindClaude:
	default:
		writeErr(w, http.StatusBadRequest, engine+" 引擎暂不支持 fork")
		return
	}
	if src.ClaudeSessionID == "" {
		writeErr(w, http.StatusBadRequest, "source session has no claude session id yet (send first message to initialize)")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	_ = decode(r, &body)
	name := body.Name
	if name == "" {
		name = src.Name + " (fork)"
	}
	forkedID, err := forkHistoryIn(prof, src.Cwd, src.ClaudeSessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now()
	sess := &store.Session{
		ID:              newID(),
		Name:            name,
		CLI:             src.CLI,
		Provider:        src.Provider,
		ModelOverride:   src.ModelOverride,
		EffortOverride:  src.EffortOverride,
		PermissionMode:  src.PermissionMode,
		Cwd:             src.Cwd,
		ClaudeSessionID: forkedID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.st.Upsert(sess, nil); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.view(sess))
}

// ---- rewind (conversation + files) ----

// handleRewind rolls a session back to a user message: files are restored by
// the claude CLI (--rewind-files), then both claude's jsonl transcript and
// cc-webui's event log are truncated before that message. The rolled-back
// message text is returned as a draft to refill the composer. Requires no
// running/queued turn and a checkpoint uuid on the target user event.
//
// claude-only: --rewind-files (the file half) exists on no other engine, and
// only the claude dialect captures a checkpoint uuid in the first place, so
// every other engine is refused up front rather than failing mid-operation.
func (s *Server) handleRewind(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.st.Get(id)
	if sess == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if eng := s.engineOf(sess); eng != config.KindClaude {
		writeErr(w, http.StatusBadRequest, eng+" 引擎不支持 rewind（文件回滚仅 claude 会话具备）")
		return
	}
	var body struct {
		Checkpoint string `json:"checkpoint"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if body.Checkpoint == "" {
		writeErr(w, http.StatusBadRequest, "checkpoint is required")
		return
	}
	// Atomically refuse if a turn is running/queued and mark the session busy
	// for the duration of the rewind, so a message sent mid-rewind can't start
	// a worker that races the truncation.
	if !s.mgr.LockForRewind(id) {
		writeErr(w, http.StatusConflict, "session is running")
		return
	}
	defer s.mgr.UnlockFromRewind(id)
	// A resident process holds the conversation in memory and re-appends
	// cached metadata on writes — never truncate the transcript under one.
	// The lock above guarantees the worker can't start a turn while we tear
	// it down; the next queued message respawns it over the truncated file.
	s.mgr.TeardownResident(id)
	if sess.ClaudeSessionID == "" {
		writeErr(w, http.StatusBadRequest, "session has no claude session id yet")
		return
	}

	// Locate the target user event by its checkpoint uuid (the SSE handler
	// renumbers seqs per-connection, so the client can't address by seq).
	events, err := s.st.Events(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var (
		target    *store.Event
		draftText string
	)
	for _, ev := range events {
		if ev.Kind != "user" {
			continue
		}
		var d struct {
			Text       string `json:"text"`
			Checkpoint string `json:"checkpoint"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		if d.Checkpoint == body.Checkpoint {
			target, draftText = ev, d.Text
			break
		}
	}
	if target == nil {
		writeErr(w, http.StatusBadRequest, "no rewindable message with that checkpoint")
		return
	}
	checkpoint := body.Checkpoint

	// 1. File rewind. Must succeed before touching the conversation, otherwise
	//    we'd leave files restored but dialogue intact (inconsistent).
	detail, err := s.mgr.RewindFiles(sess, checkpoint)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "文件回滚失败："+err.Error())
		return
	}

	// 2. Back up claude's transcript, then truncate it before the checkpoint
	//    line (the rolled-back message becomes a draft, so it's removed too).
	backupPath, err := s.backupClaudeHistory(sess)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "备份历史失败，对话未回滚："+err.Error())
		return
	}
	if _, err := claudehist.TruncateBeforeUUID(sess.Cwd, sess.ClaudeSessionID, checkpoint); err != nil {
		// Files already rewound; report the partial state honestly.
		writeErr(w, http.StatusInternalServerError, "文件已回滚，但对话截断失败（备份在 "+backupPath+"）："+err.Error())
		return
	}
	// If the truncation removed the last user prompt (rewinding the first
	// message), the claude transcript is now empty and can't be --resume'd.
	// Clear the id so the next turn starts a fresh claude session (the backed-
	// up transcript preserves the old one).
	if !claudehist.HasUserPrompt(sess.Cwd, sess.ClaudeSessionID) {
		// Clearing the id archives the old one (WithClaudeSID) so a stale id
		// left in the now-emptied transcript filename / backups stays searchable.
		s.st.Upsert(sess, store.WithClaudeSID(""))
	}

	// 3. Truncate cc-webui's event log before the target user event, then
	//    append a rewind marker as the new tail.
	kept := make([]*store.Event, 0, len(events))
	for _, ev := range events {
		if ev.Seq < target.Seq {
			kept = append(kept, ev)
		}
	}
	if err := s.st.ReplaceEvents(id, kept); err != nil {
		writeErr(w, http.StatusInternalServerError, "事件截断失败："+err.Error())
		return
	}
	rewindEv, err := s.st.AppendEvent(id, "rewind", map[string]any{
		"checkpoint": checkpoint,
		"draft_text": draftText,
		"ok":         true,
		"detail":     detail,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mgr.BroadcastEvent(id, rewindEv)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"draft_text": draftText,
		"files":      detail,
	})
}

// backupClaudeHistory copies the session's jsonl transcript to
// <data_dir>/backups/<sid>.<unix_ms>.jsonl and returns the backup path.
// Truncating the transcript is irreversible, so a backup always precedes it.
func (s *Server) backupClaudeHistory(sess *store.Session) (string, error) {
	src := claudehist.HistoryPath(sess.Cwd, sess.ClaudeSessionID)
	raw, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("read claude history: %w", err)
	}
	dir := filepath.Join(s.cfg().DataDir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, fmt.Sprintf("%s.%d.jsonl", sess.ClaudeSessionID, time.Now().UnixMilli()))
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		return "", err
	}
	return dst, nil
}

// ---- static ----

func (s *Server) serveStatic() http.Handler {
	fileServer := http.FileServerFS(s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			if !s.tokenOK(r) {
				http.Redirect(w, r, "/login.html", http.StatusFound)
				return
			}
			http.ServeFileFS(w, r, s.static, "index.html")
			return
		}
		// PWA assets and static resources must load without auth.
		pwaAsset := r.URL.Path == "/manifest.json" ||
			r.URL.Path == "/sw.js" ||
			strings.HasPrefix(r.URL.Path, "/icons/") ||
			strings.HasPrefix(r.URL.Path, "/vendor/")
		if !s.tokenOK(r) && r.URL.Path != "/login.html" && !pwaAsset {
			http.Redirect(w, r, "/login.html", http.StatusFound)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// Run starts the HTTP server.
func (s *Server) Run() error {
	log.Printf("cc-webui listening on %s", s.cfg().Listen)
	return http.ListenAndServe(s.cfg().Listen, s.mux)
}
