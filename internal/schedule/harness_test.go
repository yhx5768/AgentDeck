package schedule

import (
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
