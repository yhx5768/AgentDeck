package schedule

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseHarness(t *testing.T) {
	raw := `{"tasks":[
	  {"id":"a","cron":"0 10 24 11 *","prompt":"一次性检查","createdAt":1700000000123},
	  {"id":"b","cron":"7 9 * * 1-5","prompt":"每日巡检","recurring":true}
	]}`
	tasks, err := ParseHarness([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d", len(tasks))
	}
	if tasks[0].Recurring {
		t.Fatal("missing recurring must mean one-shot")
	}
	if !tasks[0].CreatedAt.Equal(time.UnixMilli(1700000000123)) {
		t.Fatalf("createdAt = %v", tasks[0].CreatedAt)
	}
	if !tasks[1].Recurring {
		t.Fatal("recurring flag lost")
	}

	// A bare array is accepted too.
	arr, err := ParseHarness([]byte(`[{"id":"c","cron":"* * * * *","prompt":"p"}]`))
	if err != nil || len(arr) != 1 || arr[0].ID != "c" {
		t.Fatalf("array parse = %+v, %v", arr, err)
	}
	if _, err := ParseHarness([]byte(`{not json`)); err == nil {
		t.Fatal("invalid JSON should error")
	}
}

func TestHarnessName(t *testing.T) {
	cases := []struct {
		prompt string
		id     string
		want   string
	}{
		{"定时检查 M145 稳定性实验的网络数据。步骤：1) 读取看板卡", "h1", "定时检查 M145 稳定性实验的网络数据"},
		{"每日看板/worker 晨间巡检（只读，不自动改状态）。请：", "h2", "每日看板/worker 晨间巡检"},
		{"M145 稳定性检查\n第二步", "h3", "M145 稳定性检查"},
		{"", "h4", "Harness 任务 h4"},
	}
	for _, tc := range cases {
		if got := HarnessName(HarnessTask{ID: tc.id, Prompt: tc.prompt}); got != tc.want {
			t.Errorf("HarnessName(%q) = %q, want %q", tc.prompt, got, tc.want)
		}
	}
	long := ""
	for i := 0; i < 80; i++ {
		long += "长"
	}
	if got := []rune(HarnessName(HarnessTask{Prompt: long})); len(got) != 40 {
		t.Fatalf("long name len = %d", len(got))
	}
}

func TestParseHarnessEmpty(t *testing.T) {
	tasks, err := ParseHarness([]byte(`{"tasks":[]}`))
	if err != nil || len(tasks) != 0 {
		t.Fatalf("empty harness = %+v, %v", tasks, err)
	}
	if _, err := ParseHarness([]byte(`{"tasks":[]`)); !errors.Is(err, ErrHarnessInvalidJSON) {
		t.Fatalf("malformed JSON error = %v", err)
	}
}

func TestWorkspaceRoot(t *testing.T) {
	cases := map[string]string{
		"/a/b/.claude/scheduled_tasks.json":    "/a/b",
		"/a/b/scheduled_tasks.json":            "/a/b",
		"/home/u/.claude/scheduled_tasks.json": "/home/u",
	}
	for in, want := range cases {
		if got := WorkspaceRoot(in); got != want {
			t.Errorf("WorkspaceRoot(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiscoverHarnessGroups(t *testing.T) {
	ws1, ws2 := t.TempDir(), t.TempDir()
	p1 := filepath.Join(ws1, ".claude", HarnessFileName)
	p2 := filepath.Join(ws2, ".claude", HarnessFileName)
	for _, p := range []string{p1, p2} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(`{"tasks":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CC_WEBUI_HARNESS_TASKS", p1)
	groups := DiscoverHarnessGroups([]string{ws1, ws2}, nil)
	byWs := map[string][]string{}
	for _, g := range groups {
		byWs[g.Workspace] = append(byWs[g.Workspace], g.Paths...)
	}
	if got := byWs[ws1]; len(got) != 1 || got[0] != p1 {
		t.Fatalf("ws1 group = %v", got)
	}
	if got := byWs[ws2]; len(got) != 1 || got[0] != p2 {
		t.Fatalf("ws2 group = %v", got)
	}
	// Duplicates (env + session cwd) collapse to one path.
	total := 0
	for _, ps := range byWs {
		total += len(ps)
	}
	if total != 2 {
		t.Fatalf("discovered %d paths, want 2: %+v", total, groups)
	}
}

func TestReadHarnessFileMissing(t *testing.T) {
	_, err := ReadHarnessFile(filepath.Join(t.TempDir(), HarnessFileName))
	if !errors.Is(err, ErrHarnessNotExist) {
		t.Fatalf("missing file error = %v", err)
	}
}

func TestReadHarnessFileRestrictsBasename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, HarnessFileName)
	if err := os.WriteFile(path, []byte(`{"tasks":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHarnessFile(path); err != nil {
		t.Fatalf("read valid file: %v", err)
	}
	other := filepath.Join(dir, "other.json")
	if err := os.WriteFile(other, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHarnessFile(other); err == nil {
		t.Fatal("other basename must be rejected")
	}
}

func TestDiscoverHarnessPathsEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, HarnessFileName)
	if err := os.WriteFile(path, []byte(`{"tasks":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CC_WEBUI_HARNESS_TASKS", path)
	got := DiscoverHarnessPaths(nil)
	found := false
	for _, p := range got {
		if p == path {
			found = true
		}
	}
	if !found {
		t.Fatalf("env path not discovered: %v", got)
	}
}
