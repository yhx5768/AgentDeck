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

// effortOKFor validates a session effort/推理 override per engine kind.
// Claude sessions take the fixed --effort level set above; opencode sessions
// take a --variant key, whose values are provider- and model-specific (e.g.
// low/high/max, none/thinking, or nothing at all) — only the shape is checked
// here, the UI offers the actual keys from the model metadata.
func effortOKFor(kind, s string) bool {
	if s == "" {
		return true // empty = default / clear the override
	}
	if kind == config.KindOpenCode {
		if s == "" || len(s) > 32 {
			return false
		}
		for _, r := range s {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
				return false
			}
		}
		return true
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
	s.mux.Handle("POST /api/sessions/import", s.auth(s.handleImport))
	s.mux.Handle("GET /api/stats", s.auth(s.handleStats))
	s.mux.Handle("GET /api/notes", s.auth(s.handleGetNotes))
	s.mux.Handle("PUT /api/notes", s.auth(s.handlePutNotes))
	s.mux.Handle("POST /api/sessions/{id}/fork", s.auth(s.handleFork))
	s.mux.Handle("POST /api/sessions/{id}/rewind", s.auth(s.handleRewind))
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
		item := prov{Name: p.Name, Kind: p.Kind}
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
		item := prov{Name: p.Name, Kind: p.Kind}
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
	Engine        string        `json:"engine"` // "claude" | "opencode" (cli kind)
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
// terminal. claude engine: cli --resume <sid> with the effective env via
// --settings. opencode engine: opencode -s <sid> (its own store/auth).
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
	if s.engineOf(sess) == config.KindOpenCode {
		if cliPath == "" {
			cliPath = "opencode"
		}
		return base + shellQuote(cliPath) + " -s " + sess.ClaudeSessionID
	}
	prov := s.cfg().ProviderByName(sess.Provider)
	var settings string
	if cliPath == "" {
		cliPath = "claude"
	}
	if cli != nil && prov != nil {
		model := prov.EffectiveModel(sess.ModelOverride)
		if len(cli.EffectiveEnv(prov, model)) > 0 {
			settings = runner.SettingsPath(s.cfg().DataDir, cli.Name, prov.Name, model)
		}
	}
	cmd := base + shellQuote(cliPath) + " --resume " + sess.ClaudeSessionID
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
	if body.Model != "" && prov.ModelByName(body.Model) == nil {
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
		// Validate the model against the (possibly newly set) provider.
		provName := sess.Provider
		if body.Provider != nil {
			provName = *body.Provider
		}
		if prov := s.cfg().ProviderByName(provName); prov != nil && prov.ModelByName(*body.Model) == nil {
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
	}
	// Resident/peer messaging is a Claude Code protocol feature; opencode
	// sessions must not enable it (the cli kind that applies after this
	// patch decides).
	if (body.Resident != nil || body.PeerName != nil) && cli != nil && cli.IsOpenCode() {
		writeErr(w, http.StatusBadRequest, "opencode 引擎不支持常驻/peer 消息")
		return
	}
	isOpenCode := cli != nil && cli.IsOpenCode()
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
	if cwdChanged && !isOpenCode && sess.Resident {
		s.mgr.TeardownResident(sess.ID)
	}
	// Workspace migration: relocate the session's on-disk claude transcript
	// (main jsonl + subagents dir) to the new project's slug so --resume and
	// history replay keep working from the migrated directory. opencode's
	// conversation lives in its own store keyed by session id — nothing to
	// move. A missing source transcript (e.g. imported from another machine)
	// is fine: the change still applies, the session just replays its stored
	// events until new turns write a fresh transcript.
	if cwdChanged && !isOpenCode && sess.ClaudeSessionID != "" {
		moved, err := claudehist.MoveSession(sess.Cwd, effCwd, sess.ClaudeSessionID)
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
			if err := claudehist.RenameSession(effCwd, sess.ClaudeSessionID, name); err != nil {
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
	// opencode's headless run has no multimodal channel (v1): image blocks
	// would be silently dropped, so refuse them up front. File uploads still
	// work (they ride as path mentions in the text).
	if len(body.Images) > 0 && s.engineOf(sess) == config.KindOpenCode {
		writeErr(w, http.StatusBadRequest, "opencode 引擎暂不支持图片消息（文件附件可用）")
		return
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
		// Replay source depends on the engine: claude sessions are parsed from
		// ~/.claude jsonl on disk, opencode sessions are exported via the CLI
		// (its transcript lives in opencode's own store).
		var extEvents []*store.Event
		if s.engineOf(sess) == config.KindOpenCode {
			if cli := s.cfg().CLIByName(sess.CLI); cli != nil {
				if oc, err := opencodehist.FetchSession(cli.Path, sess.ClaudeSessionID); err == nil {
					extEvents = oc.ToEvents()
				} else {
					log.Printf("opencode replay %s: %v", sess.ClaudeSessionID, err)
				}
			}
		} else {
			extEvents, _ = claudehist.Convert(sess.Cwd, sess.ClaudeSessionID)
			// 子代理 transcript 单独解析（subagents/agent-*.jsonl），补齐回放侧
			// 与实时流（parent_tool_use_id 透传）同形的子代理消息事件。
			if subEvents, err := claudehist.ConvertSubagents(sess.Cwd, sess.ClaudeSessionID); err == nil {
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
	if _, err := os.Stat(claudehist.HistoryPath(body.Cwd, body.ClaudeSessionID)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusNotFound, "claude session history not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	events, err := claudehist.Convert(body.Cwd, body.ClaudeSessionID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusNotFound, "claude session history not found")
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

// handleOpenCodeSessions lists sessions from the opencode engine's own store
// (via `opencode session list --format json`), the import source for the
// opencode tab. Requires the cli name so the backend knows which binary to
// ask; cwd/prefix filter like the claude history endpoint.
func (s *Server) handleOpenCodeSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cli := s.cfg().CLIByName(strings.TrimSpace(q.Get("cli")))
	if cli == nil || !cli.IsOpenCode() {
		writeErr(w, http.StatusBadRequest, "an opencode kind cli is required")
		return
	}
	cwd := q.Get("cwd")
	prefix := strings.ToLower(strings.TrimSpace(q.Get("prefix")))
	// `session list` 只返回进程 cwd 所属 opencode 项目桶的会话；cwd 参数选桶
	//（git 仓库根 = 该仓库桶，非 git 目录 = global 桶），空则用服务器自身 cwd。
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
	list, err := opencodehist.List(cli.Path, listDir)
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

// forkClaudeHistory duplicates the on-disk claude transcript of oldID under a
// freshly generated session id, rewriting every occurrence of the old id.
// This mirrors what `claude --resume <id> --fork-session` does, but without
// having to run a model turn.
func forkClaudeHistory(cwd, oldID string) (string, error) {
	srcPath := claudehist.HistoryPath(cwd, oldID)
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		return "", fmt.Errorf("read claude history: %w", err)
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	newID := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	forked := bytes.ReplaceAll(raw, []byte(oldID), []byte(newID))
	if err := os.WriteFile(claudehist.HistoryPath(cwd, newID), forked, 0o600); err != nil {
		return "", fmt.Errorf("write forked history: %w", err)
	}
	return newID, nil
}

// handleFork creates a new session as a real branch of the source: the
// on-disk claude transcript is duplicated under a new ClaudeSessionID (the
// equivalent of `claude --resume <id> --fork-session`), so the fork and the
// source diverge from this point on and resume independently.
func (s *Server) handleFork(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	src := s.st.Get(id)
	if src == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if s.engineOf(src) == config.KindOpenCode {
		writeErr(w, http.StatusBadRequest, "opencode 引擎暂不支持 fork")
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
	forkedID, err := forkClaudeHistory(src.Cwd, src.ClaudeSessionID)
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
func (s *Server) handleRewind(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.st.Get(id)
	if sess == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if s.engineOf(sess) == config.KindOpenCode {
		writeErr(w, http.StatusBadRequest, "opencode 引擎不支持 rewind（checkpoint 仅 claude 会话产生）")
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
