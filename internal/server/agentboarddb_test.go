package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cc-webui/internal/boarddb"
	"cc-webui/internal/config"
)

// boardDBServer wires a Server with a fresh board DB under a temp data dir.
func boardDBServer(t *testing.T) *Server {
	t.Helper()
	return boardDBServerAt(t, t.TempDir())
}

func boardDBServerAt(t *testing.T, dataDir string) *Server {
	t.Helper()
	cfg := &config.Config{DataDir: dataDir}
	s := &Server{cm: config.NewManager(filepath.Join(dataDir, "config.yaml"), cfg), boardHub: newBoardHub()}
	s.initAgentBoardDB()
	t.Cleanup(s.closeAgentBoardDB)
	return s
}

func TestAgentBoardDBCreatesEmptyDB(t *testing.T) {
	s := boardDBServer(t)
	if s.boardDBErr != nil {
		t.Fatal(s.boardDBErr)
	}
	w := httptest.NewRecorder()
	s.handleAgentBoardDB(w, httptest.NewRequest(http.MethodGet, "/api/agent-board", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var snapshot map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot["source"] != "sqlite" || len(snapshot["tasks"].([]any)) != 0 {
		t.Fatalf("snapshot = %v", snapshot)
	}
	if _, err := os.Stat(s.boardDB.Path()); err != nil {
		t.Fatalf("db not created: %v", err)
	}
}

func TestAgentBoardDBAppendEventIdempotent(t *testing.T) {
	s := boardDBServer(t)
	if _, err := s.boardDB.CreateTask(map[string]any{"id": "T-1", "title": "x"}); err != nil {
		t.Fatal(err)
	}
	post := func() map[string]any {
		req := httptest.NewRequest(http.MethodPost, "/api/agent-board/tasks/T-1/events",
			strings.NewReader(`{"kind":"用户消息","text":"抽屉消息","at":"2026-11-03 20:10:00"}`))
		req.SetPathValue("id", "T-1")
		w := httptest.NewRecorder()
		s.handleAgentBoardAppendEventDB(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if post()["duplicate"] != false {
		t.Fatal("first append should not be a duplicate")
	}
	if post()["duplicate"] != true {
		t.Fatal("second append should be a duplicate")
	}
	task, err := s.boardDB.GetTask("T-1")
	if err != nil {
		t.Fatal(err)
	}
	if events := task["timeline"].([]map[string]any); len(events) != 2 { // 建卡 + 抽屉消息
		t.Fatalf("events = %+v", events)
	}
}

func TestAgentBoardDBCreateUpdateBind(t *testing.T) {
	s := boardDBServer(t)

	body := strings.NewReader(`{"id":"T-901","title":"新建任务","project":"主agent","tags":["看板"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/agent-board/tasks", body)
	w := httptest.NewRecorder()
	s.handleAgentBoardCreateTask(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create code = %d body=%s", w.Code, w.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created["task"].(map[string]any)["project"] != "AgentDeck" {
		t.Fatalf("created = %v", created)
	}

	req = httptest.NewRequest(http.MethodPatch, "/api/agent-board/tasks/T-901",
		strings.NewReader(`{"status":"进行中","status_reason":"接线中"}`))
	req.SetPathValue("id", "T-901")
	w = httptest.NewRecorder()
	s.handleAgentBoardUpdateTask(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update code = %d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/agent-board/tasks/T-901/worker",
		strings.NewReader(`{"worker":"AgentDeck-卡片自动化","session":"sess-9","cwd":"/work"}`))
	req.SetPathValue("id", "T-901")
	w = httptest.NewRecorder()
	s.handleAgentBoardBindWorker(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("bind code = %d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/agent-board/workers", nil)
	w = httptest.NewRecorder()
	s.handleAgentBoardWorkers(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("workers code = %d body=%s", w.Code, w.Body.String())
	}
	var workers map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &workers); err != nil {
		t.Fatal(err)
	}
	if len(workers["workers"].([]any)) != 1 {
		t.Fatalf("workers = %v", workers)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/agent-board/backup", nil)
	w = httptest.NewRecorder()
	s.handleAgentBoardBackup(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("backup code = %d body=%s", w.Code, w.Body.String())
	}
	var backup map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &backup); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backup["path"].(string)); err != nil {
		t.Fatalf("backup file: %v", err)
	}
}

func TestAgentBoardDBExistingKeepsTasks(t *testing.T) {
	dir := t.TempDir()
	st, err := boarddb.Open(filepath.Join(dir, "agent-board.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(map[string]any{"id": "T-999", "title": "已有任务"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	s := boardDBServerAt(t, dir)
	if s.boardDBErr != nil {
		t.Fatal(s.boardDBErr)
	}
	w := httptest.NewRecorder()
	s.handleAgentBoardDB(w, httptest.NewRequest(http.MethodGet, "/api/agent-board", nil))
	var snapshot map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	tasks := snapshot["tasks"].([]any)
	if len(tasks) != 1 || tasks[0].(map[string]any)["id"] != "T-999" {
		t.Fatalf("tasks = %v", tasks)
	}
}

func TestAgentBoardDBOpenFailureReportsError(t *testing.T) {
	dir := t.TempDir()
	// A regular file where the data dir should be makes <dir>/agent-board.db
	// unopenable.
	blocked := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := boardDBServerAt(t, blocked)
	if s.boardDBErr == nil {
		t.Fatal("expected open failure")
	}
	w := httptest.NewRecorder()
	s.handleAgentBoardDB(w, httptest.NewRequest(http.MethodGet, "/api/agent-board", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "看板数据库不可用") {
		t.Fatalf("body = %s", w.Body.String())
	}
}
