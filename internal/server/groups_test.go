package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"cc-webui/internal/config"
	"cc-webui/internal/runner"
	"cc-webui/internal/store"
)

// groupTestServer wires a Server with a store and a synthetic config: two
// claude providers (so a provider switch is testable), one opencode pair (so
// engine mismatch is testable) and a qoder pair.
func groupTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		DataDir: dir,
		CLIs: []config.CLI{
			{Name: "claude", Path: "claude"},
			{Name: "oc", Kind: config.KindOpenCode, Path: "opencode"},
			{Name: "qoder", Kind: config.KindQoder, Path: "qoderclicn"},
		},
		Providers: []config.Provider{
			{Name: "gw-a", Models: []config.Model{{Name: "model-a1"}, {Name: "model-a2"}}},
			{Name: "gw-b", Models: []config.Model{{Name: "model-b1"}}},
			{Name: "oc-p", Kind: config.KindOpenCode, Models: []config.Model{{Name: "kimi-k3"}}},
			{Name: "qoder-p", Kind: config.KindQoder, Models: []config.Model{}},
		},
	}
	cm := config.NewManager(filepath.Join(dir, "config.yaml"), cfg)
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return &Server{cm: cm, st: st, mgr: runner.NewManager(cm, st)}, st
}

func doJSON(t *testing.T, s *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	// Call handlers directly, so the mux never gets to populate the wildcards
	// handlePatchGroupMember reads — set them from the path by hand.
	if parts := strings.Split(strings.Trim(path, "/"), "/"); len(parts) == 5 && parts[3] == "members" {
		req.SetPathValue("id", parts[2])
		req.SetPathValue("memberId", parts[4])
	}
	w := httptest.NewRecorder()
	switch {
	case method == http.MethodPost && path == "/api/groups":
		s.handleCreateGroup(w, req)
	case method == http.MethodPatch:
		s.handlePatchGroupMember(w, req)
	default:
		t.Fatalf("unhandled %s %s", method, path)
	}
	return w
}

func decodeGroup(t *testing.T, w *httptest.ResponseRecorder) groupView {
	t.Helper()
	var g groupView
	if err := json.Unmarshal(w.Body.Bytes(), &g); err != nil {
		t.Fatalf("unmarshal group view: %v (body=%s)", err, w.Body.String())
	}
	return g
}

// createGroup is the fixture every patch test starts from.
func createGroup(t *testing.T, s *Server) groupView {
	t.Helper()
	w := doJSON(t, s, http.MethodPost, "/api/groups", map[string]any{
		"name": "g",
		"cwd":  t.TempDir(),
		"members": []map[string]any{
			{"name": "alice", "cli": "claude", "provider": "gw-a", "model": "model-a1", "effort": "low"},
			{"name": "bob", "cli": "claude", "provider": "gw-b"},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create group: %d %s", w.Code, w.Body.String())
	}
	return decodeGroup(t, w)
}

// The create form has to carry the per-member model and reasoning level all the
// way down: the member record the group view renders AND the shadow session that
// dispatch actually spawns.
func TestCreateGroupCarriesModelAndEffort(t *testing.T) {
	s, st := groupTestServer(t)
	g := createGroup(t, s)

	mb := g.Members[0]
	if mb.Model != "model-a1" || mb.Effort != "low" {
		t.Fatalf("member record lost profile: model=%q effort=%q", mb.Model, mb.Effort)
	}
	sess := st.Get(mb.ID)
	if sess == nil {
		t.Fatal("shadow session missing")
	}
	if sess.GroupID != g.ID {
		t.Fatalf("shadow session group id = %q, want %q", sess.GroupID, g.ID)
	}
	if sess.ModelOverride != "model-a1" || sess.EffortOverride != "low" {
		t.Fatalf("shadow session profile = model %q / effort %q", sess.ModelOverride, sess.EffortOverride)
	}
	// Empty effort stays empty (= inherit), never a silently filled-in default.
	if bob := st.Get(g.Members[1].ID); bob.EffortOverride != "" {
		t.Fatalf("member without effort got %q", bob.EffortOverride)
	}
}

func TestCreateGroupRejectsBadProfile(t *testing.T) {
	cases := []struct {
		name   string
		member map[string]any
	}{
		{"effort not in claude set", map[string]any{"name": "a", "cli": "claude", "provider": "gw-a", "effort": "turbo"}},
		{"model unknown for provider", map[string]any{"name": "a", "cli": "claude", "provider": "gw-a", "model": "model-b1"}},
		{"engine mismatch", map[string]any{"name": "a", "cli": "claude", "provider": "oc-p"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := groupTestServer(t)
			w := doJSON(t, s, http.MethodPost, "/api/groups", map[string]any{
				"name": "g", "cwd": t.TempDir(), "members": []map[string]any{c.member},
			})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
			}
		})
	}
}

// A runtime edit must land in BOTH records: the shadow session is what the next
// dispatch spawns, the group meta is what the group view shows. Writing only one
// leaves the UI and the runner disagreeing.
func TestPatchGroupMemberUpdatesSessionAndMeta(t *testing.T) {
	s, st := groupTestServer(t)
	g := createGroup(t, s)
	mb := g.Members[0]

	w := doJSON(t, s, http.MethodPatch, "/api/groups/"+g.ID+"/members/"+mb.ID, map[string]any{
		"model": "model-a2", "effort": "max",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	got := decodeGroup(t, w).Members[0]
	if got.Model != "model-a2" || got.Effort != "max" {
		t.Fatalf("meta after patch: model=%q effort=%q", got.Model, got.Effort)
	}
	sess := st.Get(mb.ID)
	if sess.ModelOverride != "model-a2" || sess.EffortOverride != "max" {
		t.Fatalf("shadow session after patch: model=%q effort=%q", sess.ModelOverride, sess.EffortOverride)
	}

	// Reload from disk: the edit has to survive a restart, not just live in RAM.
	reread, err := s.mgr.ReadGroupMeta(g.ID)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if m := reread.Members[0]; m.Model != "model-a2" || m.Effort != "max" {
		t.Fatalf("meta.json on disk: model=%q effort=%q", m.Model, m.Effort)
	}
}

// Switching provider without naming a model must fall back to that provider's
// default instead of 400-ing on a model that no longer exists there (same rule
// as patching a single session's provider).
func TestPatchGroupMemberProviderSwitchDropsStaleModel(t *testing.T) {
	s, st := groupTestServer(t)
	g := createGroup(t, s)
	mb := g.Members[0]

	w := doJSON(t, s, http.MethodPatch, "/api/groups/"+g.ID+"/members/"+mb.ID, map[string]any{
		"provider": "gw-b",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	got := decodeGroup(t, w).Members[0]
	if got.Provider != "gw-b" || got.Model != "" {
		t.Fatalf("member after provider switch: provider=%q model=%q", got.Provider, got.Model)
	}
	if sess := st.Get(mb.ID); sess.Provider != "gw-b" || sess.ModelOverride != "" {
		t.Fatalf("shadow session after switch: provider=%q model=%q", sess.Provider, sess.ModelOverride)
	}
}

func TestPatchGroupMemberRejects(t *testing.T) {
	t.Run("member of another group", func(t *testing.T) {
		s, _ := groupTestServer(t)
		g := createGroup(t, s)
		// A real session that is not one of this group's members.
		other := &store.Session{ID: "outsider", CLI: "claude", Provider: "gw-a", GroupID: "other-group"}
		if err := s.st.Upsert(other, nil); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		w := doJSON(t, s, http.MethodPatch, "/api/groups/"+g.ID+"/members/outsider", map[string]any{"effort": "high"})
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body=%s)", w.Code, w.Body.String())
		}
	})

	t.Run("unknown member id", func(t *testing.T) {
		s, _ := groupTestServer(t)
		g := createGroup(t, s)
		w := doJSON(t, s, http.MethodPatch, "/api/groups/"+g.ID+"/members/nope", map[string]any{"effort": "high"})
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body=%s)", w.Code, w.Body.String())
		}
	})

	t.Run("engine mismatch", func(t *testing.T) {
		s, _ := groupTestServer(t)
		g := createGroup(t, s)
		w := doJSON(t, s, http.MethodPatch, "/api/groups/"+g.ID+"/members/"+g.Members[0].ID, map[string]any{"provider": "oc-p"})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
		}
	})

	t.Run("invalid effort for engine", func(t *testing.T) {
		s, _ := groupTestServer(t)
		g := createGroup(t, s)
		w := doJSON(t, s, http.MethodPatch, "/api/groups/"+g.ID+"/members/"+g.Members[0].ID, map[string]any{"effort": "turbo"})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
		}
	})

	t.Run("empty patch", func(t *testing.T) {
		s, _ := groupTestServer(t)
		g := createGroup(t, s)
		w := doJSON(t, s, http.MethodPatch, "/api/groups/"+g.ID+"/members/"+g.Members[0].ID, map[string]any{})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
		}
	})
}
