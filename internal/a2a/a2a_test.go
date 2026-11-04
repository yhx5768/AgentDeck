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

func TestParseToleratesSingleQuotes(t *testing.T) {
	text := "preamble\n<cross-session-message from='worker-1'>\nhello\n</cross-session-message>\n"
	from, body, ok := Parse(text)
	if !ok || from != "worker-1" || body != "hello" {
		t.Fatalf("got from=%q body=%q ok=%v", from, body, ok)
	}
}
