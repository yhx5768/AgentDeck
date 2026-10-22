package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cc-webui/internal/codexhist"
	"cc-webui/internal/config"
	"cc-webui/internal/mcodehist"
	"cc-webui/internal/runner"
	"cc-webui/internal/store"
)

// Group chat HTTP layer. Groups own shadow member sessions (store.GroupID,
// hidden from the sidebar); dispatch/wait/log live in runner/group.go. This
// file is validation, member lifecycle, @ routing and the group SSE stream.

// resolveProfile resolves a cli/provider pair and rejects an engine mismatch.
// A session (and therefore a group member, whose shadow session is one) binds
// to a single engine — see AGENTS.md §8.
func (s *Server) resolveProfile(cliName, provName string) (*config.CLI, *config.Provider, error) {
	cli := s.cfg().CLIByName(cliName)
	if cli == nil {
		return nil, nil, fmt.Errorf("unknown cli: %s", cliName)
	}
	prov := s.cfg().ProviderByName(provName)
	if prov == nil {
		return nil, nil, fmt.Errorf("unknown provider: %s", provName)
	}
	if prov.Kind != cli.Kind {
		return nil, nil, fmt.Errorf("engine mismatch: cli %q (%s) with provider %q (%s)", cli.Name, cli.Kind, prov.Name, prov.Kind)
	}
	return cli, prov, nil
}

// validateEffort checks a reasoning level against the engine that will run it:
// claude takes the fixed --effort set, opencode any well-formed --variant key,
// qoder its own --reasoning-effort set, mcode a per-model short list read from
// its runtime config (only the CLI itself knows those, and it would reject an
// unknown one after the run started). model is the provider-relative override
// ("" = provider default) and is only read for mcode.
func (s *Server) validateEffort(cli *config.CLI, prov *config.Provider, model, effort string) error {
	kind := config.KindClaude
	if cli != nil {
		kind = cli.Kind
	}
	if !effortOKFor(kind, effort) {
		return fmt.Errorf("invalid effort: %s", effort)
	}
	if prov == nil || effort == "" {
		return nil
	}
	m := prov.EffectiveModel(model)
	switch kind {
	case config.KindMcode:
		cat, err := mcodehist.LoadCatalog(mcodeProviderIDFromModel(m, prov.Name))
		if mm := cat.Find(mcodehist.BareModelID(m)); err == nil && mm != nil && len(mm.Efforts) > 0 &&
			!mcodehist.EffortOK(cat, mcodehist.BareModelID(m), effort) {
			return fmt.Errorf("模型 %s 不支持推理档位 %q（可选：%s）", m, effort, strings.Join(mm.Efforts, ", "))
		}
	case config.KindCodex:
		if cli == nil {
			return nil
		}
		cat, err := codexhist.LoadCatalogFor(cli.Path, codexHome(cli), cli.ModelProvider, false)
		if mm := cat.Find(m); err == nil && mm != nil && len(mm.Efforts) > 0 && !cat.EffortOK(m, effort) {
			return fmt.Errorf("模型 %s 不支持推理档位 %q（可选：%s）", m, effort, strings.Join(mm.Efforts, ", "))
		}
	}
	return nil
}

// groupView is the wire shape for a group.
type groupView struct {
	ID        string               `json:"id"`
	Name      string               `json:"name"`
	Cwd       string               `json:"cwd"`
	Members   []runner.GroupMember `json:"members"`
	Running   bool                 `json:"running"`
	UpdatedAt time.Time            `json:"updated_at"`
	// PendingAsks aggregates the members' outstanding AskUserQuestion prompts —
	// a member's card only shows in the group view, so without this the group
	// row would be as silent as an unopened session row.
	PendingAsks []pendingAskView `json:"pending_asks"`
}

func (s *Server) groupViewOf(meta *runner.GroupMeta) groupView {
	return groupView{
		ID: meta.ID, Name: meta.Name, Cwd: meta.Cwd, Members: meta.Members,
		Running: s.mgr.GroupBusy(meta.ID), UpdatedAt: meta.UpdatedAt,
		PendingAsks: s.groupPendingAsksOf(meta.Members),
	}
}

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	out := []groupView{}
	entries, err := os.ReadDir(s.mgr.GroupsDir())
	if err != nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, err := s.mgr.ReadGroupMeta(e.Name())
		if err != nil {
			continue
		}
		out = append(out, s.groupViewOf(meta))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string `json:"name"`
		Cwd     string `json:"cwd"`
		Members []struct {
			Name     string `json:"name"`
			CLI      string `json:"cli"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Effort   string `json:"effort"`
		} `json:"members"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		writeErr(w, http.StatusBadRequest, "group name is required")
		return
	}
	if len(body.Members) == 0 || len(body.Members) > 8 {
		writeErr(w, http.StatusBadRequest, "1-8 members required")
		return
	}
	resolvedCwd, err := runner.ResolveCwd(strings.TrimSpace(body.Cwd))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad cwd: "+err.Error())
		return
	}
	seen := map[string]bool{}
	type validated struct {
		name   string
		cli    string
		prov   string
		model  string
		effort string
	}
	var members []validated
	for _, mb := range body.Members {
		name := strings.TrimSpace(mb.Name)
		if name == "" || strings.ContainsAny(name, " @\t\n") {
			writeErr(w, http.StatusBadRequest, "member names must be non-empty without spaces or @")
			return
		}
		key := strings.ToLower(name)
		if seen[key] {
			writeErr(w, http.StatusBadRequest, "duplicate member name: "+name)
			return
		}
		seen[key] = true
		cli, prov, err := s.resolveProfile(mb.CLI, mb.Provider)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if mb.Model != "" && !s.modelKnown(cli, prov, mb.Model) {
			writeErr(w, http.StatusBadRequest, "unknown model for provider "+prov.Name+": "+mb.Model)
			return
		}
		if err := s.validateEffort(cli, prov, mb.Model, mb.Effort); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		members = append(members, validated{name: name, cli: cli.Name, prov: prov.Name, model: mb.Model, effort: mb.Effort})
	}
	gid := newID()
	now := time.Now()
	meta := &runner.GroupMeta{ID: gid, Name: body.Name, Cwd: resolvedCwd, CreatedAt: now}
	// Shadow sessions first; on any failure roll back the ones created.
	created := []string{}
	rollback := func() {
		for _, id := range created {
			s.mgr.TeardownResident(id)
			s.mgr.Stop(id)
			s.mgr.ClearPermissions(id)
			_ = s.st.Delete(id)
		}
		_ = os.RemoveAll(filepath.Join(s.mgr.GroupsDir(), gid))
	}
	for _, mb := range members {
		sess := &store.Session{
			ID: newID(), Name: body.Name + "/" + mb.name,
			CLI: mb.cli, Provider: mb.prov, ModelOverride: mb.model,
			EffortOverride: mb.effort,
			Cwd:            resolvedCwd, GroupID: gid,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.st.Upsert(sess, nil); err != nil {
			rollback()
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		created = append(created, sess.ID)
		meta.Members = append(meta.Members, runner.GroupMember{
			ID: sess.ID, Name: mb.name, CLI: mb.cli, Provider: mb.prov, Model: mb.model, Effort: mb.effort,
		})
	}
	if err := s.mgr.WriteGroupMeta(meta); err != nil {
		rollback()
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.groupViewOf(meta))
}

func (s *Server) handleGetGroup(w http.ResponseWriter, r *http.Request) {
	gid := r.PathValue("id")
	meta, err := s.mgr.ReadGroupMeta(gid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "group not found")
		return
	}
	log_, err := s.mgr.ReadGroupLog(gid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if log_ == nil {
		log_ = []runner.GroupEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": s.groupViewOf(meta), "transcript": log_})
}

// handlePatchGroupMember re-profiles one member at runtime: CLI, provider,
// model and reasoning level. Two records carry the member's profile — the
// shadow session (store.Session, what dispatch actually reads) and the group
// meta member (what the group view renders) — so both are written here.
//
// The edit applies from the NEXT dispatch: a leg already in flight was spawned
// with the old profile and is left alone (same rule as patching a running
// single session's model). Nothing is blocked while the group is busy.
func (s *Server) handlePatchGroupMember(w http.ResponseWriter, r *http.Request) {
	meta, err := s.mgr.ReadGroupMeta(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "group not found")
		return
	}
	memberID := r.PathValue("memberId")
	idx := -1
	for i := range meta.Members {
		if meta.Members[i].ID == memberID {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "member not found in group")
		return
	}
	mb := &meta.Members[idx]

	var body struct {
		CLI      *string `json:"cli"`
		Provider *string `json:"provider"`
		Model    *string `json:"model"`
		Effort   *string `json:"effort"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if body.CLI == nil && body.Provider == nil && body.Model == nil && body.Effort == nil {
		writeErr(w, http.StatusBadRequest, "nothing to update")
		return
	}
	cliName, provName, model, effort := mb.CLI, mb.Provider, mb.Model, mb.Effort
	if body.CLI != nil {
		cliName = strings.TrimSpace(*body.CLI)
	}
	if body.Provider != nil {
		provName = strings.TrimSpace(*body.Provider)
	}
	if body.Effort != nil {
		effort = *body.Effort
	}
	cli, prov, err := s.resolveProfile(cliName, provName)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Model != nil {
		model = *body.Model
	} else if body.Provider != nil && prov.ModelByName(model) == nil {
		// Provider changed without a new model: the old override does not exist
		// in the new provider, so fall back to its default model (same rule as
		// patching a session's provider).
		model = ""
	}
	if model != "" && !s.modelKnown(cli, prov, model) {
		writeErr(w, http.StatusBadRequest, "unknown model for provider "+prov.Name+": "+model)
		return
	}
	if err := s.validateEffort(cli, prov, model, effort); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	sess := s.st.Get(memberID)
	if sess == nil || sess.GroupID != meta.ID {
		// The shadow session is gone (or was never ours) — the meta copy alone
		// would show a profile that never runs, so refuse instead of half-writing.
		writeErr(w, http.StatusNotFound, "member session not found")
		return
	}
	if err := s.st.Upsert(sess, func(cur *store.Session) {
		cur.CLI = cli.Name
		cur.Provider = prov.Name
		cur.ModelOverride = model
		cur.EffortOverride = effort
		cur.UpdatedAt = time.Now()
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	mb.CLI, mb.Provider, mb.Model, mb.Effort = cli.Name, prov.Name, model, effort
	meta.UpdatedAt = time.Now()
	if err := s.mgr.WriteGroupMeta(meta); err != nil {
		// The shadow session already carries the new profile, so the next
		// dispatch is correct either way; only the group view's copy is stale
		// after a restart. Say so instead of reporting a plain failure.
		log.Printf("group %s member %s: write meta after profile patch: %v", meta.ID, mb.Name, err)
		writeErr(w, http.StatusInternalServerError, "成员配置已更新，但群聊档案落盘失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.groupViewOf(meta))
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	gid := r.PathValue("id")
	meta, err := s.mgr.ReadGroupMeta(gid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "group not found")
		return
	}
	s.mgr.GroupStop(gid)
	// Wait for the turn to actually drain before removing the dir: member
	// legs append their transcript entries on completion, which would
	// otherwise resurrect the directory after RemoveAll.
	for i := 0; i < 40 && s.mgr.GroupBusy(gid); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	for _, mb := range meta.Members {
		s.mgr.TeardownResident(mb.ID)
		s.mgr.Stop(mb.ID)
		s.mgr.ClearPermissions(mb.ID)
		_ = s.st.Delete(mb.ID)
	}
	_ = os.RemoveAll(filepath.Join(s.mgr.GroupsDir(), gid))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// parseGroupRecipients resolves @name/@all mentions (case-insensitive).
// No mention = all members. Unknown mention = error.
func parseGroupRecipients(meta *runner.GroupMeta, text string) ([]string, error) {
	byName := map[string]string{}
	for _, mb := range meta.Members {
		byName[strings.ToLower(mb.Name)] = mb.ID
	}
	var out []string
	seen := map[string]bool{}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "@all") {
		for _, mb := range meta.Members {
			out = append(out, mb.ID)
		}
		return out, nil
	}
	for _, tok := range strings.Fields(text) {
		if !strings.HasPrefix(tok, "@") || len(tok) < 2 {
			continue
		}
		name := strings.Trim(strings.TrimPrefix(tok, "@"), ".,;:!?()[]\"'")
		id, ok := byName[strings.ToLower(name)]
		if !ok {
			return nil, &unknownMemberError{name}
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		for _, mb := range meta.Members {
			out = append(out, mb.ID)
		}
	}
	return out, nil
}

type unknownMemberError struct{ name string }

func (e *unknownMemberError) Error() string { return "unknown member @" + e.name }

func (s *Server) handleGroupMessage(w http.ResponseWriter, r *http.Request) {
	gid := r.PathValue("id")
	meta, err := s.mgr.ReadGroupMeta(gid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "group not found")
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.Text) == "" {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}
	recipients, err := parseGroupRecipients(meta, body.Text)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	turnID, err := s.mgr.GroupSend(gid, strings.TrimSpace(body.Text), recipients)
	if err != nil {
		if err.Error() == "turn_busy" {
			writeErr(w, http.StatusConflict, "group turn in flight, wait for it to finish")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"turn_id": turnID, "recipients": recipients})
}

func (s *Server) handleGroupStop(w http.ResponseWriter, r *http.Request) {
	gid := r.PathValue("id")
	if _, err := s.mgr.ReadGroupMeta(gid); err != nil {
		writeErr(w, http.StatusNotFound, "group not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": s.mgr.GroupStop(gid)})
}

// handleGroupEvents replays the group transcript then streams live group
// events (member texts, approvals relayed from members, turn end). Same
// per-connection seq renumbering as session events.
func (s *Server) handleGroupEvents(w http.ResponseWriter, r *http.Request) {
	gid := r.PathValue("id")
	if _, err := s.mgr.ReadGroupMeta(gid); err != nil {
		writeErr(w, http.StatusNotFound, "group not found")
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
	w.Header().Set("X-Accel-Buffering", "no")

	log_, _ := s.mgr.ReadGroupLog(gid)
	ch, unsubscribe := s.mgr.SubscribeGroup(gid)
	defer unsubscribe()

	var sendSeq int64
	send := func(kind string, data any) bool {
		raw, err := json.Marshal(data)
		if err != nil {
			return true
		}
		sendSeq++
		ev := store.Event{Seq: sendSeq, Ts: time.Now(), Kind: kind, Data: raw}
		out, err := json.Marshal(&ev)
		if err != nil {
			return true
		}
		if _, err := w.Write([]byte("data: " + string(out) + "\n\n")); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for _, e := range log_ {
		var payload runner.GroupEvent
		switch e.Kind {
		case "user":
			payload = runner.GroupEvent{Type: "user", TurnID: e.TurnID, Recipients: e.Recipients, Text: e.Text, Ts: e.Ts}
		case "member":
			payload = runner.GroupEvent{Type: "member_end", TurnID: e.TurnID, MemberID: e.MemberID, MemberName: e.MemberName, Text: e.Text, OK: true, Ts: e.Ts}
		default:
			continue
		}
		if !send("group", payload) {
			return
		}
	}
	raw, _ := json.Marshal(map[string]any{})
	sendSeq++
	if _, err := w.Write([]byte("data: " + string(mustMarshal(store.Event{Seq: sendSeq, Ts: time.Now(), Kind: "replay_end", Data: raw})) + "\n\n")); err != nil {
		return
	}
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			// Forward member approval prompts with their own kinds so the
			// shared card renderer handles them; group timeline events ride
			// Kind "group".
			out, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			// Renumber seq per connection (frontend dedups by seq).
			var decoded store.Event
			if err := json.Unmarshal(out, &decoded); err != nil {
				continue
			}
			sendSeq++
			decoded.Seq = sendSeq
			final, err := json.Marshal(&decoded)
			if err != nil {
				continue
			}
			if _, err := w.Write([]byte("data: " + string(final) + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func mustMarshal(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}
