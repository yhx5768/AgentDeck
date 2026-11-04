package server

import (
	"path/filepath"
	"strings"
	"testing"

	"cc-webui/internal/config"
	"cc-webui/internal/runner"
	"cc-webui/internal/store"
)

// boardMCPServer wires a Server with both a board DB and a store, plus a main
// agent and a plain worker session, to exercise the board MCP tools.
func boardMCPServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir}
	cm := config.NewManager(filepath.Join(dir, "config.yaml"), cfg)
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cm: cm, st: st, mgr: runner.NewManager(cm, st), boardHub: newBoardHub()}
	s.initAgentBoardDB(boardPaths{DB: filepath.Join(dir, "agent-board.db")})
	if s.boardDBErr != nil {
		t.Fatal(s.boardDBErr)
	}
	t.Cleanup(s.closeAgentBoardDB)
	for _, sess := range []*store.Session{
		{ID: "main", Name: "main", MainAgent: true, Cwd: dir, CLI: "claude", Provider: "gw"},
		{ID: "worker", Name: "worker", Cwd: dir, CLI: "claude", Provider: "gw"},
	} {
		if err := st.Upsert(sess, nil); err != nil {
			t.Fatal(err)
		}
	}
	return s, st
}

func TestAgentdeckBoardToolsMainAgentOnly(t *testing.T) {
	s, _ := boardMCPServer(t)

	// The main agent can create / list / update / comment / bind.
	out, err := s.agentdeckToolCall("main", "board_create", map[string]any{
		"id": "T-1", "title": "任务A", "project": "cc-webui", "worker": "w1",
	})
	if err != nil || !strings.Contains(out, "\"ok\":true") {
		t.Fatalf("main board_create: err=%v out=%s", err, out)
	}
	out, err = s.agentdeckToolCall("main", "board_list", nil)
	if err != nil || !strings.Contains(out, "T-1") {
		t.Fatalf("main board_list: err=%v out=%s", err, out)
	}
	out, err = s.agentdeckToolCall("main", "board_list", map[string]any{"project": "nope"})
	if err != nil || strings.Contains(out, "T-1") {
		t.Fatalf("board_list filter: err=%v out=%s", err, out)
	}
	out, err = s.agentdeckToolCall("main", "board_update", map[string]any{"id": "T-1", "status": "待验收"})
	if err != nil || !strings.Contains(out, "待验收") {
		t.Fatalf("board_update: err=%v out=%s", err, out)
	}
	out, err = s.agentdeckToolCall("main", "board_comment", map[string]any{"id": "T-1", "text": "派工：跑一遍测试"})
	if err != nil || !strings.Contains(out, "\"ok\":true") {
		t.Fatalf("board_comment: err=%v out=%s", err, out)
	}
	out, err = s.agentdeckToolCall("main", "board_bind_worker", map[string]any{"id": "T-1", "worker": "w2"})
	if err != nil || !strings.Contains(out, "w2") {
		t.Fatalf("board_bind_worker: err=%v out=%s", err, out)
	}
	out, err = s.agentdeckToolCall("main", "board_get", map[string]any{"id": "T-1"})
	if err != nil || !strings.Contains(out, "派工：跑一遍测试") {
		t.Fatalf("board_get timeline: err=%v out=%s", err, out)
	}
}

func TestAgentdeckBoardToolsRejectNonMainAgent(t *testing.T) {
	s, _ := boardMCPServer(t)

	for _, tc := range []struct{ session, tool string }{
		{"worker", "board_create"},
		{"worker", "board_list"},
		{"worker", "board_update"},
		{"worker", "board_comment"},
		{"worker", "board_bind_worker"},
		{"", "board_list"}, // no session identity at all
	} {
		if _, err := s.agentdeckToolCall(tc.session, tc.tool, map[string]any{"id": "x", "title": "x", "text": "x"}); err == nil {
			t.Fatalf("session %q must not use board tool %q", tc.session, tc.tool)
		}
	}
}
