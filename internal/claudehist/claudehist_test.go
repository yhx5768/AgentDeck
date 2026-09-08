package claudehist

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
