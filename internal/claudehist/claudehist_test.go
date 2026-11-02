package claudehist

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeHist writes the given raw jsonl lines (one per string) to a fake
// projects dir under t.TempDir() and returns the (cwd, sessionID) to address it.
func writeHist(t *testing.T, lines []string) (string, string) {
	t.Helper()
	cwd := filepath.Join(t.TempDir(), "proj")
	sid := "s1"
	dir := HistoryDir(cwd)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := HistoryPath(cwd, sid)
	raw := ""
	for _, l := range lines {
		raw += l + "\n"
	}
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return cwd, sid
}

func realUserLine(uuid, text string) string {
	return `{"type":"user","uuid":"` + uuid + `","message":{"role":"user","content":"` + text + `"}}`
}

func TestLastUserUUIDMatchesLastOccurrence(t *testing.T) {
	cwd, sid := writeHist(t, []string{
		realUserLine("u1", "hello"),
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":"hi"}}`,
		realUserLine("u2", "hello"), // duplicate text, later uuid
	})
	got := LastUserUUID(cwd, sid, "hello")
	if got != "u2" {
		t.Fatalf("LastUserUUID = %q, want u2 (last matching)", got)
	}
}

func TestLastUserUUIDIgnoresToolResultAndSystem(t *testing.T) {
	cwd, sid := writeHist(t, []string{
		realUserLine("u1", "real prompt"),
		// tool_result echo: type=user but content is a tool_result block (no text)
		`{"type":"user","uuid":"tr1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"done"}]}}`,
		// system-injected xml-ish message
		`{"type":"user","uuid":"sys1","message":{"role":"user","content":"<task-notification>"} }`,
	})
	got := LastUserUUID(cwd, sid, "real prompt")
	if got != "u1" {
		t.Fatalf("LastUserUUID = %q, want u1", got)
	}
	// No match for text that only appears as system/tool_result.
	if u := LastUserUUID(cwd, sid, "<task-notification>"); u != "" {
		t.Fatalf("system message matched: %q", u)
	}
}

func TestLastUserUUIDEmptyForImageOnly(t *testing.T) {
	cwd, sid := writeHist(t, []string{
		realUserLine("u1", "text prompt"),
		// image-only user message (no text block)
		`{"type":"user","uuid":"img1","message":{"role":"user","content":[{"type":"image","source":{}}]}}`,
	})
	if u := LastUserUUID(cwd, sid, ""); u != "" {
		t.Fatalf("empty matchText should return empty, got %q", u)
	}
}

func TestTruncateBeforeUUIDKeepsPrefix(t *testing.T) {
	cwd, sid := writeHist(t, []string{
		`{"type":"queue-operation","operation":"enqueue"}`,
		realUserLine("u1", "first"),
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":"r1"}}`,
		realUserLine("u2", "second"), // target
		`{"type":"assistant","uuid":"a2","message":{"role":"assistant","content":"r2"}}`,
	})
	removed, err := TruncateBeforeUUID(cwd, sid, "u2")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 { // u2 line + a2 line
		t.Fatalf("removed = %d, want 2", removed)
	}
	raw, _ := os.ReadFile(HistoryPath(cwd, sid))
	want := `{"type":"queue-operation","operation":"enqueue"}` + "\n" +
		realUserLine("u1", "first") + "\n" +
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":"r1"}}` + "\n"
	if string(raw) != want {
		t.Fatalf("truncated jsonl =\n%s\nwant\n%s", raw, want)
	}
	if !HasUserPrompt(cwd, sid) {
		t.Fatal("HasUserPrompt = false after keeping u1, want true")
	}
}

func TestTruncateBeforeUUIDFirstMessageEmptiesPrompt(t *testing.T) {
	cwd, sid := writeHist(t, []string{
		`{"type":"queue-operation","operation":"enqueue"}`,
		realUserLine("u1", "only"), // target = first user message
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":"r1"}}`,
	})
	if _, err := TruncateBeforeUUID(cwd, sid, "u1"); err != nil {
		t.Fatal(err)
	}
	if HasUserPrompt(cwd, sid) {
		t.Fatal("HasUserPrompt = true after removing the only user prompt, want false (so caller clears sid)")
	}
}

func TestTruncateBeforeUUIDNotFound(t *testing.T) {
	cwd, sid := writeHist(t, []string{realUserLine("u1", "x")})
	_, err := TruncateBeforeUUID(cwd, sid, "missing")
	if err == nil {
		t.Fatal("expected error for missing uuid")
	}
	// File must be untouched on not-found.
	raw, _ := os.ReadFile(HistoryPath(cwd, sid))
	if len(raw) == 0 {
		t.Fatal("jsonl was emptied despite uuid not found")
	}
}

// peerLine mimics a real inbound peer-message transcript entry (isMeta user
// line with origin.kind="peer").
func peerLine(uuid, name, body string) string {
	return `{"type":"user","isMeta":true,"uuid":"` + uuid + `",` +
		`"origin":{"kind":"peer","name":"` + name + `","body":"` + body + `","from":"uds:/run/user/1000/cc-socks/1.sock"},` +
		`"message":{"role":"user","content":"Another Claude session sent a message:\n<cross-session-message from-name=\"` + name + `\">\n` + body + `\n</cross-session-message>"}}`
}

func TestConvertPeerMessagePassThrough(t *testing.T) {
	cwd, sid := writeHist(t, []string{
		realUserLine("u1", "hello"),
		peerLine("p1", "test1", "问下几点了"),
		`{"type":"user","isMeta":true,"uuid":"m1","message":{"role":"user","content":"<local-command-caveat>x</local-command-caveat>"}}`, // 其它 isMeta 照旧跳过
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":"r1"}}`,
	})
	events, err := Convert(cwd, sid)
	if err != nil {
		t.Fatal(err)
	}
	var peerFound bool
	for _, ev := range events {
		if ev.Kind != "stream" {
			continue
		}
		var d struct {
			Type   string `json:"type"`
			Origin *struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
				Body string `json:"body"`
			} `json:"origin"`
		}
		if json.Unmarshal(ev.Data, &d) != nil || d.Origin == nil {
			continue
		}
		if d.Type == "user" && d.Origin.Kind == "peer" {
			peerFound = true
			if d.Origin.Name != "test1" || d.Origin.Body != "问下几点了" {
				t.Fatalf("peer origin = %+v, want name=test1 body=问下几点了", d.Origin)
			}
		}
	}
	if !peerFound {
		t.Fatal("peer message missing from Convert output")
	}
	for _, ev := range events {
		if ev.Kind == "stream" && strings.Contains(string(ev.Data), "local-command-caveat") {
			t.Fatal("non-peer isMeta line leaked into Convert output")
		}
	}
}

func TestPeerMessageIsNotARealUserPrompt(t *testing.T) {
	cwd, sid := writeHist(t, []string{peerLine("p1", "test1", "hi")})
	if HasUserPrompt(cwd, sid) {
		t.Fatal("peer isMeta line counted as a user prompt (would break rewind sid clearing)")
	}
	if u := LastUserUUID(cwd, sid, "hi"); u != "" {
		t.Fatalf("peer line matched LastUserUUID: %q", u)
	}
}

// queuedAttachmentLine mimics a peer message absorbed mid-turn: the CLI folds
// it into the running turn and writes an attachment line (type=attachment,
// attachment.type=queued_command) carrying the same peer origin instead of a
// user line. Prompt is the wrapped cross-session text; origin.body is decoded.
func queuedAttachmentLine(uuid, name, body string) string {
	return `{"type":"attachment","uuid":"` + uuid + `","timestamp":"2026-10-31T17:49:56.324Z",` +
		`"attachment":{"type":"queued_command","commandMode":"prompt","isMeta":true,` +
		`"prompt":"<cross-session-message from=\"uds:/run/user/1000/cc-socks/1.sock\" from-name=\"` + name + `\">\n` + body + `\n</cross-session-message>",` +
		`"origin":{"kind":"peer","name":"` + name + `","body":"` + body + `","from":"uds:/run/user/1000/cc-socks/1.sock"}}}`
}

func TestConvertQueuedPeerAttachment(t *testing.T) {
	cwd, sid := writeHist(t, []string{
		realUserLine("u1", "hello"),
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":"r1"}}`,
		// 忙时并入的 peer 消息（attachment 行）
		queuedAttachmentLine("q1", "worker-1", "【补充】再查一下构建"),
		// 非 peer 的 queued_command（后台任务通知）必须继续跳过
		`{"type":"attachment","uuid":"q2","attachment":{"type":"queued_command","commandMode":"task-notification","prompt":"<task-notification>x</task-notification>"}}`,
	})
	events, err := Convert(cwd, sid)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range events {
		if ev.Kind != "stream" {
			continue
		}
		var d struct {
			Type   string `json:"type"`
			UUID   string `json:"uuid"`
			Origin *struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
				Body string `json:"body"`
			} `json:"origin"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(ev.Data, &d) != nil || d.Origin == nil {
			continue
		}
		if d.Type != "user" || d.Origin.Kind != "peer" {
			t.Fatalf("unexpected peer-ish event: %s", ev.Data)
		}
		found = true
		if d.UUID != "q1" || d.Origin.Name != "worker-1" || d.Origin.Body != "【补充】再查一下构建" {
			t.Fatalf("queued peer event = %s", ev.Data)
		}
		if !strings.Contains(d.Message.Content, "<cross-session-message") {
			t.Fatalf("queued peer message content lost the prompt wrapper: %q", d.Message.Content)
		}
	}
	if !found {
		t.Fatal("mid-turn peer attachment missing from Convert output")
	}
	for _, ev := range events {
		if ev.Kind == "stream" && strings.Contains(string(ev.Data), "task-notification") {
			t.Fatal("non-peer queued_command leaked into Convert output")
		}
	}
}

func TestQueuedPeerAttachmentIsNotARealUserPrompt(t *testing.T) {
	cwd, sid := writeHist(t, []string{queuedAttachmentLine("q1", "worker-1", "hi")})
	if HasUserPrompt(cwd, sid) {
		t.Fatal("queued peer attachment counted as a user prompt (would break rewind sid clearing)")
	}
	if u := LastUserUUID(cwd, sid, "hi"); u != "" {
		t.Fatalf("queued peer attachment matched LastUserUUID: %q", u)
	}
}

// PeerAttachmentEvent is the live-tail entry point: it parses one raw
// transcript line (the resident host feeds it newly appended lines).
func TestPeerAttachmentEvent(t *testing.T) {
	ev, ok := PeerAttachmentEvent([]byte(queuedAttachmentLine("q1", "worker-1", "补充")))
	if !ok {
		t.Fatal("peer attachment line not recognized")
	}
	if ev.Kind != "stream" {
		t.Fatalf("kind = %q", ev.Kind)
	}
	var d struct {
		Type   string `json:"type"`
		UUID   string `json:"uuid"`
		Origin *struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
			Body string `json:"body"`
		} `json:"origin"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Type != "user" || d.UUID != "q1" || d.Origin == nil ||
		d.Origin.Kind != "peer" || d.Origin.Name != "worker-1" || d.Origin.Body != "补充" {
		t.Fatalf("event = %s", ev.Data)
	}
}

func TestPeerAttachmentEventIgnoresOtherLines(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"user","origin":{"kind":"peer","name":"p","body":"idle turn"}}`),
		[]byte(`{"type":"attachment","attachment":{"type":"queued_command","commandMode":"task-notification","prompt":"x"}}`),
		[]byte(`{"type":"attachment","attachment":{"type":"queued_command","origin":{"kind":"human"},"prompt":"x"}}`),
		[]byte(`{"type":"assistant","message":{"role":"assistant","content":"x"}}`),
		[]byte(`not json`),
	}
	for _, line := range lines {
		if ev, ok := PeerAttachmentEvent(line); ok {
			t.Fatalf("line %q unexpectedly converted to %s", line, ev.Data)
		}
	}
}

// An idle-turn inbound peer message is a plain user line with origin=peer; the
// resident host recovers it from the transcript because stdout carries no
// frame for it.
func TestPeerMessageEventConvertsIdleTurn(t *testing.T) {
	line := []byte(`{"type":"user","isMeta":true,"uuid":"p0","timestamp":"2026-11-02T06:33:38.492Z",` +
		`"origin":{"kind":"peer","name":"idle-peer","body":"idle body","from":"uds:/run/user/1000/cc-socks/1.sock"},` +
		`"message":{"role":"user","content":"idle body"}}`)
	ev, ok := PeerMessageEvent(line)
	if !ok {
		t.Fatal("idle peer message must convert")
	}
	var d struct {
		Type   string `json:"type"`
		UUID   string `json:"uuid"`
		Origin *struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
			Body string `json:"body"`
		} `json:"origin"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Type != "user" || d.UUID != "p0" || d.Origin == nil ||
		d.Origin.Kind != "peer" || d.Origin.Name != "idle-peer" || d.Origin.Body != "idle body" {
		t.Fatalf("event = %s", ev.Data)
	}
	if !ev.Ts.Equal(time.Date(2026, 11, 2, 6, 33, 38, 492000000, time.UTC)) {
		t.Fatalf("ts = %s", ev.Ts)
	}
}

func TestPeerMessageEventIgnoresOtherLines(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"hello"}}`),
		[]byte(`{"type":"user","isMeta":true,"uuid":"m1","message":{"role":"user","content":"x"}}`),
		[]byte(peerAttachmentLineForTest("q1", "worker-1", "fold in")),
		[]byte(`{"type":"attachment","attachment":{"type":"queued_command","origin":{"kind":"peer"},"prompt":"x"}}`),
		[]byte(`{"type":"assistant","message":{"role":"assistant","content":"x"}}`),
		[]byte(`not json`),
	}
	for _, line := range lines {
		if ev, ok := PeerMessageEvent(line); ok {
			t.Fatalf("line %q unexpectedly converted to %s", line, ev.Data)
		}
	}
}

func peerAttachmentLineForTest(uuid, name, body string) string {
	return `{"type":"attachment","uuid":"` + uuid + `","attachment":{"type":"queued_command","commandMode":"prompt",` +
		`"prompt":"x","origin":{"kind":"peer","name":"` + name + `","body":"` + body + `"}}}`
}

func TestRenameSessionAppendsCustomTitle(t *testing.T) {
	cwd, sid := writeHist(t, []string{realUserLine("u1", "hello")})
	if err := RenameSession(cwd, sid, "  worker-1  "); err != nil {
		t.Fatal(err)
	}
	if err := RenameSession(cwd, sid, "worker-2"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(HistoryPath(cwd, sid))
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	last := lines[len(lines)-1]
	var entry struct {
		Type        string `json:"type"`
		CustomTitle string `json:"customTitle"`
		SessionID   string `json:"sessionId"`
	}
	if err := json.Unmarshal([]byte(last), &entry); err != nil {
		t.Fatalf("last line not JSON: %v", err)
	}
	if entry.Type != "custom-title" || entry.CustomTitle != "worker-2" || entry.SessionID != sid {
		t.Fatalf("last entry = %+v, want custom-title/worker-2/%s", entry, sid)
	}
	if err := RenameSession(cwd, sid, "   "); err == nil {
		t.Fatal("empty title should error")
	}
}

// writeRegistry writes one registry entry file into a fake ~/.claude/sessions
// dir (HOME pointed at root) and returns the root.
func writeRegistry(t *testing.T, root string, pid int, sessionID, name string) {
	t.Helper()
	dir := filepath.Join(root, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"name":%q,"peerProtocol":1}`, pid, sessionID, name)
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", pid)), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMoveSessionRelocatesTranscript(t *testing.T) {
	root := t.TempDir()
	cwdFrom := filepath.Join(root, "old-proj")
	cwdTo := filepath.Join(root, "new-proj")
	sid := "s1"
	fromDir := HistoryDir(cwdFrom)
	if err := os.MkdirAll(filepath.Join(fromDir, sid, "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HistoryPath(cwdFrom, sid), []byte(realUserLine("u1", "hi")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fromDir, sid, "subagents", "agent-a.jsonl"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	moved, err := MoveSession(cwdFrom, cwdTo, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !moved {
		t.Fatal("expected moved=true")
	}
	if !pathExists(HistoryPath(cwdTo, sid)) {
		t.Fatal("main jsonl not relocated to destination")
	}
	if !pathIsDir(filepath.Join(HistoryDir(cwdTo), sid, "subagents")) {
		t.Fatal("subagents dir not relocated to destination")
	}
	if pathExists(HistoryPath(cwdFrom, sid)) || pathIsDir(filepath.Join(fromDir, sid)) {
		t.Fatal("source transcript still present after move")
	}
	// The relocated transcript is addressable via the new cwd.
	if events, err := Convert(cwdTo, sid); err != nil || len(events) == 0 {
		t.Fatalf("Convert(new cwd) = %d events, err=%v; want >0", len(events), err)
	}

	// No-op when the source has nothing on disk.
	if moved, err := MoveSession(cwdFrom, cwdTo, "missing"); err != nil || moved {
		t.Fatalf("expected no-op for missing session, moved=%v err=%v", moved, err)
	}
}

func TestMoveSessionRefusesExistingDestination(t *testing.T) {
	root := t.TempDir()
	cwdFrom := filepath.Join(root, "old-proj")
	cwdTo := filepath.Join(root, "new-proj")
	if err := os.MkdirAll(HistoryDir(cwdFrom), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(HistoryDir(cwdTo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HistoryPath(cwdFrom, "s1"), []byte("z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HistoryPath(cwdTo, "s1"), []byte("existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MoveSession(cwdFrom, cwdTo, "s1"); err == nil {
		t.Fatal("expected error when destination already holds the session")
	}
	if raw, _ := os.ReadFile(HistoryPath(cwdFrom, "s1")); string(raw) != "z\n" {
		t.Fatal("source jsonl modified despite destination conflict")
	}
}

func TestReadRegistryForSessionFindsLauncherChild(t *testing.T) {
	// Launcher-style CLI (ducc): the real claude is a child process, so the
	// registry file carries the child pid, not the pid the host spawned.
	root := t.TempDir()
	t.Setenv("HOME", root)
	writeRegistry(t, root, 2000, "fcdd656c-e1c3-4b45-82bd-cb226ec42b51", "work-summary-agent")
	writeRegistry(t, root, 999, "other-session", "unrelated")

	if e := ReadRegistryEntry(1000); e != nil {
		t.Fatalf("pid lookup unexpectedly found %+v", e)
	}
	e := ReadRegistryForSession("fcdd656c-e1c3-4b45-82bd-cb226ec42b51")
	if e == nil || e.Pid != 2000 || e.Name != "work-summary-agent" {
		t.Fatalf("session lookup = %+v, want pid=2000 name=work-summary-agent", e)
	}
	if e := ReadRegistryForSession("missing-session"); e != nil {
		t.Fatalf("session lookup for unknown id = %+v, want nil", e)
	}
	if e := ReadRegistryForSession(""); e != nil {
		t.Fatalf("session lookup for empty id = %+v, want nil", e)
	}
}
