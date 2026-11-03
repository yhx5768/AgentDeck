package boarddb

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func sampleJSON() map[string]any {
	return map[string]any{"tasks": []any{
		map[string]any{
			"id": "YHX-TODO-819", "icafe_id": "YHX-TODO-819", "title": "回复上板",
			"status": "已完成", "project": "AgentDeck", "worker": "AgentDeck-卡片自动化",
			"session": "sess-1", "cwd": "/work", "updated_at": "2026-11-03 19:35",
			"progress": "完成", "last_event": "A2A 收", "report_file": "report.md",
			"tags": []any{"规划"}, "url": "https://example.test/YHX-TODO-819",
			"timeline": []any{
				map[string]any{"at": "2026-11-03 19:35", "kind": "最近回报", "text": "完成"},
				map[string]any{"at": "2026-11-03 19:09", "kind": "A2A 收", "text": "worker 回报"},
				map[string]any{"at": "2026-11-03 19:06", "kind": "原始任务", "text": "用户原问题"},
				map[string]any{"at": "2026-11-03 19:06", "kind": "A2A 发", "text": "派工"},
			},
		},
		map[string]any{
			"id": "YHX-TODO-818", "icafe_id": "YHX-TODO-818", "title": "自动同步",
			"status": "待验收", "project": "AgentDeck", "updated_at": "2026-11-03 19:31",
			"progress_locked": true, "status_reason": "最终方案",
			"timeline": []any{map[string]any{"at": "2026-11-03 19:20", "kind": "主 Agent 回复", "text": "方案"}},
		},
	}}
}

func TestMigrateOnceAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "store.json")
	writeJSON(t, jsonPath, sampleJSON())
	dbPath := filepath.Join(dir, "board.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	result, err := st.MigrateOnce(jsonPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Migrated || result.Tasks != 2 || result.Events != 5 {
		t.Fatalf("result = %+v", result)
	}
	snapshot, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot["source"] != "sqlite" {
		t.Fatalf("source = %v", snapshot["source"])
	}
	tasks := snapshot["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d", len(tasks))
	}
	var task819 map[string]any
	for _, raw := range tasks {
		task := raw.(map[string]any)
		if task["id"] == "YHX-TODO-819" {
			task819 = task
		}
	}
	if task819 == nil {
		t.Fatal("819 missing")
	}
	kinds := []string{}
	for _, raw := range task819["timeline"].([]map[string]any) {
		kinds = append(kinds, raw["kind"].(string))
	}
	want := []string{"最近回报", "A2A 收", "原始任务", "A2A 发"}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("timeline = %v, want %v", kinds, want)
	}

	second, err := st.MigrateOnce(jsonPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Migrated || second.Reason != "already-migrated" {
		t.Fatalf("second = %+v", second)
	}
}

func TestMigrateOnceDoesNotOverwriteNewerDB(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "store.json")
	writeJSON(t, jsonPath, sampleJSON())
	dbPath := filepath.Join(dir, "board.db")

	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateTask(map[string]any{"id": "YHX-TODO-900", "title": "测试"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateTask("YHX-TODO-900", map[string]any{"status": "进行中"}); err != nil {
		t.Fatal(err)
	}
	result, err := st.MigrateOnce(jsonPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Migrated || result.Reason != "database-not-empty" {
		t.Fatalf("result = %+v", result)
	}
	task, err := st.GetTask("YHX-TODO-900")
	if err != nil {
		t.Fatal(err)
	}
	if task["status"] != "进行中" {
		t.Fatalf("status = %v", task["status"])
	}
	if _, err := st.GetTask("YHX-TODO-819"); err == nil {
		t.Fatal("legacy task should not be imported into a non-empty DB")
	}
}

func TestCreateUpdateEventBindAndWorkers(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	task, err := st.CreateTask(map[string]any{
		"id": "YHX-TODO-900", "title": "SQLite 看板", "status": "新建",
		"project": "主agent", "tags": []any{"规划", "存储"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if task["project"] != "AgentDeck" {
		t.Fatalf("project = %v", task["project"])
	}
	if fmt.Sprint(task["tags"]) != "[规划 存储]" {
		t.Fatalf("tags = %v", task["tags"])
	}

	task, err = st.UpdateTask("YHX-TODO-900", map[string]any{
		"status": "进行中", "progress": "接线中", "status_reason": "等重启窗口",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task["status"] != "进行中" || task["progress"] != "接线中" || task["status_reason"] != "等重启窗口" {
		t.Fatalf("task = %+v", task)
	}

	if err := st.BindWorker("YHX-TODO-900", "AgentDeck-卡片自动化", "sess-9", "/work"); err != nil {
		t.Fatal(err)
	}
	created, err := st.AppendEvent("YHX-TODO-900", "A2A 发", "派工内容", "2026-11-03 20:00:00", "cli")
	if err != nil || !created {
		t.Fatalf("append created=%v err=%v", created, err)
	}
	duplicate, err := st.AppendEvent("YHX-TODO-900", "A2A 发", "派工内容", "2026-11-03 20:00:00", "cli")
	if err != nil || duplicate {
		t.Fatalf("duplicate created=%v err=%v", duplicate, err)
	}
	task, err = st.GetTask("YHX-TODO-900")
	if err != nil {
		t.Fatal(err)
	}
	events := task["timeline"].([]map[string]any)
	if len(events) != 4 { // 建卡 + 状态 + 派工 + A2A 发
		t.Fatalf("events = %d %+v", len(events), events)
	}
	if task["worker"] != "AgentDeck-卡片自动化" || task["session"] != "sess-9" {
		t.Fatalf("worker/session = %v/%v", task["worker"], task["session"])
	}
	workers, err := st.Workers()
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 || workers[0]["peer_name"] != "AgentDeck-卡片自动化" {
		t.Fatalf("workers = %+v", workers)
	}
}

func TestConcurrentAppends(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateTask(map[string]any{"id": "YHX-TODO-901", "title": "并发"}); err != nil {
		t.Fatal(err)
	}
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := st.AppendEvent("YHX-TODO-901", "用户消息", fmt.Sprintf("消息-%02d", i), "2026-11-03 20:00:00", "test"); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	task, err := st.GetTask("YHX-TODO-901")
	if err != nil {
		t.Fatal(err)
	}
	events := task["timeline"].([]map[string]any)
	if len(events) != n+1 {
		t.Fatalf("events = %d, want %d", len(events), n+1)
	}
}

func TestBackupAndReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "board.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(map[string]any{"id": "YHX-TODO-902", "title": "备份"}); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backup.db")
	if err := st.Backup(backup); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	copied, err := Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	task, err := copied.GetTask("YHX-TODO-902")
	if err != nil {
		t.Fatal(err)
	}
	if task["title"] != "备份" {
		t.Fatalf("title = %v", task["title"])
	}
}
