package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestCreateSessionParentID covers the session hierarchy record: a created
// session keeps an explicit parent_id, and an unknown parent is rejected.
func TestCreateSessionParentID(t *testing.T) {
	s, st := createResidentServer(t)
	parent := postCreateSession(t, s, map[string]any{
		"name": "主 Agent", "cli": "claude", "provider": "gw", "cwd": t.TempDir(),
	})
	if parent.Code != http.StatusOK {
		t.Fatalf("create parent: code=%d body=%s", parent.Code, parent.Body.String())
	}
	parentID := decodeID(t, parent.Body.Bytes())

	child := postCreateSession(t, s, map[string]any{
		"name": "worker", "cli": "claude", "provider": "gw", "cwd": t.TempDir(),
		"parent_id": parentID,
	})
	if child.Code != http.StatusOK {
		t.Fatalf("create child: code=%d body=%s", child.Code, child.Body.String())
	}
	childID := decodeID(t, child.Body.Bytes())
	stored := st.Get(childID)
	if stored == nil || stored.ParentID != parentID {
		t.Fatalf("child parent_id = %+v, want %s", stored, parentID)
	}

	// Unknown parent is a bad request, not a silent top-level session.
	bad := postCreateSession(t, s, map[string]any{
		"name": "x", "cli": "claude", "provider": "gw", "cwd": t.TempDir(),
		"parent_id": "does-not-exist",
	})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("unknown parent: code=%d body=%s", bad.Code, bad.Body.String())
	}
}

// TestNewSessionMCPRecordsCallerAsParent: a worker created over the agentdeck
// MCP by another session is that session's child, without the model having to
// pass anything.
func TestNewSessionMCPRecordsCallerAsParent(t *testing.T) {
	s, st := createResidentServer(t)
	main := postCreateSession(t, s, map[string]any{
		"name": "main", "cli": "claude", "provider": "gw", "cwd": t.TempDir(), "main_agent": true,
	})
	mainID := decodeID(t, main.Body.Bytes())

	out, err := s.agentdeckToolCall(mainID, "new_session", map[string]any{
		"name": "worker-1", "cli": "claude", "provider": "gw", "cwd": t.TempDir(),
	})
	if err != nil {
		t.Fatalf("new_session: %v", err)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("parse created session: %v (%s)", err, out)
	}
	if created.ID == "" {
		t.Fatalf("no session id in %s", out)
	}
	if got := st.Get(created.ID); got == nil || got.ParentID != mainID {
		t.Fatalf("new_session parent_id = %+v, want %s", got, mainID)
	}

	// No caller identity (opencode/mcode share one MCP config) → top-level.
	out, err = s.agentdeckToolCall("", "new_session", map[string]any{
		"name": "worker-2", "cli": "claude", "provider": "gw", "cwd": t.TempDir(),
	})
	if err != nil {
		t.Fatalf("new_session (no caller): %v", err)
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatal(err)
	}
	if got := st.Get(created.ID); got == nil || got.ParentID != "" {
		t.Fatalf("no-caller new_session should be top-level: %+v", got)
	}
}

func decodeID(t *testing.T, raw []byte) string {
	t.Helper()
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode session: %v (%s)", err, raw)
	}
	if v.ID == "" {
		t.Fatalf("no id in %s", raw)
	}
	return v.ID
}
