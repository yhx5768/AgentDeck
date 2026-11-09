package boarddb

import (
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
