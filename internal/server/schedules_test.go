package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cc-webui/internal/a2a"
	"cc-webui/internal/schedule"
	"cc-webui/internal/store"
)

func scheduleServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	s, st := createResidentServer(t)
	if err := st.Upsert(&store.Session{
		ID: "main", Name: "主 Agent", PeerName: "mainAgent", MainAgent: true,
		Cwd: t.TempDir(), CLI: "claude", Provider: "gw",
	}, nil); err != nil {
		t.Fatal(err)
	}
	return s, st
}

func scheduleCall(t *testing.T, s *Server, method, path, id string, body any, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if id != "" {
		req.SetPathValue("id", id)
	}
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func createSchedule(t *testing.T, s *Server, body map[string]any) map[string]any {
	t.Helper()
	w := scheduleCall(t, s, http.MethodPost, "/api/schedules", "", body, s.handleCreateSchedule)
	if w.Code != http.StatusOK {
		t.Fatalf("create: code=%d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode create: %v (%s)", err, w.Body.String())
	}
	return out
}

func listSchedules(t *testing.T, s *Server) []map[string]any {
	t.Helper()
	w := scheduleCall(t, s, http.MethodGet, "/api/schedules", "", nil, s.handleListSchedules)
	if w.Code != http.StatusOK {
		t.Fatalf("list: code=%d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Tasks []map[string]any `json:"tasks"`
		Now   int64            `json:"now"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list: %v (%s)", err, w.Body.String())
	}
	if out.Now == 0 {
		t.Fatal("list should include server now")
	}
	return out.Tasks
}

// Empty body returns [] (not null) and every field the UI needs is present.
func TestSchedulesAPIEmptyAndCreate(t *testing.T) {
	s, _ := scheduleServer(t)
	w := scheduleCall(t, s, http.MethodGet, "/api/schedules", "", nil, s.handleListSchedules)
	if !strings.Contains(w.Body.String(), `"tasks":[]`) {
		t.Fatalf("empty list must marshal tasks as []: %s", w.Body.String())
	}

	// Empty target defaults to mainAgent and resolves to its id.
	out := createSchedule(t, s, map[string]any{
		"name": "每日站会", "cron": "0 9 * * *", "prompt": "总结昨天的工作",
	})
	if out["target_session"] != "main" || out["target_name"] != "主 Agent" {
		t.Fatalf("target not resolved: %+v", out)
	}
	if out["enabled"] != true {
		t.Fatalf("enabled should default true: %+v", out)
	}
	if out["next_run_at"] == nil || out["next_run_at"] == "" {
		t.Fatalf("next_run_at missing: %+v", out)
	}

	// Validation: bad cron, unknown target, empty prompt.
	for name, body := range map[string]map[string]any{
		"bad cron":     {"name": "x", "cron": "not a cron", "prompt": "p"},
		"bad target":   {"name": "x", "cron": "0 9 * * *", "prompt": "p", "target_session": "ghost"},
		"empty prompt": {"name": "x", "cron": "0 9 * * *", "prompt": "  "},
		"empty name":   {"name": " ", "cron": "0 9 * * *", "prompt": "p"},
		"never-fires":  {"name": "x", "cron": "0 0 30 2 *", "prompt": "p"},
	} {
		w := scheduleCall(t, s, http.MethodPost, "/api/schedules", "", body, s.handleCreateSchedule)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: code=%d want 400 (body=%s)", name, w.Code, w.Body.String())
		}
	}

	// Address names resolve too, and are stored as the concrete session id.
	out = createSchedule(t, s, map[string]any{
		"name": "按地址名", "cron": "*/30 * * * *", "prompt": "ping", "target_session": "mainAgent",
	})
	if out["target_session"] != "main" {
		t.Fatalf("address target not resolved: %+v", out)
	}
	if got := len(listSchedules(t, s)); got != 2 {
		t.Fatalf("list len = %d, want 2", got)
	}
}

func TestSchedulesAPIUpdateEnableDelete(t *testing.T) {
	s, _ := scheduleServer(t)
	task := createSchedule(t, s, map[string]any{
		"name": "巡检", "cron": "0 9 * * *", "prompt": "旧提示",
	})
	id := task["id"].(string)

	w := scheduleCall(t, s, http.MethodPut, "/api/schedules/"+id, id, map[string]any{
		"name": "巡检 v2", "cron": "*/5 * * * *", "prompt": "新提示", "enabled": true,
	}, s.handleUpdateSchedule)
	if w.Code != http.StatusOK {
		t.Fatalf("update: code=%d body=%s", w.Code, w.Body.String())
	}
	var updated map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &updated)
	if updated["name"] != "巡检 v2" || updated["cron"] != "*/5 * * * *" || updated["prompt"] != "新提示" {
		t.Fatalf("update not applied: %+v", updated)
	}

	// Disabling clears the next run; enabling computes a fresh one.
	w = scheduleCall(t, s, http.MethodPost, "/api/schedules/"+id+"/enable", id, map[string]any{"enabled": false}, s.handleEnableSchedule)
	if w.Code != http.StatusOK {
		t.Fatalf("disable: code=%d body=%s", w.Code, w.Body.String())
	}
	var disabled map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &disabled)
	if disabled["enabled"] != false || disabled["next_run_at"] != nil {
		t.Fatalf("disable result: %+v", disabled)
	}
	w = scheduleCall(t, s, http.MethodPost, "/api/schedules/"+id+"/enable", id, map[string]any{"enabled": true}, s.handleEnableSchedule)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var enabled map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &enabled)
	if enabled["next_run_at"] == nil {
		t.Fatalf("enable did not compute next run: %+v", enabled)
	}

	// Unknown ids are 404 on every operation.
	for _, call := range []struct {
		name    string
		handler http.HandlerFunc
		method  string
		body    any
	}{
		{"update", s.handleUpdateSchedule, http.MethodPut, map[string]any{"name": "x", "cron": "0 9 * * *", "prompt": "p"}},
		{"enable", s.handleEnableSchedule, http.MethodPost, map[string]any{"enabled": true}},
		{"run", s.handleRunSchedule, http.MethodPost, nil},
		{"delete", s.handleDeleteSchedule, http.MethodDelete, nil},
	} {
		w := scheduleCall(t, s, call.method, "/api/schedules/nope", "nope", call.body, call.handler)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s on unknown id: code=%d body=%s", call.name, w.Code, w.Body.String())
		}
	}

	w = scheduleCall(t, s, http.MethodDelete, "/api/schedules/"+id, id, nil, s.handleDeleteSchedule)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: code=%d body=%s", w.Code, w.Body.String())
	}
	if got := len(listSchedules(t, s)); got != 0 {
		t.Fatalf("list after delete = %d", got)
	}
}

func TestSchedulePreview(t *testing.T) {
	s, _ := scheduleServer(t)
	w := scheduleCall(t, s, http.MethodPost, "/api/schedules/preview", "", map[string]any{"cron": "*/15 * * * *"}, s.handlePreviewSchedule)
	if w.Code != http.StatusOK {
		t.Fatalf("preview: code=%d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["next_run_local"] == nil || out["next_run_at"] == nil {
		t.Fatalf("preview missing next run: %+v", out)
	}
	w = scheduleCall(t, s, http.MethodPost, "/api/schedules/preview", "", map[string]any{"cron": "61 * * * *"}, s.handlePreviewSchedule)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid preview: code=%d want 400", w.Code)
	}
}

func userEvents(t *testing.T, st *store.Store, sid string) []*store.Event {
	t.Helper()
	evs, err := st.Events(sid)
	if err != nil {
		t.Fatal(err)
	}
	var out []*store.Event
	for _, ev := range evs {
		if ev.Kind == "user" {
			out = append(out, ev)
		}
	}
	return out
}

// run-now goes through the delivery seam, records the outcome, and does not
// shift the cron schedule. The seam is stubbed so the test never spawns a CLI.
func TestScheduleRunNowDeliversAndRecords(t *testing.T) {
	s, _ := scheduleServer(t)
	type call struct{ from, to, msg string }
	var calls []call
	s.schedDeliver = func(from, to, msg string) error {
		calls = append(calls, call{from, to, msg})
		return nil
	}
	task := createSchedule(t, s, map[string]any{
		"name": "手动跑一下", "cron": "0 9 * * *", "prompt": "检查构建",
	})
	id := task["id"].(string)
	nextBefore := task["next_run_at"]

	w := scheduleCall(t, s, http.MethodPost, "/api/schedules/"+id+"/run", id, nil, s.handleRunSchedule)
	if w.Code != http.StatusOK {
		t.Fatalf("run: code=%d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["last_status"] != "ok" || out["last_run_at"] == nil {
		t.Fatalf("run result: %+v", out)
	}
	if out["next_run_at"] != nextBefore {
		t.Fatalf("run-now shifted next_run_at: %v -> %v", nextBefore, out["next_run_at"])
	}
	if len(calls) != 1 || calls[0].to != "main" || calls[0].msg != "检查构建" {
		t.Fatalf("delivery calls = %+v", calls)
	}
	if !strings.Contains(calls[0].from, "手动跑一下") {
		t.Fatalf("sender should carry the task name: %q", calls[0].from)
	}

	// A delivery failure reports it to the caller and records it on the task.
	s.schedDeliver = func(from, to, msg string) error { return fmt.Errorf("目标会话不存在") }
	w = scheduleCall(t, s, http.MethodPost, "/api/schedules/"+id+"/run", id, nil, s.handleRunSchedule)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("failed run: code=%d body=%s", w.Code, w.Body.String())
	}
	got := s.scheduleStore().Get(id)
	if got == nil || !strings.HasPrefix(got.LastStatus, "error: ") {
		t.Fatalf("failure not recorded: %+v", got)
	}
}

// The due loop claims once: a second pass for the same minute must not deliver
// the same occurrence again.
func TestScheduleDueFiresOnce(t *testing.T) {
	s, _ := scheduleServer(t)
	calls := 0
	s.schedDeliver = func(from, to, msg string) error {
		calls++
		if to != "main" || msg != "tick" {
			t.Errorf("unexpected delivery: to=%q msg=%q", to, msg)
		}
		return nil
	}
	task := createSchedule(t, s, map[string]any{
		"name": "每分钟", "cron": "* * * * *", "prompt": "tick",
	})
	id := task["id"].(string)
	now := time.Now()
	past := now.Add(-2 * time.Minute).Truncate(time.Minute)
	if _, err := s.scheduleStore().Update(id, func(t *schedule.Task) { t.NextRunAt = &past }); err != nil {
		t.Fatal(err)
	}

	s.runDueSchedules(now)
	if calls != 1 {
		t.Fatalf("due run delivered %d times, want 1", calls)
	}
	s.runDueSchedules(now)
	if calls != 1 {
		t.Fatalf("second pass double-fired: %d deliveries", calls)
	}
	got := s.scheduleStore().Get(id)
	if got.LastStatus != "ok" || got.LastRunAt == nil {
		t.Fatalf("due run not recorded: %+v", got)
	}
	if got.NextRunAt == nil || !got.NextRunAt.After(now) {
		t.Fatalf("next_run_at not advanced past now: %+v", got.NextRunAt)
	}
}

// Without a test seam, fireSchedule uses deliverA2A: the prompt lands as a
// persisted A2A user event (source=a2a + envelope) on the target session.
func TestScheduleFireUsesA2ADelivery(t *testing.T) {
	s, st := scheduleServer(t)
	status := s.fireSchedule(&schedule.Task{Name: "真实投递", Prompt: "检查构建", TargetSession: "main"})
	if status != "ok" {
		t.Fatalf("fireSchedule = %q", status)
	}
	evs := userEvents(t, st, "main")
	if len(evs) != 1 {
		t.Fatalf("user events = %d, want 1", len(evs))
	}
	var data struct {
		Text   string `json:"text"`
		Source string `json:"source"`
		From   string `json:"from"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal(evs[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Source != a2a.Source || data.Body != "检查构建" || !strings.Contains(data.From, "真实投递") {
		t.Fatalf("stored event = %+v", data)
	}
	if from, body, ok := a2a.Parse(data.Text); !ok || from != data.From || body != "检查构建" {
		t.Fatalf("envelope not parseable: ok=%v from=%q body=%q text=%q", ok, from, body, data.Text)
	}
	// Let the (nonexistent) CLI fail and finish writing status before the temp
	// dir is removed; start errors leave the runner busy forever, so poll the
	// event log rather than Busy here.
	waitStatusError(t, st, "main")
}

func waitStatusError(t *testing.T, st *store.Store, sid string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		evs, _ := st.Events(sid)
		for _, ev := range evs {
			if ev.Kind != "status" {
				continue
			}
			var d map[string]string
			_ = json.Unmarshal(ev.Data, &d)
			if d["state"] == "error" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker did not emit a status error")
}
