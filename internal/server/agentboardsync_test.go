package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testRouting = `| task_id | card | status | worker_peer | session_id | cwd | last_event | last_report | report_file | card_modified | comment_id | updated |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 20261103-a | YHX-TODO-780 | 待验收 | Q4规划-风险状态机 | sess-1 | /work | 回报 | v2 完成 | report.md | 2026-11-03 14:00 | 1 | 2026-11-03 14:00 |
| 20261103-b | YHX-TODO-799 | 待验收 | 鸿蒙CUID指标 | sess-2 | /work | 回报 | 验证完成 | report2.md | 2026-11-03 15:00 | 2 | 2026-11-03 15:00 |
`

func readTestStore(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var store map[string]any
	if err := json.Unmarshal(raw, &store); err != nil {
		t.Fatal(err)
	}
	return store
}

func testTasks(t *testing.T, store map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range store["tasks"].([]any) {
		out = append(out, raw.(map[string]any))
	}
	return out
}

func TestSyncBoardStoreParsesAndNormalizes(t *testing.T) {
	dir := t.TempDir()
	routing := filepath.Join(dir, "routing.md")
	storePath := filepath.Join(dir, "store.json")
	if err := os.WriteFile(routing, []byte(testRouting), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syncBoardStore(storePath, routing); err != nil {
		t.Fatal(err)
	}
	store := readTestStore(t, storePath)
	tasks := testTasks(t, store)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(tasks))
	}
	byID := map[string]map[string]any{}
	for _, task := range tasks {
		byID[task["id"].(string)] = task
	}
	if byID["YHX-TODO-780"]["project"] != "风险识别" {
		t.Fatalf("780 project = %v, want 风险识别", byID["YHX-TODO-780"]["project"])
	}
	if byID["YHX-TODO-799"]["project"] != "鸿蒙CUID" {
		t.Fatalf("799 project = %v, want 鸿蒙CUID", byID["YHX-TODO-799"]["project"])
	}
	if len(byID["YHX-TODO-780"]["timeline"].([]any)) != 1 {
		t.Fatalf("timeline not created")
	}
}

func TestSyncBoardStoreMergesAndDropsExtra(t *testing.T) {
	dir := t.TempDir()
	routing := filepath.Join(dir, "routing.md")
	storePath := filepath.Join(dir, "store.json")
	if err := os.WriteFile(routing, []byte(testRouting), 0o644); err != nil {
		t.Fatal(err)
	}
	existing := map[string]any{
		"tasks": []any{
			map[string]any{
				"id": "YHX-TODO-780", "icafe_id": "YHX-TODO-780", "title": "保留标题",
				"project": "Q4规划", "status": "进行中", "tags": []any{"规划"},
				"timeline": []any{map[string]any{"at": "2026-11-03 13:00", "kind": "主 Agent 回复", "text": "旧"}},
			},
			map[string]any{"id": "LOCAL-1", "icafe_id": "", "title": "本地任务", "status": "新建"},
		},
	}
	raw, _ := json.Marshal(existing)
	if err := os.WriteFile(storePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syncBoardStore(storePath, routing); err != nil {
		t.Fatal(err)
	}
	tasks := testTasks(t, readTestStore(t, storePath))
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d, want 2 (local task dropped)", len(tasks))
	}
	var first map[string]any
	for _, task := range tasks {
		if task["id"] == "YHX-TODO-780" {
			first = task
		}
	}
	if first["title"] != "保留标题" {
		t.Fatalf("title = %v, want 保留标题", first["title"])
	}
	if first["status"] != "待验收" || first["project"] != "风险识别" {
		t.Fatalf("merge = %v/%v", first["status"], first["project"])
	}
	if tags, ok := first["tags"].([]any); !ok || len(tags) != 1 || tags[0] != "规划" {
		t.Fatalf("tags = %v", first["tags"])
	}
	if len(first["timeline"].([]any)) != 2 {
		t.Fatalf("timeline should keep old + append report, got %v", first["timeline"])
	}
}

func TestSyncBoardStoreIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	routing := filepath.Join(dir, "routing.md")
	storePath := filepath.Join(dir, "store.json")
	if err := os.WriteFile(routing, []byte(testRouting), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := syncBoardStore(storePath, routing); err != nil {
			t.Fatal(err)
		}
	}
	tasks := testTasks(t, readTestStore(t, storePath))
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(tasks))
	}
	if len(tasks[0]["timeline"].([]any)) != 1 {
		t.Fatalf("repeated sync duplicated timeline: %v", tasks[0]["timeline"])
	}
}

func TestBoardStoreLockRejectsConcurrentWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	unlock, err := lockBoardStore(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := lockBoardStore(path, 100*time.Millisecond); err == nil {
		t.Fatal("second writer should time out while the first holds the lock")
	}
}

func TestParseBoardRoutingTableSkipsShortRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routing.md")
	content := "| a | b |\n" + testRouting
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := parseBoardRoutingTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
}
