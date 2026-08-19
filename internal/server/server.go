package server

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"cc-webui/internal/claudehist"
	"cc-webui/internal/config"
	"cc-webui/internal/runner"
	"cc-webui/internal/store"
)

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

const cookieName = "ccgw_token"

type Server struct {
	cfg    *config.Config
	st     *store.Store
	mgr    *runner.Manager
	static fs.FS
	mux    *http.ServeMux
}

func New(cfg *config.Config, st *store.Store, mgr *runner.Manager, static fs.FS) *Server {
	s := &Server{cfg: cfg, st: st, mgr: mgr, static: static, mux: http.NewServeMux()}
	s.routes()
	return s
}

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
	s.mux.Handle("POST /api/sessions/{id}/stop", s.auth(s.handleStop))
	s.mux.Handle("GET /api/sessions/{id}/events", s.auth(s.handleEvents))
	s.mux.Handle("GET /api/claude-sessions", s.auth(s.handleClaudeSessions))
	s.mux.Handle("POST /api/sessions/import", s.auth(s.handleImport))
	s.mux.Handle("POST /api/sessions/{id}/fork", s.auth(s.handleFork))
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
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.GatewayToken)) == 1
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
	if subtle.ConstantTimeCompare([]byte(body.Token), []byte(s.cfg.GatewayToken)) != 1 {
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
		Name           string   `json:"name"`
		Path           string   `json:"path"`
		Args           []string `json:"args"`
		PermissionMode string   `json:"permission_mode"`
		EnvKeys        []string `json:"env_keys"`
	}
	out := []item{}
	for _, c := range s.cfg.CLIs {
		it := item{Name: c.Name, Path: c.Path, Args: c.Args, PermissionMode: c.PermissionMode}
		for k := range c.Env {
			it.EnvKeys = append(it.EnvKeys, k)
		}
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
		EnvKeys []string `json:"env_keys"`
		Models  []model  `json:"models"`
	}
	out := []prov{}
	for _, p := range s.cfg.Providers {
		item := prov{Name: p.Name}
		for k := range p.Env {
			item.EnvKeys = append(item.EnvKeys, k)
		}
		for _, m := range p.Models {
			mm := model{Name: m.Name, Input: m.Input}
			if mm.Input == nil {
				mm.Input = []string{"text"}
			}
			for k := range m.Env {
				mm.EnvKeys = append(mm.EnvKeys, k)
			}
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
		EnvKeys []string `json:"env_keys"`
		Models  []model  `json:"models"`
	}
	type cli struct {
		Name           string   `json:"name"`
		Path           string   `json:"path"`
		Args           []string `json:"args"`
		PermissionMode string   `json:"permission_mode"`
		EnvKeys        []string `json:"env_keys"`
	}
	out := struct {
		CLIs      []cli  `json:"clis"`
		Providers []prov `json:"providers"`
	}{}
	for _, c := range s.cfg.CLIs {
		it := cli{Name: c.Name, Path: c.Path, Args: c.Args, PermissionMode: c.PermissionMode}
		for k := range c.Env {
			it.EnvKeys = append(it.EnvKeys, k)
		}
		out.CLIs = append(out.CLIs, it)
	}
	for _, p := range s.cfg.Providers {
		item := prov{Name: p.Name}
		for k := range p.Env {
			item.EnvKeys = append(item.EnvKeys, k)
		}
		for _, m := range p.Models {
			mm := model{Name: m.Name, Input: m.Input}
			if mm.Input == nil {
				mm.Input = []string{"text"}
			}
			for k := range m.Env {
				mm.EnvKeys = append(mm.EnvKeys, k)
			}
			item.Models = append(item.Models, mm)
		}
		out.Providers = append(out.Providers, item)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	home, _ := os.UserHomeDir()
	writeJSON(w, http.StatusOK, map[string]any{
		"default_cwd": s.cfg.DefaultCwd,
		"data_dir":    s.cfg.DataDir,
		"home":        home,
	})
}

// ---- sessions ----

type sessionView struct {
	*store.Session
	Status        runner.Status `json:"status"`
	ResumeCommand string        `json:"resume_command,omitempty"`
}

func (s *Server) view(sess *store.Session) sessionView {
	return sessionView{Session: sess, Status: s.mgr.GetStatus(sess.ID), ResumeCommand: s.resumeCommand(sess)}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// resumeCommand builds the shell command that resumes this session from a
// terminal, mirroring the session's CLI and effective env (--settings).
func (s *Server) resumeCommand(sess *store.Session) string {
	if sess.ClaudeSessionID == "" {
		return ""
	}
	cli := s.cfg.CLIByName(sess.CLI)
	prov := s.cfg.ProviderByName(sess.Provider)
	var cliPath string
	var settings string
	if cli != nil {
		cliPath = cli.Path
	}
	if cliPath == "" {
		cliPath = "claude"
	}
	if cli != nil && prov != nil {
		model := prov.EffectiveModel(sess.ModelOverride)
		if len(cli.EffectiveEnv(prov, model)) > 0 {
			settings = runner.SettingsPath(s.cfg.DataDir, cli.Name, prov.Name, model)
		}
	}
	cmd := "cd " + shellQuote(sess.Cwd) + " && " + shellQuote(cliPath) + " --resume " + sess.ClaudeSessionID
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
		PermissionMode string `json:"permission_mode"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if body.CLI == "" && len(s.cfg.CLIs) > 0 {
		body.CLI = s.cfg.CLIs[0].Name
	}
	if body.Provider == "" && len(s.cfg.Providers) > 0 {
		body.Provider = s.cfg.Providers[0].Name
	}
	cli := s.cfg.CLIByName(body.CLI)
	if cli == nil {
		writeErr(w, http.StatusBadRequest, "unknown cli: "+body.CLI)
		return
	}
	prov := s.cfg.ProviderByName(body.Provider)
	if prov == nil {
		writeErr(w, http.StatusBadRequest, "unknown provider: "+body.Provider)
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
	now := time.Now()
	sess := &store.Session{
		ID:             newID(),
		Name:           body.Name,
		CLI:            body.CLI,
		Provider:       body.Provider,
		Cwd:            body.Cwd,
		ModelOverride:  body.Model,
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
		PermissionMode *string `json:"permission_mode"`
		Archived       *bool   `json:"archived"`
		SortOrder      *int    `json:"sort_order"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if body.CLI != nil && s.cfg.CLIByName(*body.CLI) == nil {
		writeErr(w, http.StatusBadRequest, "unknown cli: "+*body.CLI)
		return
	}
	if body.Provider != nil && s.cfg.ProviderByName(*body.Provider) == nil {
		writeErr(w, http.StatusBadRequest, "unknown provider: "+*body.Provider)
		return
	}
	if body.Model != nil && *body.Model != "" {
		// Validate the model against the (possibly newly set) provider.
		provName := sess.Provider
		if body.Provider != nil {
			provName = *body.Provider
		}
		if prov := s.cfg.ProviderByName(provName); prov != nil && prov.ModelByName(*body.Model) == nil {
			writeErr(w, http.StatusBadRequest, "unknown model for provider "+provName+": "+*body.Model)
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
				if prov := s.cfg.ProviderByName(cur.Provider); prov != nil && prov.ModelByName(cur.ModelOverride) == nil {
					cur.ModelOverride = ""
				}
			}
		}
		if body.Model != nil {
			cur.ModelOverride = *body.Model
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
		cur.UpdatedAt = time.Now()
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.view(s.st.Get(sess.ID)))
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mgr.Stop(id)
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
		Text   string             `json:"text"`
		Images []runner.Image     `json:"images,omitempty"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if strings.TrimSpace(body.Text) == "" && len(body.Images) == 0 {
		writeErr(w, http.StatusBadRequest, "empty message")
		return
	}
	// Workspace gate: refuse turns until the session has a cwd.
	if sess.Cwd == "" {
		writeErr(w, http.StatusBadRequest, "select a workspace before sending messages")
		return
	}
	if err := s.mgr.Enqueue(id, runner.Message{Text: body.Text, Images: body.Images}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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
		claudeEvents, _ := claudehist.Convert(sess.Cwd, sess.ClaudeSessionID)
		merged = mergeStoredAndClaude(stored, claudeEvents)
	} else {
		for _, e := range stored {
			merged = append(merged, e)
		}
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
			if id, ok := msg["id"].(string); ok {
				return "assistant|" + id
			}
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
	if body.CLI == "" && len(s.cfg.CLIs) > 0 {
		body.CLI = s.cfg.CLIs[0].Name
	}
	if body.Provider == "" && len(s.cfg.Providers) > 0 {
		body.Provider = s.cfg.Providers[0].Name
	}
	if s.cfg.CLIByName(body.CLI) == nil {
		writeErr(w, http.StatusBadRequest, "unknown cli: "+body.CLI)
		return
	}
	if s.cfg.ProviderByName(body.Provider) == nil {
		writeErr(w, http.StatusBadRequest, "unknown provider: "+body.Provider)
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
	if err := s.st.Upsert(sess, nil); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.view(sess))
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
	log.Printf("cc-webui listening on %s", s.cfg.Listen)
	return http.ListenAndServe(s.cfg.Listen, s.mux)
}
