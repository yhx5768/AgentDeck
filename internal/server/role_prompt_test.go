package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cc-webui/internal/store"
)

func patchSession(t *testing.T, s *Server, id string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/"+id, bytes.NewReader(raw))
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	s.handlePatchSession(w, req)
	return w
}

func createSession(t *testing.T, s *Server, st *store.Store, body map[string]any) *store.Session {
	t.Helper()
	if body["cli"] == nil {
		body["cli"] = "claude"
		body["provider"] = "gw"
		body["model"] = "m1"
	}
	if body["cwd"] == nil {
		body["cwd"] = t.TempDir()
	}
	w := postCreateSession(t, s, body)
	if w.Code != http.StatusOK {
		t.Fatalf("create: code = %d, body=%s", w.Code, w.Body.String())
	}
	var view sessionView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal view: %v", err)
	}
	sess := st.Get(view.Session.ID)
	if sess == nil {
		t.Fatal("session not persisted")
	}
	return sess
}

// TestCreateSessionRolePrompt pins the create path: the field is stored and
// echoed in the session view (the JSON contract agentdeck.py and the UI use).
func TestCreateSessionRolePrompt(t *testing.T) {
	s, st := createResidentServer(t)
	const rp = "你是 cc-webui 的 worker，只做被指派的任务。"
	w := postCreateSession(t, s, map[string]any{
		"cli": "claude", "provider": "gw", "model": "m1", "cwd": t.TempDir(),
		"role_prompt": rp,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create: code = %d, body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["role_prompt"] != rp {
		t.Fatalf("view role_prompt = %v, want %q", body["role_prompt"], rp)
	}
	var view sessionView
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if got := st.Get(view.Session.ID).RolePrompt; got != rp {
		t.Fatalf("stored role_prompt = %q", got)
	}
}

// TestCreateSessionRolePromptSizeLimit pins the 32 KiB cap at both sides of the
// boundary.
func TestCreateSessionRolePromptSizeLimit(t *testing.T) {
	s, _ := createResidentServer(t)
	for _, c := range []struct {
		size int
		code int
	}{
		{maxRolePromptBytes, http.StatusOK},
		{maxRolePromptBytes + 1, http.StatusBadRequest},
	} {
		w := postCreateSession(t, s, map[string]any{
			"cli": "claude", "provider": "gw", "model": "m1", "cwd": t.TempDir(),
			"role_prompt": strings.Repeat("x", c.size),
		})
		if w.Code != c.code {
			t.Fatalf("size %d: code = %d, want %d (body=%s)", c.size, w.Code, c.code, w.Body.String())
		}
	}
}

// TestPatchSessionRolePrompt covers the update path: set, clear with an empty
// string, and a patch that also carries a fast-path flag (unread) — the latter
// must not let the flags-only shortcut swallow the role prompt.
func TestPatchSessionRolePrompt(t *testing.T) {
	s, st := createResidentServer(t)
	sess := createSession(t, s, st, map[string]any{"name": "worker"})

	w := patchSession(t, s, sess.ID, map[string]any{"role_prompt": "第一条规则"})
	if w.Code != http.StatusOK {
		t.Fatalf("set: code = %d, body=%s", w.Code, w.Body.String())
	}
	if got := st.Get(sess.ID).RolePrompt; got != "第一条规则" {
		t.Fatalf("stored role_prompt = %q", got)
	}

	w = patchSession(t, s, sess.ID, map[string]any{"unread": false, "role_prompt": "带旗标一起改"})
	if w.Code != http.StatusOK {
		t.Fatalf("flag+role: code = %d, body=%s", w.Code, w.Body.String())
	}
	if got := st.Get(sess.ID).RolePrompt; got != "带旗标一起改" {
		t.Fatalf("flags-only fast path swallowed the role prompt: %q", got)
	}

	w = patchSession(t, s, sess.ID, map[string]any{"role_prompt": ""})
	if w.Code != http.StatusOK {
		t.Fatalf("clear: code = %d, body=%s", w.Code, w.Body.String())
	}
	if got := st.Get(sess.ID).RolePrompt; got != "" {
		t.Fatalf("empty string must clear, got %q", got)
	}
}

// TestPatchSessionRolePromptSizeLimit: an oversized value is rejected and the
// stored text is left untouched.
func TestPatchSessionRolePromptSizeLimit(t *testing.T) {
	s, st := createResidentServer(t)
	sess := createSession(t, s, st, map[string]any{"role_prompt": "keep me"})
	w := patchSession(t, s, sess.ID, map[string]any{
		"role_prompt": strings.Repeat("x", maxRolePromptBytes+1),
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	if got := st.Get(sess.ID).RolePrompt; got != "keep me" {
		t.Fatalf("rejected patch changed the stored value: %q", got)
	}
}

// TestCreateSessionRolePromptOtherEngines: non-append engines still store the
// value (the API does not gate the field); only the injection differs per
// engine (opencode has no append semantics — see AGENTS.md §14).
func TestCreateSessionRolePromptOtherEngines(t *testing.T) {
	s, st := createResidentServer(t)
	sess := createSession(t, s, st, map[string]any{
		"cli": "oc", "provider": "oc-p", "role_prompt": "opencode 上暂不生效",
	})
	if got := st.Get(sess.ID).RolePrompt; got != "opencode 上暂不生效" {
		t.Fatalf("role_prompt = %q", got)
	}
}
