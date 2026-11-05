package server

import (
	"strings"
	"testing"

	"cc-webui/internal/store"
)

func TestReadSessionMessagesProjectsText(t *testing.T) {
	s, st := boardMCPServer(t)
	putSession(t, st, &store.Session{ID: "s1", Name: "s1", Cwd: t.TempDir(), CLI: "claude", Provider: "gw"})

	if _, err := st.AppendEvent("s1", "user", map[string]string{"text": "你好"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent("s1", "stream", map[string]any{
		"type": "assistant",
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": "SECRET_REASONING"},
			map[string]any{"type": "text", "text": "我来跑个命令"},
			map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "SECRET_CMD"}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	// The engine echo of the user prompt must not be duplicated, and a
	// tool_result must be dropped.
	if _, err := st.AppendEvent("s1", "stream", map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": "SECRET_OUTPUT"}}},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := s.handleReadSessionMessages("s1", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"你好", "我来跑个命令", "[tool: Bash]", "\"role\": \"user\"", "\"role\": \"assistant\""} {
		if !strings.Contains(out, want) {
			t.Fatalf("read output missing %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{"SECRET_REASONING", "SECRET_CMD", "SECRET_OUTPUT"} {
		if strings.Contains(out, leak) {
			t.Fatalf("read output must not leak %q:\n%s", leak, out)
		}
	}
}

func TestReadSessionMessagesPages(t *testing.T) {
	s, st := boardMCPServer(t)
	putSession(t, st, &store.Session{ID: "s1", Name: "s1", Cwd: t.TempDir(), CLI: "claude", Provider: "gw"})
	for _, text := range []string{"m1", "m2", "m3", "m4", "m5"} {
		if _, err := st.AppendEvent("s1", "user", map[string]string{"text": text}); err != nil {
			t.Fatal(err)
		}
	}
	last, err := s.handleReadSessionMessages("s1", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(last, "m5") || !strings.Contains(last, "m4") || strings.Contains(last, "m3") {
		t.Fatalf("last page = %s", last)
	}
	if !strings.Contains(last, "\"has_more\": true") {
		t.Fatalf("last page must report has_more: %s", last)
	}
	first, err := s.handleReadSessionMessages("s1", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "m2") || !strings.Contains(first, "m1") || strings.Contains(first, "m3") {
		t.Fatalf("earlier page = %s", first)
	}
	if !strings.Contains(first, "\"has_more\": false") {
		t.Fatalf("first page must not report has_more: %s", first)
	}
}

func TestSearchMessagesFindsHits(t *testing.T) {
	s, st := boardMCPServer(t)
	putSession(t, st, &store.Session{ID: "s1", Name: "s1", Cwd: t.TempDir(), CLI: "claude", Provider: "gw"})
	if _, err := st.AppendEvent("s1", "user", map[string]string{"text": "alpha needle beta"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent("s1", "user", map[string]string{"text": "nothing here"}); err != nil {
		t.Fatal(err)
	}
	out, err := s.handleSearchMessages("NEEDLE", "s1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "needle") || !strings.Contains(out, "\"role\": \"user\"") {
		t.Fatalf("search miss: %s", out)
	}
	if strings.Contains(out, "nothing here") {
		t.Fatalf("search returned a non-match: %s", out)
	}
	if _, err := s.handleSearchMessages("", "", 10); err == nil {
		t.Fatal("empty query must error")
	}
}

func TestSendMessageIdempotent(t *testing.T) {
	s, st := boardMCPServer(t)
	putSession(t, st, &store.Session{ID: "target", Name: "target", Cwd: t.TempDir(), CLI: "claude", Provider: "gw"})

	args := map[string]any{"to": "target", "message": "hello", "from": "main", "send_id": "abc"}
	if _, err := s.agentdeckToolCall("main", "send_message", args); err != nil {
		t.Fatal(err)
	}
	second, err := s.agentdeckToolCall("main", "send_message", map[string]any{
		"to": "target", "message": "hello", "from": "main", "send_id": "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second, "\"duplicate\":true") {
		t.Fatalf("second send with the same send_id must be a duplicate: %s", second)
	}
	waitIdle(t, s, "target")
	if n := countUserEvents(st, "target", func(map[string]string) bool { return true }); n != 1 {
		t.Fatalf("idempotent send must deliver once (got %d)", n)
	}

	// A different send_id delivers again.
	if _, err := s.agentdeckToolCall("main", "send_message", map[string]any{
		"to": "target", "message": "hello", "from": "main", "send_id": "def",
	}); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s, "target")
	if n := countUserEvents(st, "target", func(map[string]string) bool { return true }); n != 2 {
		t.Fatalf("a fresh send_id must deliver (got %d)", n)
	}
}
