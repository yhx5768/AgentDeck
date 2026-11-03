package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"cc-webui/internal/config"
)

func boardServer(t *testing.T, path string) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, AgentBoardPath: path}
	return &Server{cm: config.NewManager(filepath.Join(dir, "config.yaml"), cfg)}
}

func TestHandleAgentBoardServesSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-board.json")
	snapshot := map[string]any{
		"generated_at": "2026-11-03 13:10:00",
		"source":       "icafe-cache+routing-table",
		"projects":     []string{"AgentDeck"},
		"tasks": []any{
			map[string]any{"id": "YHX-TODO-806", "status": "进行中", "project": "AgentDeck"},
		},
	}
	raw, _ := json.Marshal(snapshot)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s := boardServer(t, path)
	w := httptest.NewRecorder()
	s.handleAgentBoard(w, httptest.NewRequest(http.MethodGet, "/api/agent-board", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["source"] != "icafe-cache+routing-table" {
		t.Fatalf("source = %v", got["source"])
	}
}

func TestHandleAgentBoardMissingSnapshot(t *testing.T) {
	s := boardServer(t, filepath.Join(t.TempDir(), "nope.json"))
	w := httptest.NewRecorder()
	s.handleAgentBoard(w, httptest.NewRequest(http.MethodGet, "/api/agent-board", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["source"] != "missing" {
		t.Fatalf("source = %v", got["source"])
	}
	if tasks, ok := got["tasks"].([]any); !ok || len(tasks) != 0 {
		t.Fatalf("tasks = %v", got["tasks"])
	}
}

func TestHandleAgentBoardRejectsBadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-board.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := boardServer(t, path)
	w := httptest.NewRecorder()
	s.handleAgentBoard(w, httptest.NewRequest(http.MethodGet, "/api/agent-board", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d", w.Code)
	}
}
