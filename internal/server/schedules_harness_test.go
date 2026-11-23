package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cc-webui/internal/schedule"
)

func writeHarnessFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", schedule.HarnessFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"tasks":[
	  {"id":"h-once","cron":"0 10 24 11 *","prompt":"M145 稳定性检查\n第二步细节","createdAt":1795435664093},
	  {"id":"h-daily","cron":"7 9 * * 1-5","prompt":"每日看板巡检","recurring":true,"createdAt":1795435965360}
	]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type harnessResp struct {
	Path       string            `json:"path"`
	Candidates []string          `json:"candidates"`
	Error      string            `json:"error"`
	Tasks      []harnessTaskView `json:"tasks"`
}

func getHarness(t *testing.T, s *Server, path string) harnessResp {
	t.Helper()
	url := "/api/schedules/harness"
	if path != "" {
		url += "?path=" + path
	}
	w := scheduleCall(t, s, http.MethodGet, url, "", nil, s.handleHarnessTasks)
	if w.Code != http.StatusOK {
		t.Fatalf("harness list: code=%d body=%s", w.Code, w.Body.String())
	}
	var out harnessResp
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func importHarness(t *testing.T, s *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return scheduleCall(t, s, http.MethodPost, "/api/schedules/import-harness", "", body, s.handleImportHarnessTask)
}

func TestHarnessListAndImport(t *testing.T) {
	s, _ := scheduleServer(t)
	path := writeHarnessFile(t)

	got := getHarness(t, s, path)
	if got.Path != path || len(got.Tasks) != 2 {
		t.Fatalf("harness list = %+v", got)
	}
	once := got.Tasks[0]
	if once.ID != "h-once" || once.Recurring || !once.Valid || once.Name != "M145 稳定性检查" {
		t.Fatalf("once view = %+v", once)
	}
	if got.Tasks[1].ID != "h-daily" || !got.Tasks[1].Recurring || !got.Tasks[1].Valid {
		t.Fatalf("daily view = %+v", got.Tasks[1])
	}
	if got.Tasks[0].ImportedID != "" {
		t.Fatalf("not imported yet: %+v", got.Tasks[0])
	}

	w := importHarness(t, s, map[string]any{"path": path, "id": "h-once"})
	if w.Code != http.StatusOK {
		t.Fatalf("import: code=%d body=%s", w.Code, w.Body.String())
	}
	var imported scheduleView
	if err := json.Unmarshal(w.Body.Bytes(), &imported); err != nil {
		t.Fatal(err)
	}
	if imported.Source != "harness" || imported.SourceID != "h-once" {
		t.Fatalf("source not recorded: %+v", imported.Task)
	}
	if imported.Enabled || imported.NextRunAt != nil {
		t.Fatalf("import must default to disabled: %+v", imported.Task)
	}
	if imported.MaxRuns != 1 || imported.RunCount != 0 {
		t.Fatalf("once task should carry max_runs=1: %+v", imported.Task)
	}
	if imported.Prompt != "M145 稳定性检查\n第二步细节" || imported.Cron != "0 10 24 11 *" {
		t.Fatalf("payload mismatch: %+v", imported.Task)
	}
	if imported.TargetSession != "main" {
		t.Fatalf("default target = %q", imported.TargetSession)
	}

	// Duplicate import is a 409, not a second copy.
	if w := importHarness(t, s, map[string]any{"path": path, "id": "h-once"}); w.Code != http.StatusConflict {
		t.Fatalf("duplicate import: code=%d body=%s", w.Code, w.Body.String())
	}
	got = getHarness(t, s, path)
	if got.Tasks[0].ImportedID != imported.ID {
		t.Fatalf("imported marker = %q, want %q", got.Tasks[0].ImportedID, imported.ID)
	}

	// Recurring task imports without the one-shot cap.
	w = importHarness(t, s, map[string]any{"path": path, "id": "h-daily"})
	if w.Code != http.StatusOK {
		t.Fatalf("import daily: code=%d body=%s", w.Code, w.Body.String())
	}
	var daily scheduleView
	_ = json.Unmarshal(w.Body.Bytes(), &daily)
	if daily.MaxRuns != 0 || daily.SourceID != "h-daily" {
		t.Fatalf("daily import = %+v", daily.Task)
	}

	// The UI's path box is restricted to scheduled_tasks.json.
	if w := scheduleCall(t, s, http.MethodGet, "/api/schedules/harness?path=/tmp/other.json", "", nil, s.handleHarnessTasks); w.Code != http.StatusBadRequest {
		t.Fatalf("arbitrary path: code=%d body=%s", w.Code, w.Body.String())
	}
	if w := scheduleCall(t, s, http.MethodGet, "/api/schedules/harness?path=/tmp/"+schedule.HarnessFileName, "", nil, s.handleHarnessTasks); w.Code != http.StatusBadRequest {
		t.Fatalf("missing file: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestImportedOneShotRunsOnceThenDisables(t *testing.T) {
	s, _ := scheduleServer(t)
	calls := 0
	s.schedDeliver = func(from, to, msg string) error { calls++; return nil }
	path := writeHarnessFile(t)
	w := importHarness(t, s, map[string]any{"path": path, "id": "h-once"})
	if w.Code != http.StatusOK {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	var imported scheduleView
	_ = json.Unmarshal(w.Body.Bytes(), &imported)
	id := imported.ID

	// Enable computes the next occurrence.
	w = scheduleCall(t, s, http.MethodPost, "/api/schedules/"+id+"/enable", id, map[string]any{"enabled": true}, s.handleEnableSchedule)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}

	// Fire it due-now; a one-shot disables itself after the run.
	past := time.Now().Add(-time.Minute)
	if _, err := s.scheduleStore().Update(id, func(t *schedule.Task) { t.NextRunAt = &past }); err != nil {
		t.Fatal(err)
	}
	s.runDueSchedules(time.Now())
	if calls != 1 {
		t.Fatalf("deliveries = %d, want 1", calls)
	}
	got := s.scheduleStore().Get(id)
	if got == nil || got.Enabled || got.NextRunAt != nil || got.RunCount != 1 || got.MaxRuns != 1 {
		t.Fatalf("after one run = %+v", got)
	}
	s.runDueSchedules(time.Now())
	if calls != 1 {
		t.Fatalf("exhausted one-shot fired again: %d deliveries", calls)
	}

	// Explicitly re-enabling starts a fresh cycle (run_count reset).
	w = scheduleCall(t, s, http.MethodPost, "/api/schedules/"+id+"/enable", id, map[string]any{"enabled": true}, s.handleEnableSchedule)
	if w.Code != http.StatusOK {
		t.Fatalf("re-enable: %d %s", w.Code, w.Body.String())
	}
	got = s.scheduleStore().Get(id)
	if !got.Enabled || got.RunCount != 0 || got.NextRunAt == nil {
		t.Fatalf("re-enable = %+v", got)
	}
}

func TestParseScheduleAPI(t *testing.T) {
	s, _ := scheduleServer(t)
	w := scheduleCall(t, s, http.MethodPost, "/api/schedules/parse", "", map[string]any{
		"text": "每天9点进行看板巡检，检查常驻worker和运行中卡片是否异常",
	}, s.handleParseSchedule)
	if w.Code != http.StatusOK {
		t.Fatalf("parse: code=%d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Mode       string `json:"mode"`
		Candidates []struct {
			Cron         string `json:"cron"`
			Label        string `json:"label"`
			Name         string `json:"name"`
			NextRunLocal string `json:"next_run_local"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Mode != "rules" || len(out.Candidates) != 1 {
		t.Fatalf("parse result = %+v", out)
	}
	c := out.Candidates[0]
	if c.Cron != "0 9 * * *" || c.Name != "看板巡检" || c.NextRunLocal == "" {
		t.Fatalf("candidate = %+v", c)
	}

	// Two concrete times produce two candidates (UI lets the user pick).
	w = scheduleCall(t, s, http.MethodPost, "/api/schedules/parse", "", map[string]any{"text": "每天9点和18点巡检"}, s.handleParseSchedule)
	if w.Code != http.StatusOK {
		t.Fatalf("ambiguous parse: %d %s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Candidates) != 2 {
		t.Fatalf("ambiguous candidates = %+v", out.Candidates)
	}
	// Missing frequency/time is a 400 with guidance.
	for _, text := range []string{"9点巡检", "每天巡检"} {
		w = scheduleCall(t, s, http.MethodPost, "/api/schedules/parse", "", map[string]any{"text": text}, s.handleParseSchedule)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%q: code=%d body=%s", text, w.Code, w.Body.String())
		}
	}
}
