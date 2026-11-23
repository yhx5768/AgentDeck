package schedule

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) *Cron {
	t.Helper()
	c, err := ParseCron(expr)
	if err != nil {
		t.Fatalf("ParseCron(%q): %v", expr, err)
	}
	return c
}

func at(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.Local)
}

func TestParseCronValid(t *testing.T) {
	for _, expr := range []string{
		"* * * * *",
		"0 9 * * *",
		"*/15 9-18 * * 1-5",
		"0 0 29 2 *",
		"5/10 * * * *",
		"0 0 * * 7",
		"0,30 8,20 1-7 */2 *",
		"  0   0   *   *   * ",
	} {
		if _, err := ParseCron(expr); err != nil {
			t.Errorf("ParseCron(%q) = %v, want ok", expr, err)
		}
	}
	if got := mustParse(t, "  0   0   *   *   * ").Expr(); got != "0 0 * * *" {
		t.Fatalf("normalized expr = %q", got)
	}
}

func TestParseCronInvalid(t *testing.T) {
	for _, expr := range []string{
		"",
		"* * * *",
		"* * * * * *",
		"60 * * * *",
		"* 24 * * *",
		"* * 0 * *",
		"* * 32 * *",
		"* * * 0 *",
		"* * * 13 *",
		"* * * * 8",
		"*/0 * * * *",
		"5-1 * * * *",
		"a * * * *",
		"1,,2 * * * *",
	} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("ParseCron(%q) = nil error, want error", expr)
		}
	}
	if _, ok := mustParse(t, "0 0 30 2 *").Next(at(2024, 1, 1, 0, 0)); ok {
		t.Fatal("Feb 30 must never fire")
	}
}

func TestCronNext(t *testing.T) {
	cases := []struct {
		expr string
		from time.Time
		want time.Time
	}{
		{"*/15 * * * *", at(2024, 1, 1, 10, 7), at(2024, 1, 1, 10, 15)},
		{"0 9 * * *", at(2024, 1, 1, 9, 0), at(2024, 1, 2, 9, 0)}, // strictly after
		{"0 9 * * 1-5", at(2024, 1, 6, 10, 0), at(2024, 1, 8, 9, 0)},
		// DOM and DOW both restricted -> either matches. Jan 5 2024 is a Friday.
		{"0 0 13 * 5", at(2024, 1, 1, 0, 0), at(2024, 1, 5, 0, 0)},
		{"0 0 13 * *", at(2024, 1, 1, 0, 0), at(2024, 1, 13, 0, 0)},
		// 7 is Sunday.
		{"0 0 * * 7", at(2024, 1, 1, 0, 0), at(2024, 1, 7, 0, 0)},
		// Leap day: 2024-02-29 is already past when starting in March 2024.
		{"0 0 29 2 *", at(2024, 3, 1, 0, 0), at(2028, 2, 29, 0, 0)},
	}
	for _, tc := range cases {
		got, ok := mustParse(t, tc.expr).Next(tc.from)
		if !ok {
			t.Errorf("%q: no next after %v", tc.expr, tc.from)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("%q: next = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestStoreCRUDAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(st.List()) != 0 {
		t.Fatal("new store should be empty")
	}
	next := at(2024, 1, 2, 9, 0)
	task, err := st.Create(&Task{
		Name: "每日站会", Cron: "0 9 * * *", Prompt: "总结昨天", TargetSession: "main",
		Enabled: true, NextRunAt: &next,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if task.ID == "" || task.CreatedAt.IsZero() {
		t.Fatalf("Create did not assign id/timestamps: %+v", task)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o, want 600", info.Mode().Perm())
	}

	// A second store sees the persisted task.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := st2.Get(task.ID)
	if got == nil || got.Name != "每日站会" || got.NextRunAt == nil || !got.NextRunAt.Equal(next) {
		t.Fatalf("reopened = %+v", got)
	}

	updated, err := st2.Update(task.ID, func(t *Task) { t.Enabled = false; t.NextRunAt = nil })
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Enabled || updated.NextRunAt != nil {
		t.Fatalf("update not applied: %+v", updated)
	}

	runAt := at(2024, 1, 2, 9, 1)
	finished, err := st2.FinishRun(task.ID, "ok", runAt)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if finished.LastStatus != "ok" || finished.LastRunAt == nil || !finished.LastRunAt.Equal(runAt) {
		t.Fatalf("finish not applied: %+v", finished)
	}

	if err := st2.Delete(task.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := st2.Get(task.ID); got != nil {
		t.Fatalf("task still present: %+v", got)
	}
	if err := st2.Delete(task.ID); err == nil {
		t.Fatal("deleting a missing task should error")
	}
}

func TestClaimDueAdvancesAndDeduplicates(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "schedules.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := at(2024, 1, 1, 10, 7)
	dueAt := at(2024, 1, 1, 10, 0)
	task, err := st.Create(&Task{
		Name: "每 15 分钟", Cron: "*/15 * * * *", Prompt: "ping", TargetSession: "main",
		Enabled: true, NextRunAt: &dueAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(&Task{
		Name: "停用的", Cron: "*/15 * * * *", Prompt: "no", TargetSession: "main",
		Enabled: false, NextRunAt: &dueAt,
	}); err != nil {
		t.Fatal(err)
	}

	claimed := st.ClaimDue(now)
	if len(claimed) != 1 || claimed[0].ID != task.ID {
		t.Fatalf("claimed = %+v, want only the enabled task", claimed)
	}
	wantNext := at(2024, 1, 1, 10, 15)
	if claimed[0].NextRunAt == nil || !claimed[0].NextRunAt.Equal(wantNext) {
		t.Fatalf("claim advanced to %v, want %v", claimed[0].NextRunAt, wantNext)
	}
	if again := st.ClaimDue(now); len(again) != 0 {
		t.Fatalf("second claim returned %d tasks (would double-fire)", len(again))
	}
	// The advancement was persisted before delivery.
	reopened, err := Open(filepath.Join(filepath.Dir(st.path), "schedules.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get(task.ID); got == nil || got.NextRunAt == nil || !got.NextRunAt.Equal(wantNext) {
		t.Fatalf("persisted claim = %+v", got)
	}
}

func TestOpenCorruptFileMovesAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedules.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err == nil {
		t.Fatal("Open should report the parse error")
	}
	if st == nil || len(st.List()) != 0 {
		t.Fatalf("store should still be usable: %+v", st)
	}
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("corrupt backup not created: %v", matches)
	}
}
