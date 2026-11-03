package boardmcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type recordedCall struct {
	method string
	path   string
	auth   string
	body   map[string]any
}

func boardAPI(t *testing.T) (*httptest.Server, *[]recordedCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []recordedCall
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		mu.Lock()
		calls = append(calls, recordedCall{r.Method, r.URL.Path, r.Header.Get("Authorization"), body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/agent-board":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"source":   "sqlite",
				"counts":   map[string]int{"进行中": 1, "待验收": 1},
				"projects": []string{"AgentDeck", "Verify"},
				"tasks": []any{
					map[string]any{"id": "YHX-TODO-1", "status": "进行中", "project": "AgentDeck", "worker": "w1"},
					map[string]any{"id": "YHX-TODO-2", "status": "待验收", "project": "Verify", "worker": "w1"},
				},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agent-board/tasks/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "task": map[string]any{"id": strings.TrimPrefix(r.URL.Path, "/api/agent-board/tasks/")}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/agent-board/workers":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "workers": []any{map[string]any{"peer_name": "w1"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/agent-board/backup":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "path": "/tmp/board-backup.sqlite"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/agent-board/tasks":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "task": body})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/agent-board/tasks/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "task": body})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "duplicate": false})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/worker"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "task": body})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "not found " + r.Method + " " + r.URL.Path})
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestToolsCoverBoardAPI(t *testing.T) {
	want := []string{
		"board_task_create", "board_task_update", "board_task_event", "board_task_bind",
		"board_task_get", "board_task_list", "board_worker_list", "board_snapshot", "board_backup",
	}
	got := map[string]bool{}
	for _, tool := range Tools {
		got[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
		if tool.InputSchema["type"] != "object" {
			t.Errorf("%s schema type = %v", tool.Name, tool.InputSchema["type"])
		}
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("missing tool %s", name)
		}
	}
	if len(Tools) != len(want) {
		t.Fatalf("tools = %d, want %d", len(Tools), len(want))
	}
	raw, _ := json.Marshal(Tools)
	for _, banned := range []string{"http://", "https://", "test-token", "gateway_token"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("tool schema leaks %q", banned)
		}
	}
}

func TestCallTools(t *testing.T) {
	srv, calls := boardAPI(t)
	s := New(srv.URL, "test-token")

	if _, err := s.Call("board_task_create", map[string]any{"title": "新任务"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call("board_task_update", map[string]any{"task_id": "YHX-TODO-1", "status": "进行中"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call("board_task_event", map[string]any{"task_id": "YHX-TODO-1", "kind": "用户消息", "text": "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call("board_task_bind", map[string]any{"task_id": "YHX-TODO-1", "worker": "w1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call("board_task_get", map[string]any{"task_id": "YHX-TODO-1"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.Call("board_task_list", map[string]any{"status": "待验收"})
	if err != nil {
		t.Fatal(err)
	}
	filtered := list.(map[string]any)["tasks"].([]any)
	if len(filtered) != 1 || filtered[0].(map[string]any)["id"] != "YHX-TODO-2" {
		t.Fatalf("filtered = %v", filtered)
	}
	summary, err := s.Call("board_task_list", map[string]any{"summary": true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.(map[string]any)["tasks"].(int) != 2 {
		t.Fatalf("summary = %v", summary)
	}
	if _, err := s.Call("board_worker_list", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call("board_snapshot", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call("board_backup", nil); err != nil {
		t.Fatal(err)
	}

	if len(*calls) < 9 {
		t.Fatalf("calls = %d", len(*calls))
	}
	for _, call := range *calls {
		if call.auth != "Bearer test-token" {
			t.Fatalf("auth = %q", call.auth)
		}
	}
	var update recordedCall
	for _, call := range *calls {
		if call.method == http.MethodPatch {
			update = call
		}
	}
	if update.path != "/api/agent-board/tasks/YHX-TODO-1" || update.body["status"] != "进行中" {
		t.Fatalf("update = %+v", update)
	}
	if _, ok := update.body["task_id"]; ok {
		t.Fatal("task_id leaked into PATCH body")
	}
}

func TestCallRejectsMissingIDs(t *testing.T) {
	s := New("http://127.0.0.1:1", "t")
	for _, name := range []string{"board_task_update", "board_task_event", "board_task_get"} {
		if _, err := s.Call(name, map[string]any{}); err == nil || !strings.Contains(err.Error(), "task_id") {
			t.Errorf("%s err = %v", name, err)
		}
	}
	if _, err := s.Call("board_task_bind", map[string]any{"task_id": "x"}); err == nil {
		t.Error("bind without worker should fail")
	}
	if _, err := s.Call("board_task_update", map[string]any{"task_id": "x"}); err == nil {
		t.Error("empty update should fail")
	}
}

func TestLoopbackURL(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:8321":   "http://127.0.0.1:8321",
		":8321":          "http://127.0.0.1:8321",
		"127.0.0.1:9000": "http://127.0.0.1:9000",
		"localhost:8321": "http://localhost:8321",
		"http://x:1":     "http://x:1",
	}
	for in, want := range cases {
		if got := loopbackURL(in); got != want {
			t.Errorf("loopbackURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewFromConfigRequiresExplicitToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("listen: \"127.0.0.1:8321\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFromConfig(path); err == nil {
		t.Fatal("expected missing-token error")
	}
	if err := os.WriteFile(path, []byte("listen: \"127.0.0.1:8321\"\ngateway_token: \"tok\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewFromConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.BaseURL != "http://127.0.0.1:8321" || s.Token != "tok" {
		t.Fatalf("server = %+v", s)
	}
}

func TestToolErrorShape(t *testing.T) {
	result := toolError(fmt.Errorf("boom").Error())
	if result["isError"] != true {
		t.Fatalf("result = %v", result)
	}
}
