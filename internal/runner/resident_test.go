package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cc-webui/internal/claudehist"
	"cc-webui/internal/store"
)

func peerAttachmentLine(uuid, name, body string) string {
	return `{"type":"attachment","uuid":"` + uuid + `","timestamp":"2026-10-31T17:49:56.324Z",` +
		`"attachment":{"type":"queued_command","commandMode":"prompt","isMeta":true,` +
		`"prompt":"<cross-session-message from-name=\"` + name + `\">\n` + body + `\n</cross-session-message>",` +
		`"origin":{"kind":"peer","name":"` + name + `","body":"` + body + `","from":"uds:/run/user/1000/cc-socks/1.sock"}}}`
}

// tailHarness sets up a Manager with one subscriber and the transcript path.
func tailHarness(t *testing.T, cwd, sid string) (*Manager, <-chan *store.Event, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{st: st, procs: map[string]*sessionRuntime{}}
	ch, unsub := m.Subscribe("sess1")
	t.Cleanup(unsub)
	return m, ch, claudehist.HistoryPath(cwd, sid)
}

func appendFile(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func wantNoEvent(t *testing.T, ch <-chan *store.Event, d time.Duration) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event: %s", ev.Data)
	case <-time.After(d):
	}
}

func wantPeerEvent(t *testing.T, ch <-chan *store.Event, name, body string) {
	t.Helper()
	select {
	case ev := <-ch:
		var d struct {
			Type   string `json:"type"`
			Origin *struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
				Body string `json:"body"`
			} `json:"origin"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d.Type != "user" || d.Origin == nil || d.Origin.Kind != "peer" ||
			d.Origin.Name != name || d.Origin.Body != body {
			t.Fatalf("event = %s", ev.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for peer attachment event")
	}
}

// TestTailPeerAttachmentsEmitsOnlyPeerMessages verifies the tailer emits the
// inbound peer messages the CLI's stdout never carries — both the idle-turn
// user line and the mid-turn queued_command attachment — while ignoring the
// surrounding transcript lines (real user prompts, task notifications).
func TestTailPeerAttachmentsEmitsOnlyPeerMessages(t *testing.T) {
	cwd, sid := "/proj/tail-test", "s1"
	m, ch, path := tailHarness(t, cwd, sid)
	dead := make(chan struct{})
	defer close(dead)
	go m.tailPeerAttachments("sess1", dead, func() string { return sid }, cwd, sid, -1, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond) // let the tailer resolve sid/path and start at 0

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path,
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"hello"}}`+"\n"+
			`{"type":"attachment","uuid":"n1","attachment":{"type":"queued_command","commandMode":"task-notification","prompt":"<task-notification>x</task-notification>"}}`+"\n")
	wantNoEvent(t, ch, 100*time.Millisecond)

	// An idle-turn inbound peer message is a plain user line with origin=peer.
	appendFile(t, path,
		`{"type":"user","isMeta":true,"uuid":"p0","origin":{"kind":"peer","name":"idle-peer","body":"idle"},"message":{"role":"user","content":"x"}}`+"\n")
	wantPeerEvent(t, ch, "idle-peer", "idle")

	appendFile(t, path, peerAttachmentLine("q1", "worker-1", "【补充】再查一下构建")+"\n")
	wantPeerEvent(t, ch, "worker-1", "【补充】再查一下构建")
}

// TestTailPeerAttachmentsSkipsHistory covers a resident process restarting on
// an existing transcript: only lines appended after the tail starts may be
// emitted, otherwise the page would replay old bubbles on top of history.
func TestTailPeerAttachmentsSkipsHistory(t *testing.T) {
	cwd, sid := "/proj/tail-test", "s1"
	m, ch, path := tailHarness(t, cwd, sid)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, peerAttachmentLine("old", "old-peer", "old message")+"\n")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	dead := make(chan struct{})
	defer close(dead)
	// initialOffset = file size at spawn: the old line predates the process.
	go m.tailPeerAttachments("sess1", dead, func() string { return sid }, cwd, sid, fi.Size(), 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	wantNoEvent(t, ch, 100*time.Millisecond)

	appendFile(t, path, peerAttachmentLine("new", "new-peer", "new message")+"\n")
	wantPeerEvent(t, ch, "new-peer", "new message")
}

// TestTailPeerAttachmentsPartialLine makes sure a half-written transcript line
// is held until its newline lands.
func TestTailPeerAttachmentsPartialLine(t *testing.T) {
	cwd, sid := "/proj/tail-test", "s1"
	m, ch, path := tailHarness(t, cwd, sid)
	dead := make(chan struct{})
	defer close(dead)
	go m.tailPeerAttachments("sess1", dead, func() string { return sid }, cwd, sid, -1, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	line := peerAttachmentLine("q1", "worker-1", "partial")
	appendFile(t, path, line[:len(line)/2])
	wantNoEvent(t, ch, 100*time.Millisecond)
	appendFile(t, path, line[len(line)/2:]+"\n")
	wantPeerEvent(t, ch, "worker-1", "partial")
}

// TestTailPeerAttachmentsSkipsStdoutForwardedUUID covers a CLI build that does
// emit the inbound user frame on stdout: the stdout path records the uuid, so
// the tailer must not draw the same message twice.
func TestTailPeerAttachmentsSkipsStdoutForwardedUUID(t *testing.T) {
	cwd, sid := "/proj/tail-test", "s1"
	m, ch, path := tailHarness(t, cwd, sid)
	m.claimPeerSeen("sess1", "p0")
	dead := make(chan struct{})
	defer close(dead)
	go m.tailPeerAttachments("sess1", dead, func() string { return sid }, cwd, sid, -1, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path,
		`{"type":"user","isMeta":true,"uuid":"p0","origin":{"kind":"peer","name":"dup","body":"dup"},"message":{"role":"user","content":"x"}}`+"\n")
	wantNoEvent(t, ch, 150*time.Millisecond)
}

// TestPeerTurnClaimedFromContentFrame pins the fix for inbound peer turns the
// CLI never announces on stdout: the first content frame of an untracked turn
// must flip the session to running (sidebar dot / workspace badge), and the
// result frame must end it.
func TestPeerTurnClaimedFromContentFrame(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert(&store.Session{ID: "sess1", CLI: "claude", Provider: "p", Cwd: "/tmp", Resident: true}, nil); err != nil {
		t.Fatal(err)
	}
	m := &Manager{st: st, procs: map[string]*sessionRuntime{}}
	proc := &residentProc{
		dead:        make(chan struct{}),
		initSeen:    make(chan struct{}),
		idleCh:      make(chan struct{}),
		turnDone:    make(chan string, 1),
		pendingCtrl: map[string]chan map[string]any{},
	}
	if m.GetStatus("sess1").Running {
		t.Fatal("fresh resident session must report idle")
	}
	m.procEvent("sess1", proc, map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_start"}})
	if !m.GetStatus("sess1").Running {
		t.Fatal("first content frame of an untracked turn must report running")
	}
	// A user frame for the same turn must not double-claim (and must record
	// its uuid so the tailer skips it).
	m.procEvent("sess1", proc, map[string]any{"type": "user", "uuid": "p1", "origin": map[string]any{"kind": "peer"}})
	m.procEvent("sess1", proc, map[string]any{"type": "result", "subtype": "success"})
	if m.GetStatus("sess1").Running {
		t.Fatal("result must end the peer turn")
	}
}
