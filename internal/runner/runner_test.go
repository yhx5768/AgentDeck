package runner

import (
	"strings"
	"testing"

	"cc-webui/internal/store"
)

// TestBusy pins the semantics the SSE replay repair depends on: Busy is true
// while a turn is in flight (cancel handle installed — including the rewind
// sentinel) or while messages are queued for a worker, and false only when
// the session runtime is truly idle.
func TestBusy(t *testing.T) {
	// NewManager stores cm/st but none of the exercised paths touch them.
	m := NewManager(nil, nil)

	if m.Busy("fresh") {
		t.Fatal("fresh runtime must report idle")
	}

	// Live one-shot / resident turn: cancel handle installed by setRunning.
	m.setRunning("running", func() {})
	if !m.Busy("running") {
		t.Fatal("turn in flight (cancel set) must report busy")
	}
	m.setIdle("running", "")
	if m.Busy("running") {
		t.Fatal("turn ended must report idle")
	}

	// Rewind holds the session via a cancel sentinel: also busy, so the
	// replay repair must not close the log while a rewind is in progress.
	if !m.LockForRewind("rewinding") {
		t.Fatal("idle session must accept the rewind lock")
	}
	if !m.Busy("rewinding") {
		t.Fatal("rewind lock must report busy")
	}
	m.UnlockFromRewind("rewinding")
	if m.Busy("rewinding") {
		t.Fatal("rewind unlock must report idle")
	}

	// Queue backlog / worker draining: covers the window between a queued
	// message and its turn_start, where cancel is not yet installed.
	r := m.rt("queued")
	r.mu.Lock()
	r.working = true
	r.queue = append(r.queue, Message{Text: "hi"})
	r.mu.Unlock()
	if !m.Busy("queued") {
		t.Fatal("queued message must report busy")
	}
}

// TestSetIdleUnread pins the server-side unread marker: a turn that ended
// (clean or error) marks the session unread so every client sees a blue dot
// until one opens it; an interrupt produced no fresh result and must not mark.
// This is the path the old localStorage blue dot duplicated client-side; moving
// it server-side is what makes the dot agree across tabs.
func TestSetIdleUnread(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	const sid = "s1"
	if err := st.Upsert(&store.Session{ID: sid, Name: "t"}, func(*store.Session) {}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	m := NewManager(nil, st)

	m.setIdle(sid, "")
	if !st.Get(sid).Unread {
		t.Fatal("clean turn end must mark unread")
	}
	if err := st.SetUnread(sid, false); err != nil {
		t.Fatalf("clear unread: %v", err)
	}
	m.setIdle(sid, "interrupted")
	if st.Get(sid).Unread {
		t.Fatal("interrupt must not mark unread")
	}
	m.setIdle(sid, "boom")
	if !st.Get(sid).Unread {
		t.Fatal("error turn end must mark unread")
	}
}

func TestParseClaudeCodeError(t *testing.T) {
	cases := []struct {
		name       string
		stderr     string
		wantKind   string
		wantMsgHas string
	}{
		{
			name:       "unrecognized_model with model",
			stderr:     `[claude-code:unrecognized_model] {"model":"glm-5.3-flash[1m]","query_source":"sdk"}`,
			wantKind:   "unrecognized_model",
			wantMsgHas: "glm-5.3-flash[1m]",
		},
		{
			name:       "unrecognized_model with extra lines ignored",
			stderr:     "some log\n[claude-code:unrecognized_model] {\"model\":\"x\"}\nmore log",
			wantKind:   "unrecognized_model",
			wantMsgHas: "x",
		},
		{
			name:     "plain stderr not recognized",
			stderr:   "something went wrong",
			wantKind: "",
		},
		{
			name:     "other claude-code kind not handled",
			stderr:   `[claude-code:rate_limit] {"retry_after":10}`,
			wantKind: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseClaudeCodeError(c.stderr)
			if got.Kind != c.wantKind {
				t.Fatalf("Kind = %q, want %q", got.Kind, c.wantKind)
			}
			if c.wantMsgHas != "" && !strings.Contains(got.Message, c.wantMsgHas) {
				t.Fatalf("Message = %q, want it to contain %q", got.Message, c.wantMsgHas)
			}
		})
	}
}
