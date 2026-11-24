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
	"cc-webui/internal/store"
)

func writeHarnessAt(t *testing.T, root, raw string) string {
	t.Helper()
	path := filepath.Join(root, ".claude", schedule.HarnessFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeHarnessFile(t *testing.T) string {
	t.Helper()
	return writeHarnessAt(t, t.TempDir(), `{"tasks":[
	  {"id":"h-once","cron":"0 10 24 11 *","prompt":"M145 稳定性检查\n第二步细节","createdAt":1795435664093},
	  {"id":"h-daily","cron":"7 9 * * 1-5","prompt":"每日看板巡检","recurring":true,"createdAt":1795435965360}
	]}`)
}

type harnessResp struct {
	Path       string             `json:"path"`
	Candidates []string           `json:"candidates"`
	Groups     []harnessGroupView `json:"groups"`
	Files      []harnessFileView  `json:"files"`
	Error      string             `json:"error"`
	Tasks      []harnessTaskView  `json:"tasks"`
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

func setMainCwd(t *testing.T, st *store.Store, cwd string) {
	t.Helper()
	sess := st.Get("main")
	if sess == nil {
		t.Fatal("main session missing")
	}
	if err := st.Upsert(sess, func(s *store.Session) { s.Cwd = cwd }); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessListAndImport(t *testing.T) {
	s, st := scheduleServer(t)
	path := writeHarnessFile(t)
	got := getHarness(t, s, path)
	if got.Path != path || len(got.Files) != 1 || len(got.Files[0].Tasks) != 2 {
		t.Fatalf("harness list = %+v", got)
	}
	once := got.Files[0].Tasks[0]
	if once.ID != "h-once" || once.Recurring || !once.Valid || once.Name != "M145 稳定性检查" {
		t.Fatalf("once view = %+v", once)
	}
	if got.Files[0].Tasks[1].ID != "h-daily" || !got.Files[0].Tasks[1].Recurring {
		t.Fatalf("daily view = %+v", got.Files[0].Tasks[1])
	}
	if once.ImportedID != "" {
		t.Fatalf("not imported yet: %+v", once)
	}
	_ = st

	w := importHarness(t, s, map[string]any{"path": path, "id": "h-once"})
	if w.Code != http.StatusOK {
		t.Fatalf("import: code=%d body=%s", w.Code, w.Body.String())
	}
	var imported scheduleView
	if err := json.Unmarshal(w.Body.Bytes(), &imported); err != nil {
		t.Fatal(err)
	}
	if imported.Source != "harness" || imported.SourceID != "h-once" || imported.SourcePath != path {
		t.Fatalf("source not recorded: %+v", imported.Task)
	}
	if imported.Enabled || imported.NextRunAt != nil || imported.MaxRuns != 1 {
		t.Fatalf("import must default to disabled one-shot: %+v", imported.Task)
	}
	if imported.Prompt != "M145 稳定性检查\n第二步细节" || imported.Cron != "0 10 24 11 *" {
		t.Fatalf("payload mismatch: %+v", imported.Task)
	}
	if imported.TargetSession != "main" {
		t.Fatalf("default target = %q", imported.TargetSession)
	}
	if w := importHarness(t, s, map[string]any{"path": path, "id": "h-once"}); w.Code != http.StatusConflict {
		t.Fatalf("duplicate import: code=%d body=%s", w.Code, w.Body.String())
	}
	got = getHarness(t, s, path)
	if got.Files[0].Tasks[0].ImportedID != imported.ID {
		t.Fatalf("imported marker = %q, want %q", got.Files[0].Tasks[0].ImportedID, imported.ID)
	}

	// The UI's path box is restricted to scheduled_tasks.json.
	if w := scheduleCall(t, s, http.MethodGet, "/api/schedules/harness?path=/tmp/other.json", "", nil, s.handleHarnessTasks); w.Code != http.StatusBadRequest {
		t.Fatalf("arbitrary path: code=%d body=%s", w.Code, w.Body.String())
	}
	if w := scheduleCall(t, s, http.MethodGet, "/api/schedules/harness?path=/tmp/"+schedule.HarnessFileName, "", nil, s.handleHarnessTasks); w.Code != http.StatusBadRequest {
		t.Fatalf("missing file: code=%d body=%s", w.Code, w.Body.String())
	}
}

// Multiple workspaces are discovered, grouped, and report per-file empty /
// malformed states without hiding the healthy ones.
func TestHarnessMultiWorkspaceGroups(t *testing.T) {
	s, st := scheduleServer(t)
	wsMain := t.TempDir()
	wsOther := t.TempDir()
	wsEmpty := t.TempDir()
	wsBad := t.TempDir()
	setMainCwd(t, st, wsMain)
	mainPath := writeHarnessAt(t, wsMain, `{"tasks":[{"id":"same-id","cron":"0 9 * * *","prompt":"main 任务"}]}`)
	otherPath := writeHarnessAt(t, wsOther, `{"tasks":[{"id":"same-id","cron":"0 10 * * *","prompt":"other 任务","recurring":true}]}`)
	emptyPath := writeHarnessAt(t, wsEmpty, `{"tasks":[]}`)
	badPath := writeHarnessAt(t, wsBad, `{not json`)
	t.Setenv("CC_WEBUI_HARNESS_TASKS", strings.Join([]string{otherPath, emptyPath, badPath}, string(filepath.ListSeparator)))

	got := getHarness(t, s, "")
	if len(got.Files) < 4 {
		t.Fatalf("files = %+v, want main/other/empty/bad", got.Files)
	}
	byPath := map[string]harnessFileView{}
	for _, f := range got.Files {
		byPath[f.Path] = f
	}
	if f := byPath[mainPath]; len(f.Tasks) != 1 || f.Tasks[0].ID != "same-id" {
		t.Fatalf("main file = %+v", f)
	}
	if f := byPath[otherPath]; len(f.Tasks) != 1 || !f.Tasks[0].Recurring {
		t.Fatalf("other file = %+v", f)
	}
	if f := byPath[emptyPath]; !f.Empty || f.Error != "" || len(f.Tasks) != 0 {
		t.Fatalf("empty file = %+v", f)
	}
	if f := byPath[badPath]; !strings.Contains(f.Error, "JSON") {
		t.Fatalf("bad file = %+v", f)
	}
	// mainAgent's workspace is the first group.
	if len(got.Groups) == 0 || got.Groups[0].Workspace != wsMain {
		t.Fatalf("groups = %+v, want main first", got.Groups)
	}
	// The same harness id in two paths is two independent tasks.
	for _, f := range got.Files {
		if f.Path != mainPath && f.Path != otherPath {
			continue
		}
		if len(f.Tasks) != 1 || f.Tasks[0].ID != "same-id" || f.Tasks[0].ImportedID != "" {
			t.Fatalf("file before import = %+v", f)
		}
	}
	w := importHarness(t, s, map[string]any{"path": mainPath, "id": "same-id"})
	if w.Code != http.StatusOK {
		t.Fatalf("import main: %d %s", w.Code, w.Body.String())
	}
	// Same id from the other workspace is not a duplicate.
	w = importHarness(t, s, map[string]any{"path": otherPath, "id": "same-id"})
	if w.Code != http.StatusOK {
		t.Fatalf("import other: %d %s", w.Code, w.Body.String())
	}
	got = getHarness(t, s, "")
	byPath = map[string]harnessFileView{}
	for _, f := range got.Files {
		byPath[f.Path] = f
	}
	if byPath[mainPath].Tasks[0].ImportedID == "" {
		t.Fatal("main import not marked")
	}
	if byPath[otherPath].Tasks[0].ImportedID == "" {
		t.Fatal("other import not marked")
	}
	if byPath[mainPath].Tasks[0].ImportedID == byPath[otherPath].Tasks[0].ImportedID {
		t.Fatal("distinct (path,id) imports must be distinct tasks")
	}
}

// A v2 import has source_id but no source_path; importing the same (path,id)
// migrates the record instead of creating a duplicate.
func TestHarnessLegacyImportMigratesSourcePath(t *testing.T) {
	s, _ := scheduleServer(t)
	path := writeHarnessFile(t)
	legacy, err := s.scheduleStore().Create(&schedule.Task{
		Name: "老导入", Cron: "0 10 24 11 *", Prompt: "M145 稳定性检查",
		TargetSession: "main", Source: "harness", SourceID: "h-once",
	})
	if err != nil {
		t.Fatal(err)
	}
	w := importHarness(t, s, map[string]any{"path": path, "id": "h-once"})
	if w.Code != http.StatusConflict {
		t.Fatalf("legacy duplicate: %d %s", w.Code, w.Body.String())
	}
	got := s.scheduleStore().Get(legacy.ID)
	if got == nil || got.SourcePath != path {
		t.Fatalf("legacy source_path not migrated: %+v", got)
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

	w = scheduleCall(t, s, http.MethodPost, "/api/schedules/"+id+"/enable", id, map[string]any{"enabled": true}, s.handleEnableSchedule)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}

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

	w = scheduleCall(t, s, http.MethodPost, "/api/schedules/parse", "", map[string]any{"text": "每天9点和18点巡检"}, s.handleParseSchedule)
	if w.Code != http.StatusOK {
		t.Fatalf("ambiguous parse: %d %s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Candidates) != 2 {
		t.Fatalf("ambiguous candidates = %+v", out.Candidates)
	}
	for _, text := range []string{"9点巡检", "每天巡检"} {
		w = scheduleCall(t, s, http.MethodPost, "/api/schedules/parse", "", map[string]any{"text": text}, s.handleParseSchedule)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%q: code=%d body=%s", text, w.Code, w.Body.String())
		}
	}
}
