package claudehist

import (
	"os"
	"path/filepath"
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
