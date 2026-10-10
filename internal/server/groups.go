package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cc-webui/internal/runner"
	"cc-webui/internal/store"
)

// Group chat HTTP layer. Groups own shadow member sessions (store.GroupID,
// hidden from the sidebar); dispatch/wait/log live in runner/group.go. This
// file is validation, member lifecycle, @ routing and the group SSE stream.

// groupView is the wire shape for a group.
type groupView struct {
	ID        string               `json:"id"`
	Name      string               `json:"name"`
	Cwd       string               `json:"cwd"`
	Members   []runner.GroupMember `json:"members"`
	Running   bool                 `json:"running"`
	UpdatedAt time.Time            `json:"updated_at"`
}

func (s *Server) groupViewOf(meta *runner.GroupMeta) groupView {
	return groupView{ID: meta.ID, Name: meta.Name, Cwd: meta.Cwd, Members: meta.Members, Running: s.mgr.GroupBusy(meta.ID), UpdatedAt: meta.UpdatedAt}
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
		name string
		cli  string
		prov string
		model string
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
		cli := s.cfg().CLIByName(mb.CLI)
		if cli == nil {
			writeErr(w, http.StatusBadRequest, "unknown cli: "+mb.CLI)
			return
		}
		prov := s.cfg().ProviderByName(mb.Provider)
		if prov == nil {
			writeErr(w, http.StatusBadRequest, "unknown provider: "+mb.Provider)
			return
		}
		if prov.Kind != cli.Kind {
			writeErr(w, http.StatusBadRequest, "engine mismatch for member "+name)
			return
		}
		if mb.Model != "" && !s.modelKnown(cli, prov, mb.Model) {
			writeErr(w, http.StatusBadRequest, "unknown model for provider "+prov.Name+": "+mb.Model)
			return
		}
		members = append(members, validated{name: name, cli: cli.Name, prov: prov.Name, model: mb.Model})
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
			Cwd: resolvedCwd, GroupID: gid,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.st.Upsert(sess, nil); err != nil {
			rollback()
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		created = append(created, sess.ID)
		meta.Members = append(meta.Members, runner.GroupMember{
			ID: sess.ID, Name: mb.name, CLI: mb.cli, Provider: mb.prov, Model: mb.model,
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
