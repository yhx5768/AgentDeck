package a2a

import "testing"

func TestEnvelopeRoundTrip(t *testing.T) {
	body := "【回报】cc-webui-修SSE断线：已改完，go test ./... 全绿\n第二行"
	text := Envelope("cc-webui-修SSE断线", body)
	from, got, ok := Parse(text)
	if !ok {
		t.Fatalf("Parse(%q) did not recognize the envelope", text)
	}
	if from != "cc-webui-修SSE断线" {
		t.Fatalf("from = %q", from)
	}
	if got != body {
		t.Fatalf("body = %q, want %q", got, body)
	}
}

func TestParseRejectsPlainUserText(t *testing.T) {
	if _, _, ok := Parse("帮我改一下 login 的 500"); ok {
		t.Fatal("plain text must not parse as an envelope")
	}
	if _, _, ok := Parse(""); ok {
		t.Fatal("empty text must not parse as an envelope")
	}
}

func TestIsBoardTool(t *testing.T) {
	for _, name := range []string{"board_list", "board_get", "board_create", "board_update", "board_comment", "board_bind_worker"} {
		if !IsBoardTool(name) {
			t.Fatalf("IsBoardTool(%q) = false", name)
		}
	}
	// artifact_publish is available to every session (workers on remote
	// machines publish their own output), so it must NOT be main-agent-only.
	for _, name := range []string{"list_sessions", "send_message", "artifact_publish", "board", ""} {
		if IsBoardTool(name) {
			t.Fatalf("IsBoardTool(%q) = true", name)
		}
	}
}

func TestArtifactPublishToolAndLegacyReportFileSchema(t *testing.T) {
	found := false
	for _, tool := range Tools {
		if tool.Name != "artifact_publish" {
			continue
		}
		found = true
		props := tool.InputSchema["properties"].(map[string]any)
		for _, key := range []string{"task_id", "title", "kind", "filename", "content", "encoding", "mime", "source_session", "url", "round"} {
			if _, ok := props[key]; !ok {
				t.Fatalf("artifact_publish missing property %q", key)
			}
		}
	}
	if !found {
		t.Fatal("artifact_publish not in Tools")
	}
	for _, tool := range BoardTools {
		if tool.Name != "board_create" && tool.Name != "board_update" {
			continue
		}
		props := tool.InputSchema["properties"].(map[string]any)
		if _, ok := props["report_file"]; !ok {
			t.Fatalf("%s missing compatibility report_file property", tool.Name)
		}
	}
}

func TestParseToleratesSingleQuotes(t *testing.T) {
	text := "preamble\n<cross-session-message from='worker-1'>\nhello\n</cross-session-message>\n"
	from, body, ok := Parse(text)
	if !ok || from != "worker-1" || body != "hello" {
		t.Fatalf("got from=%q body=%q ok=%v", from, body, ok)
	}
}
