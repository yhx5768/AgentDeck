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

func writeBoardFile(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func boardDBServer(t *testing.T, jsonPath, routingPath string) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir}
	s := &Server{cm: config.NewManager(filepath.Join(dir, "config.yaml"), cfg), boardHub: newBoardHub()}
	s.initAgentBoardDB(boardPaths{
		DB:           filepath.Join(dir, "agent-board.db"),
		LegacyJSON:   jsonPath,
		RoutingTable: routingPath,
	})
	if s.boardDBErr != nil {
		t.Fatal(s.boardDBErr)
	}
	t.Cleanup(s.closeAgentBoardDB)
	return s
}

func boardDBServerAt(t *testing.T, dbPath, jsonPath, routingPath string) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir}
	s := &Server{cm: config.NewManager(filepath.Join(dir, "config.yaml"), cfg), boardHub: newBoardHub()}
	s.initAgentBoardDB(boardPaths{DB: dbPath, LegacyJSON: jsonPath, RoutingTable: routingPath})
	t.Cleanup(s.closeAgentBoardDB)
	return s
}

func sampleBoardJSON() map[string]any {
	return map[string]any{"tasks": []any{
		map[string]any{
			"id": "YHX-TODO-819", "icafe_id": "YHX-TODO-819", "title": "回复上板",
			"status": "已完成", "project": "AgentDeck", "worker": "AgentDeck-卡片自动化",
			"session": "sess-1", "cwd": "/work", "updated_at": "2026-11-03 19:35",
			"progress": "完成", "last_event": "A2A 收", "report_file": "report.md",
			"timeline": []any{
				map[string]any{"at": "2026-11-03 19:35", "kind": "最近回报", "text": "完成"},
				map[string]any{"at": "2026-11-03 19:06", "kind": "原始任务", "text": "用户原问题"},
			},
		},
	}}
}

const sampleRouting = `| task_id | card | status | worker_peer | session_id | cwd | last_event | last_report | report_file | card_modified | comment_id | updated |
|---|---|---|---|---|---|---|---|---|---|---|---|
| SQLite 看板 | YHX-TODO-900 | 进行中 | AgentDeck-卡片自动化 | sess-9 | /work | A2A 收 | 已接上 | report.md | — | — | 2026-11-03 20:00 |
`

func TestAgentBoardDBMigratesOnStartup(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "store.json")
	routingPath := filepath.Join(dir, "routing-table.md")
	writeBoardFile(t, jsonPath, sampleBoardJSON())
	if err := os.WriteFile(routingPath, []byte(sampleRouting), 0o644); err != nil {
		t.Fatal(err)
	}
	s := boardDBServer(t, jsonPath, routingPath)

	w := httptest.NewRecorder()
	s.handleAgentBoardDB(w, httptest.NewRequest(http.MethodGet, "/api/agent-board", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var snapshot map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot["source"] != "sqlite" {
		t.Fatalf("source = %v", snapshot["source"])
	}
	tasks := snapshot["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d", len(tasks))
	}
	if snapshot["counts"].(map[string]any)["已完成"].(float64) != 1 {
		t.Fatalf("counts = %v", snapshot["counts"])
	}
}

func TestAgentBoardDBEventDoesNotWriteLegacyJSON(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "store.json")
	writeBoardFile(t, jsonPath, sampleBoardJSON())
	before, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	s := boardDBServer(t, jsonPath, "")

	body := strings.NewReader(`{"kind":"用户消息","text":"抽屉消息","at":"2026-11-03 20:10:00"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/agent-board/tasks/YHX-TODO-819/events", body)
	req.SetPathValue("id", "YHX-TODO-819")
	w := httptest.NewRecorder()
	s.handleAgentBoardAppendEventDB(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["duplicate"] != false {
		t.Fatalf("result = %v", result)
	}

	after, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("DB mode must not rewrite the legacy JSON store")
	}
	task, err := s.boardDB.GetTask("YHX-TODO-819")
	if err != nil {
		t.Fatal(err)
	}
	events := task["timeline"].([]map[string]any)
	if len(events) != 3 || events[0]["text"] != "抽屉消息" {
		t.Fatalf("events = %+v", events)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/agent-board/tasks/YHX-TODO-819/events",
		strings.NewReader(`{"kind":"用户消息","text":"抽屉消息","at":"2026-11-03 20:10:00"}`))
	req.SetPathValue("id", "YHX-TODO-819")
	w = httptest.NewRecorder()
	s.handleAgentBoardAppendEventDB(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("duplicate code = %d body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["duplicate"] != true {
		t.Fatalf("duplicate result = %v", result)
	}
}

func TestAgentBoardDBCreateUpdateBind(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "store.json")
	writeBoardFile(t, jsonPath, sampleBoardJSON())
	s := boardDBServer(t, jsonPath, "")

	body := strings.NewReader(`{"id":"YHX-TODO-901","title":"新建任务","project":"主agent","tags":["看板"]}`)
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

	req = httptest.NewRequest(http.MethodPatch, "/api/agent-board/tasks/YHX-TODO-901",
		strings.NewReader(`{"status":"进行中","status_reason":"接线中"}`))
	req.SetPathValue("id", "YHX-TODO-901")
	w = httptest.NewRecorder()
	s.handleAgentBoardUpdateTask(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update code = %d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/agent-board/tasks/YHX-TODO-901/worker",
		strings.NewReader(`{"worker":"AgentDeck-卡片自动化","session":"sess-9","cwd":"/work"}`))
	req.SetPathValue("id", "YHX-TODO-901")
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

func TestAgentBoardDBCreatesEmptyWithoutLegacyFiles(t *testing.T) {
	dir := t.TempDir()
	s := boardDBServer(t, filepath.Join(dir, "missing.json"), filepath.Join(dir, "missing-table.md"))
	w := httptest.NewRecorder()
	s.handleAgentBoardDB(w, httptest.NewRequest(http.MethodGet, "/api/agent-board", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var snapshot map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot["tasks"].([]any)) != 0 {
		t.Fatalf("tasks = %v", snapshot["tasks"])
	}
	if _, err := os.Stat(s.boardDB.Path()); err != nil {
		t.Fatalf("db not created: %v", err)
	}
}

func TestAgentBoardDBFailedImportRemovesNewDB(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "store.json")
	if err := os.WriteFile(jsonPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "agent-board.db")
	s := boardDBServerAt(t, dbPath, jsonPath, "")
	if s.boardDBErr == nil {
		t.Fatal("expected migration failure")
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("partial DB should be removed, stat err = %v", err)
	}
}

func TestDefaultBoardPathsNeedNoConfig(t *testing.T) {
	dir := t.TempDir()
	paths := defaultBoardPaths(dir)
	if paths.DB != filepath.Join(dir, "agent-board.db") {
		t.Fatalf("db = %q", paths.DB)
	}
	if paths.LegacyJSON != filepath.Join(dir, "agent-board.local.json") {
		t.Fatalf("legacy json = %q", paths.LegacyJSON)
	}
	if paths.RoutingTable != defaultAgentBoardRoutingTable {
		t.Fatalf("routing = %q", paths.RoutingTable)
	}
	if !filepath.IsAbs(defaultAgentBoardRoutingTable) {
		t.Fatalf("routing path should be absolute: %q", defaultAgentBoardRoutingTable)
	}
}

func TestAgentBoardDBExistingSkipsMigration(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agent-board.db")
	st, err := boarddb.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(map[string]any{"id": "YHX-TODO-999", "title": "已有任务"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(dir, "store.json")
	writeBoardFile(t, jsonPath, sampleBoardJSON())
	s := boardDBServerAt(t, dbPath, jsonPath, "")
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
	tasks := snapshot["tasks"].([]any)
	if len(tasks) != 1 || tasks[0].(map[string]any)["id"] != "YHX-TODO-999" {
		t.Fatalf("existing DB should not import legacy JSON: %v", tasks)
	}
}

func TestAgentBoardDBOpenFailureReportsError(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "not-a-db")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	s := boardDBServerAt(t, blocked, filepath.Join(dir, "missing.json"), "")
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
