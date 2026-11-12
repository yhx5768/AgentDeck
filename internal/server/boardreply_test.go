package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cc-webui/internal/runner"
)

func boardReplyTask(t *testing.T, s *Server, id string) map[string]any {
	t.Helper()
	if _, err := s.boardDB.CreateTask(map[string]any{"id": id, "title": "回复回写"}); err != nil {
		t.Fatal(err)
	}
	task, err := s.boardDB.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func boardReplyEvents(task map[string]any) []map[string]any {
	raw, _ := task["timeline"].([]map[string]any)
	if raw != nil {
		return raw
	}
	// GetTask returns []map[string]any, but be tolerant of a JSON round trip.
	items, _ := task["timeline"].([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if ev, ok := item.(map[string]any); ok {
			out = append(out, ev)
		}
	}
	return out
}

func countEventsOfKind(task map[string]any, kind string) int {
	n := 0
	for _, ev := range boardReplyEvents(task) {
		if ev["kind"] == kind {
			n++
		}
	}
	return n
}

func TestPostMessageBindsBoardTask(t *testing.T) {
	s, st := boardMCPServer(t)
	boardReplyTask(t, s, "T-1")

	post := func(sessionID, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sessionID+"/messages",
			strings.NewReader(body))
		req.SetPathValue("id", sessionID)
		w := httptest.NewRecorder()
		s.handlePostMessage(w, req)
		return w
	}

	w := post("main", `{"text":"看板提问","board_task_id":"T-1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("post: code=%d body=%s", w.Code, w.Body.String())
	}
	events, err := st.Events("main")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Kind != "user" {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data["board_task_id"] == "T-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("user event not bound to the card: %+v", events)
	}

	// An ordinary message (no board_task_id) must not carry a binding.
	if w := post("main", `{"text":"普通消息"}`); w.Code != http.StatusOK {
		t.Fatalf("plain post: code=%d body=%s", w.Code, w.Body.String())
	}
	events, _ = st.Events("main")
	var lastUser map[string]any
	for _, ev := range events {
		if ev.Kind == "user" {
			lastUser = map[string]any{}
			_ = json.Unmarshal(ev.Data, &lastUser)
		}
	}
	if _, ok := lastUser["board_task_id"]; ok {
		t.Fatalf("ordinary message got a board binding: %+v", lastUser)
	}

	// Unknown card and non-main-agent sessions are rejected before enqueue.
	userEvents := func() int {
		events, _ := st.Events("main")
		n := 0
		for _, ev := range events {
			if ev.Kind == "user" {
				n++
			}
		}
		return n
	}
	before := userEvents()
	if w := post("main", `{"text":"x","board_task_id":"T-NOPE"}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown card: code=%d body=%s", w.Code, w.Body.String())
	}
	if w := post("worker", `{"text":"x","board_task_id":"T-1"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("non-main agent: code=%d body=%s", w.Code, w.Body.String())
	}
	if after := userEvents(); after != before {
		t.Fatalf("rejected posts enqueued a turn: before=%d after=%d", before, after)
	}
}

func TestBoardTurnFinalIdempotentAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s := boardDBServerAt(t, dir)
	boardReplyTask(t, s, "T-1")

	final := runner.TurnFinal{
		SessionID: "main", BoardTaskID: "T-1", TurnID: "main#3", UserSeq: 3,
		Text: "## 结论\n\n- 已完成\n- 测试通过\n",
	}
	s.onBoardTurnFinal(final)
	s.onBoardTurnFinal(final) // duplicate callback in the same process

	task, err := s.boardDB.GetTask("T-1")
	if err != nil {
		t.Fatal(err)
	}
	if n := countEventsOfKind(task, "主 Agent 回复"); n != 1 {
		t.Fatalf("reply events = %d, timeline=%+v", n, task["timeline"])
	}
	for _, ev := range boardReplyEvents(task) {
		if ev["kind"] != "主 Agent 回复" {
			continue
		}
		if ev["text"] != final.Text {
			t.Fatalf("stored text = %q", ev["text"])
		}
		data, _ := ev["data"].(map[string]any)
		if data["auto"] != true || data["turn_id"] != "main#3" || data["session"] != "main" {
			t.Fatalf("event data = %+v", data)
		}
	}

	// Reopen the same DB (process restart) and replay the same turn: the
	// idempotency key is persisted, so no second reply appears.
	s.closeAgentBoardDB()
	s2 := boardDBServerAt(t, dir)
	s2.onBoardTurnFinal(final)
	task, err = s2.boardDB.GetTask("T-1")
	if err != nil {
		t.Fatal(err)
	}
	if n := countEventsOfKind(task, "主 Agent 回复"); n != 1 {
		t.Fatalf("reply events after restart = %d", n)
	}

	// A different turn appends its own reply.
	s2.onBoardTurnFinal(runner.TurnFinal{
		SessionID: "main", BoardTaskID: "T-1", TurnID: "main#4", UserSeq: 4, Text: "第二轮回复",
	})
	task, _ = s2.boardDB.GetTask("T-1")
	if n := countEventsOfKind(task, "主 Agent 回复"); n != 2 {
		t.Fatalf("reply events after second turn = %d", n)
	}
}

func TestManualCommentDoesNotDuplicateAutoReply(t *testing.T) {
	s := boardDBServer(t)
	boardReplyTask(t, s, "T-1")
	// The automatic writeback preserves the final assistant text byte-for-byte,
	// leading newlines included; manual dedupe compares the trimmed text.
	const storedText = "\n\n最终回复（含 Markdown）"
	const manualText = "最终回复（含 Markdown）"
	s.onBoardTurnFinal(runner.TurnFinal{
		SessionID: "main", BoardTaskID: "T-1", TurnID: "main#1", UserSeq: 1, Text: storedText,
	})

	post := func() map[string]any {
		req := httptest.NewRequest(http.MethodPost, "/api/agent-board/tasks/T-1/events",
			strings.NewReader(`{"kind":"主 Agent 回复","text":"`+manualText+`"}`))
		req.SetPathValue("id", "T-1")
		w := httptest.NewRecorder()
		s.handleAgentBoardAppendEventDB(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("append: code=%d body=%s", w.Code, w.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := post(); got["duplicate"] != true || got["deduped_by"] != "auto_reply" {
		t.Fatalf("manual duplicate response = %+v", got)
	}
	task, _ := s.boardDB.GetTask("T-1")
	if n := countEventsOfKind(task, "主 Agent 回复"); n != 1 {
		t.Fatalf("duplicated reply events = %d", n)
	}
	for _, ev := range boardReplyEvents(task) {
		if ev["kind"] == "主 Agent 回复" && ev["text"] != storedText {
			t.Fatalf("auto reply text was not preserved verbatim: %q", ev["text"])
		}
	}

	// The reverse order: the model posts first, then the turn callback fires.
	boardReplyTask(t, s, "T-2")
	const manual = "手动先贴的回复"
	req := httptest.NewRequest(http.MethodPost, "/api/agent-board/tasks/T-2/events",
		strings.NewReader(`{"kind":"主 Agent 回复","text":"`+manual+`"}`))
	req.SetPathValue("id", "T-2")
	w := httptest.NewRecorder()
	s.handleAgentBoardAppendEventDB(w, req)
	s.onBoardTurnFinal(runner.TurnFinal{
		SessionID: "main", BoardTaskID: "T-2", TurnID: "main#2", UserSeq: 2, Text: manual,
	})
	task, _ = s.boardDB.GetTask("T-2")
	if n := countEventsOfKind(task, "主 Agent 回复"); n != 1 {
		t.Fatalf("reverse-order duplicate events = %d", n)
	}

	// A user message repeating the reply text is not a model duplicate.
	userReq2 := httptest.NewRequest(http.MethodPost, "/api/agent-board/tasks/T-1/events",
		strings.NewReader(`{"kind":"用户消息","source":"drawer","text":"`+manualText+`"}`))
	userReq2.SetPathValue("id", "T-1")
	userW2 := httptest.NewRecorder()
	s.handleAgentBoardAppendEventDB(userW2, userReq2)
	task, _ = s.boardDB.GetTask("T-1")
	nUser := 0
	for _, ev := range boardReplyEvents(task) {
		if ev["kind"] == "用户消息" && ev["text"] == manualText {
			nUser++
		}
	}
	if nUser != 1 {
		t.Fatalf("user message with reply text was deduped: %d", nUser)
	}

	// A user message whose text happens to equal the answer must not suppress
	// the automatic writeback.
	boardReplyTask(t, s, "T-3")
	userReq := httptest.NewRequest(http.MethodPost, "/api/agent-board/tasks/T-3/events",
		strings.NewReader(`{"kind":"用户消息","source":"drawer","text":"继续"}`))
	userReq.SetPathValue("id", "T-3")
	userW := httptest.NewRecorder()
	s.handleAgentBoardAppendEventDB(userW, userReq)
	s.onBoardTurnFinal(runner.TurnFinal{
		SessionID: "main", BoardTaskID: "T-3", TurnID: "main#3", UserSeq: 3, Text: "继续",
	})
	task, _ = s.boardDB.GetTask("T-3")
	if n := countEventsOfKind(task, "主 Agent 回复"); n != 1 {
		t.Fatalf("user-message text suppressed auto reply: events=%d", n)
	}
}
