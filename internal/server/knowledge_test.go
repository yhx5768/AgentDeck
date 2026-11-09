package server

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"cc-webui/internal/boarddb"
	"cc-webui/internal/knowledge"
)

func newKnowledgeTestServer(t *testing.T) (*Server, *boarddb.Store, *knowledge.Store) {
	t.Helper()
	dir := t.TempDir()
	boardSt, err := boarddb.Open(filepath.Join(dir, "agent-board.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { boardSt.Close() })
	knowledgeSt, err := knowledge.Open(filepath.Join(dir, "knowledge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { knowledgeSt.Close() })
	return &Server{boardDB: boardSt, knowledgeDB: knowledgeSt}, boardSt, knowledgeSt
}

func TestCollectKnowledgeIdempotentAndScoped(t *testing.T) {
	s, boardSt, knowledgeSt := newKnowledgeTestServer(t)
	task, err := boardSt.CreateTask(map[string]any{
		"title": "SSE 重编号", "status": "待验收", "project": "AgentDeck",
		"description": "sendSeq 与存储 seq 无关", "tags": []string{"SSE"}, "report_file": "/tmp/legacy-report.md",
	})
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := task["id"].(string)
	if _, err := boardSt.CreateArtifact(map[string]any{
		"task_id": taskID, "title": "验证报告", "kind": "markdown",
		"filename": "report.md", "storage_path": "demo/report.md", "size": 12,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := boardSt.CreateTask(map[string]any{"title": "进行中任务", "status": "进行中"}); err != nil {
		t.Fatal(err)
	}

	admin := knowledgeActor{Address: "mainAgent", Admin: true}
	result, err := s.knowledgeCollectData(admin)
	if err != nil {
		t.Fatal(err)
	}
	if result["created"] != 1 {
		t.Fatalf("first collect: %#v", result)
	}
	res, err := knowledgeSt.Search(knowledge.SearchOptions{Query: "重编号", IncludeDrafts: true})
	if err != nil || res.Total != 1 {
		t.Fatalf("collected entry not searchable: total=%d err=%v", res.Total, err)
	}
	entry := res.Entries[0]
	if entry.SourceKind != "collect" || entry.SourceTaskID != taskID || entry.Status != "draft" {
		t.Fatalf("unexpected collected entry: %#v", entry)
	}
	if !strings.Contains(entry.Body, "验证报告") || !strings.Contains(entry.Summary, "sendSeq") {
		t.Fatalf("body/summary missing task/artifact content: body=%s summary=%s", entry.Body, entry.Summary)
	}
	if strings.Contains(entry.Body, "legacy-report") {
		t.Fatalf("legacy report_file path leaked into body: %s", entry.Body)
	}
	if len(entry.SourceArtifactIDs) != 1 {
		t.Fatalf("artifact ids: %#v", entry.SourceArtifactIDs)
	}

	second, err := s.knowledgeCollectData(admin)
	if err != nil {
		t.Fatal(err)
	}
	if second["created"] != 0 || second["skipped"] == 0 {
		t.Fatalf("second collect not idempotent: %#v", second)
	}
}

func TestWorkerKnowledgePermissions(t *testing.T) {
	s, boardSt, _ := newKnowledgeTestServer(t)
	task, err := boardSt.CreateTask(map[string]any{"title": "来源任务", "project": "P", "status": "进行中"})
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := task["id"].(string)
	worker := knowledgeActor{SessionID: "sess-worker", Address: "worker", Admin: false}
	if _, err := s.knowledgeAddData(worker, map[string]any{"title": "没有来源", "body": "x"}); err == nil {
		t.Fatal("worker add without source_task_id should fail")
	}
	result, err := s.knowledgeAddData(worker, map[string]any{
		"title": "worker 结论", "body": "只写草稿", "source_task_id": taskID, "status": "approved",
	})
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := result["entry"].(map[string]any)
	if !ok || entry["status"] != "draft" {
		t.Fatalf("worker entry must be draft: %#v", result)
	}
	if s.knowledgeActorForMCP("").Admin {
		t.Fatal("missing MCP session must not be admin")
	}
	if s.knowledgeActorForMCP("unknown-session").Admin {
		t.Fatal("unknown MCP session must not be admin")
	}
}

func TestKnowledgeSearchVisibilityAndHTTP(t *testing.T) {
	s, _, knowledgeSt := newKnowledgeTestServer(t)
	approvedText := "approved"
	if _, _, err := knowledgeSt.Create(knowledge.CreateInput{
		Title: "公开知识", Body: "已通过内容", Project: "P", Status: approvedText, Actor: "mainAgent",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := knowledgeSt.Create(knowledge.CreateInput{
		Title: "草稿知识", Body: "待审内容", Project: "P", Actor: "mainAgent",
	}); err != nil {
		t.Fatal(err)
	}
	worker := knowledgeActor{Address: "worker", Admin: false}
	res, err := s.knowledgeSearchData(worker, knowledgeSearchParams{Query: "知识"})
	if err != nil {
		t.Fatal(err)
	}
	if res["total"] != 1 {
		t.Fatalf("worker should see only approved: %#v", res)
	}
	admin := knowledgeActor{Address: "mainAgent", Admin: true}
	res, err = s.knowledgeSearchData(admin, knowledgeSearchParams{Query: "知识", Status: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if res["total"] != 2 {
		t.Fatalf("admin should see drafts+approved: %#v", res)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/knowledge?q=%E7%9F%A5%E8%AF%86&status=all", nil)
	s.handleKnowledgeList(rec, req)
	if rec.Code != 200 {
		t.Fatalf("HTTP list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Entries []map[string]any `json:"entries"`
		Total   int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Total != 2 || len(payload.Entries) != 2 {
		t.Fatalf("HTTP list payload: %#v", payload)
	}
	if _, ok := payload.Entries[0]["body"]; ok {
		t.Fatalf("list response must not include body: %#v", payload.Entries[0])
	}
}
