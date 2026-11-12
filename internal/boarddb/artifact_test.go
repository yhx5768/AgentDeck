package boarddb

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestArtifactsAndLegacyReportFile(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.CreateTask(map[string]any{
		"id": "T-1", "title": "产物托管", "report_file": "reports/old.html",
	}); err != nil {
		t.Fatal(err)
	}

	task, err := st.GetTask("T-1")
	if err != nil {
		t.Fatal(err)
	}
	// Legacy report_file is synthesized as a display-only artifact.
	artifacts := task["artifacts"].([]map[string]any)
	if len(artifacts) != 1 {
		t.Fatalf("legacy artifacts = %d %+v", len(artifacts), artifacts)
	}
	legacy := artifacts[0]
	if legacy["legacy"] != true || legacy["kind"] != "html" || legacy["filename"] != "old.html" {
		t.Fatalf("legacy artifact = %+v", legacy)
	}
	if task["report_file"] != "reports/old.html" {
		t.Fatalf("report_file changed: %v", task["report_file"])
	}

	created, err := st.CreateArtifact(map[string]any{
		"id": "A-1", "task_id": "T-1", "title": "验收报告", "kind": "html",
		"mime": "text/html; charset=utf-8", "filename": "report.html", "size": 12,
		"storage_path": "T-1/A-1/report.html", "source_session": "worker-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created["id"] != "A-1" || created["source_session"] != "worker-1" {
		t.Fatalf("created artifact = %+v", created)
	}

	task, err = st.GetTask("T-1")
	if err != nil {
		t.Fatal(err)
	}
	artifacts = task["artifacts"].([]map[string]any)
	if len(artifacts) != 2 {
		t.Fatalf("artifacts = %d %+v", len(artifacts), artifacts)
	}
	if artifacts[0]["id"] != "A-1" || artifacts[1]["legacy"] != true {
		t.Fatalf("artifact order = %+v", artifacts)
	}

	// Snapshot carries the same artifacts array.
	snapshot, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	tasks := snapshot["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d", len(tasks))
	}
	if got := tasks[0].(map[string]any)["artifacts"].([]map[string]any); len(got) != 2 {
		t.Fatalf("snapshot artifacts = %+v", got)
	}

	// A publish records a timeline event and bumps last_event.
	found := false
	for _, event := range task["timeline"].([]map[string]any) {
		if event["kind"] == "产物" {
			found = true
		}
	}
	if !found || task["last_event"] != "产物" {
		t.Fatalf("timeline/last_event = %+v / %v", task["timeline"], task["last_event"])
	}

	// External URL artifacts need no storage path.
	if _, err := st.CreateArtifact(map[string]any{
		"task_id": "T-1", "title": "设计稿", "kind": "url", "url": "https://example.com/x",
	}); err != nil {
		t.Fatal(err)
	}
	// Hosted artifacts without storage_path are rejected.
	if _, err := st.CreateArtifact(map[string]any{
		"task_id": "T-1", "title": "坏产物", "kind": "html", "filename": "x.html",
	}); err == nil {
		t.Fatal("expected storage_path validation error")
	}
	// Unknown task is rejected.
	if _, err := st.CreateArtifact(map[string]any{
		"task_id": "nope", "kind": "html", "storage_path": "x",
	}); err == nil {
		t.Fatal("expected task not found")
	}
	// Unsafe ids are rejected (they become path components on the server).
	if _, err := st.CreateArtifact(map[string]any{
		"task_id": "T-1", "id": "../evil", "kind": "html", "storage_path": "x",
	}); err == nil {
		t.Fatal("expected invalid artifact id")
	}
}

func TestArtifactExplicitRound(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateTask(map[string]any{"id": "T-R", "title": "轮次"}); err != nil {
		t.Fatal(err)
	}

	created, err := st.CreateArtifact(map[string]any{
		"id": "A-1", "task_id": "T-R", "title": "报告", "kind": "html",
		"filename": "r.html", "storage_path": "T-R/A-1/r.html", "round": 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created["round"] != 3 {
		t.Fatalf("created round = %v", created["round"])
	}
	// Numeric strings are accepted too (MCP/raw-query shapes).
	if _, err := st.CreateArtifact(map[string]any{
		"id": "A-2", "task_id": "T-R", "title": "文字稿", "kind": "markdown",
		"filename": "r.md", "storage_path": "T-R/A-2/r.md", "round": "3",
	}); err != nil {
		t.Fatal(err)
	}
	// Round is optional and defaults to the legacy/unknown bucket.
	if _, err := st.CreateArtifact(map[string]any{
		"id": "A-3", "task_id": "T-R", "title": "旧产物", "kind": "file",
		"filename": "old.bin", "storage_path": "T-R/A-3/old.bin",
	}); err != nil {
		t.Fatal(err)
	}
	// Invalid rounds are rejected.
	if _, err := st.CreateArtifact(map[string]any{
		"id": "A-4", "task_id": "T-R", "kind": "file", "storage_path": "x", "round": -1,
	}); err == nil {
		t.Fatal("expected negative round rejection")
	}
	if _, err := st.CreateArtifact(map[string]any{
		"id": "A-5", "task_id": "T-R", "kind": "file", "storage_path": "x", "round": 1.5,
	}); err == nil {
		t.Fatal("expected fractional round rejection")
	}

	task, err := st.GetTask("T-R")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	for _, item := range task["artifacts"].([]map[string]any) {
		byID[item["id"].(string)] = item
	}
	if byID["A-1"]["round"] != 3 || byID["A-1"]["round_source"] != "explicit" {
		t.Fatalf("A-1 = %+v", byID["A-1"])
	}
	if byID["A-2"]["round"] != 3 || byID["A-2"]["round_source"] != "explicit" {
		t.Fatalf("A-2 = %+v", byID["A-2"])
	}
	if byID["A-3"]["round"] != 0 || byID["A-3"]["round_source"] != "inferred" || byID["A-3"]["round_batch"] != 1 {
		t.Fatalf("A-3 = %+v", byID["A-3"])
	}
}

func TestArtifactLegacyRoundInference(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateTask(map[string]any{
		"id": "T-L", "title": "旧数据", "report_file": "reports/old.html",
	}); err != nil {
		t.Fatal(err)
	}
	mk := func(id, at string, extra map[string]any) {
		t.Helper()
		input := map[string]any{
			"id": id, "task_id": "T-L", "title": id, "kind": "file",
			"filename": id + ".bin", "storage_path": "T-L/" + id + "/x.bin",
		}
		for k, v := range extra {
			input[k] = v
		}
		if _, err := st.CreateArtifact(input); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.Exec("UPDATE task_artifacts SET created_at=? WHERE id=?", at, id); err != nil {
			t.Fatal(err)
		}
	}
	mk("old-a", "2026-11-09 10:00:00", nil)
	mk("old-b", "2026-11-09 10:05:00", nil) // same batch as old-a
	mk("new-a", "2026-11-09 12:00:00", nil) // newer batch
	mk("bad-time", "", nil)                 // unparsable -> history
	mk("explicit", "2026-11-09 09:00:00", map[string]any{"round": 7})

	task, err := st.GetTask("T-L")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	for _, item := range task["artifacts"].([]map[string]any) {
		byID[item["id"].(string)] = item
	}
	if byID["old-a"]["round_source"] != "inferred" || byID["old-a"]["round_batch"] != 2 {
		t.Fatalf("old-a = %+v", byID["old-a"])
	}
	if byID["old-b"]["round_source"] != "inferred" || byID["old-b"]["round_batch"] != 2 {
		t.Fatalf("old-b = %+v", byID["old-b"])
	}
	if byID["new-a"]["round_source"] != "inferred" || byID["new-a"]["round_batch"] != 1 {
		t.Fatalf("new-a = %+v", byID["new-a"])
	}
	if byID["bad-time"]["round_source"] != "" || byID["bad-time"]["round_batch"] != 0 {
		t.Fatalf("bad-time = %+v", byID["bad-time"])
	}
	if byID["explicit"]["round"] != 7 || byID["explicit"]["round_source"] != "explicit" {
		t.Fatalf("explicit = %+v", byID["explicit"])
	}
	legacy := byID["legacy-report"]
	if legacy == nil || legacy["round_source"] != "" || legacy["round_batch"] != 0 {
		t.Fatalf("legacy-report = %+v", legacy)
	}

	// Snapshot must carry the same grouping hints (it uses a different query).
	snapshot, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	snapByID := map[string]map[string]any{}
	for _, item := range snapshot["tasks"].([]any)[0].(map[string]any)["artifacts"].([]map[string]any) {
		snapByID[item["id"].(string)] = item
	}
	if snapByID["old-a"]["round_batch"] != 2 || snapByID["new-a"]["round_batch"] != 1 {
		t.Fatalf("snapshot grouping = %+v / %+v", snapByID["old-a"], snapByID["new-a"])
	}
}

func TestArtifactRoundColumnMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE task_artifacts (
		id TEXT PRIMARY KEY,
		task_id TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL DEFAULT 'file',
		mime TEXT NOT NULL DEFAULT '',
		filename TEXT NOT NULL DEFAULT '',
		size INTEGER NOT NULL DEFAULT 0,
		storage_path TEXT NOT NULL DEFAULT '',
		url TEXT NOT NULL DEFAULT '',
		source_session TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateTask(map[string]any{"id": "T-M", "title": "迁移"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateArtifact(map[string]any{
		"id": "A-M", "task_id": "T-M", "title": "新产物", "kind": "html",
		"filename": "m.html", "storage_path": "T-M/A-M/m.html", "round": 5,
	}); err != nil {
		t.Fatal(err)
	}
	task, err := st.GetTask("T-M")
	if err != nil {
		t.Fatal(err)
	}
	if got := task["artifacts"].([]map[string]any)[0]["round"]; got != 5 {
		t.Fatalf("round after migration = %v", got)
	}
}
