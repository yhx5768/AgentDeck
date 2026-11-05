package runner

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cc-webui/internal/config"
	"cc-webui/internal/store"
)

func notifierTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	cm := config.NewManager(filepath.Join(dir, "config.yaml"), &config.Config{DataDir: dir})
	return NewManager(cm, st)
}

func TestEmitStatusErrorNotifies(t *testing.T) {
	m := notifierTestManager(t)
	if err := m.st.Upsert(&store.Session{ID: "w1", Cwd: t.TempDir(), CLI: "claude", Provider: "gw"}, nil); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	m.SetNotifier(Notifier{TurnError: func(sid, msg string) {
		mu.Lock()
		got = append(got, sid+":"+msg)
		mu.Unlock()
	}})

	m.emit("w1", "status", map[string]string{"state": "error", "error": "boom"})
	// A map[string]any error (the unrecognized-model shape) must notify too.
	m.emit("w1", "status", map[string]any{"state": "error", "error": "bad model"})
	// Non-error statuses must not.
	m.emit("w1", "status", map[string]string{"state": "turn_end"})
	m.emit("w1", "status", map[string]string{"state": "interrupted"})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "w1:boom" || got[1] != "w1:bad model" {
		t.Fatalf("notifications = %v", got)
	}
}

func TestCreatePendingNotifies(t *testing.T) {
	m := notifierTestManager(t)
	var got *PermissionRequest
	m.SetNotifier(Notifier{Ask: func(sid string, pr *PermissionRequest) {
		if sid == "w1" {
			got = pr
		}
	}})
	pr := m.PermissionRequest("w1", "p1", "AskUserQuestion", map[string]any{"questions": []any{}}, "")
	if got == nil || got.ID != "p1" || pr.ID != "p1" {
		t.Fatalf("ask notifier did not receive the pending request: %v", got)
	}
}

func TestScanStallsLatchesOnce(t *testing.T) {
	m := notifierTestManager(t)
	var mu sync.Mutex
	var hits []string
	m.SetNotifier(Notifier{Stalled: func(sid string, silent time.Duration) {
		mu.Lock()
		hits = append(hits, sid)
		mu.Unlock()
	}})

	r := m.rt("w1")
	r.mu.Lock()
	r.working = true
	r.cancel = func() {}
	r.lastActivity = time.Now().Add(-time.Hour)
	r.mu.Unlock()

	if got := m.scanStalls(time.Now()); len(got) != 1 || got[0].id != "w1" {
		t.Fatalf("first scan hits = %+v", got)
	}
	if got := m.scanStalls(time.Now()); len(got) != 0 {
		t.Fatalf("stall must latch (one notification per turn): %+v", got)
	}
	// A turn that is not running is never a stall.
	r.mu.Lock()
	r.cancel = nil
	r.stallNotified = false
	r.lastActivity = time.Now().Add(-time.Hour)
	r.mu.Unlock()
	if got := m.scanStalls(time.Now()); len(got) != 0 {
		t.Fatalf("idle runtime must not stall: %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 1 {
		t.Fatalf("notifier hits = %v", hits)
	}
}
