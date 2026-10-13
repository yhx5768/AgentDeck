package opencodehist

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cc-webui/internal/store"
)

// fixture mirrors the real `opencode export` JSON shape (field names taken
// from a live export of opencode 1.18.x). Parts that must be skipped
// (patch / step-*) are present in realistic positions.
const fixture = `{
  "info": {
    "id": "ses_test123",
    "title": "test session",
    "directory": "/home/me/proj",
    "agent": "build",
    "model": {"id": "k3", "providerID": "kimi-for-coding", "variant": "default"},
    "version": "1.18.19",
    "time": {"created": 1787000000000, "updated": 1787000100000}
  },
  "messages": [
    {
      "info": {"role": "user", "id": "msg_u1", "time": {"created": 1787000000100}},
      "parts": [
        {"type": "text", "text": "\"hello\"", "id": "prt_u1", "sessionID": "ses_test123", "messageID": "msg_u1"}
      ]
    },
    {
      "info": {"role": "assistant", "id": "msg_a1", "time": {"created": 1787000000200}},
      "parts": [
        {"type": "step-start", "id": "prt_s1", "sessionID": "ses_test123", "messageID": "msg_a1", "snapshot": "abc"},
        {"type": "reasoning", "text": "let me think", "id": "prt_r1", "sessionID": "ses_test123", "messageID": "msg_a1"},
        {"type": "text", "text": "hi there", "id": "prt_t1", "sessionID": "ses_test123", "messageID": "msg_a1"},
        {"type": "step-finish", "reason": "stop", "id": "prt_f1", "sessionID": "ses_test123", "messageID": "msg_a1"}
      ]
    },
    {
      "info": {"role": "assistant", "id": "msg_a2", "time": {"created": 1787000000300}},
      "parts": [
        {"type": "step-start", "id": "prt_s2", "sessionID": "ses_test123", "messageID": "msg_a2"},
        {"type": "tool", "tool": "bash", "callID": "tool_1",
         "state": {"status": "completed", "input": {"command": "echo hi"}, "output": "hi\n"},
         "id": "prt_tool1", "sessionID": "ses_test123", "messageID": "msg_a2"},
        {"type": "patch", "hash": "abc", "files": ["/home/me/proj/f.md"],
         "id": "prt_p1", "sessionID": "ses_test123", "messageID": "msg_a2"},
        {"type": "step-finish", "reason": "tool-calls", "id": "prt_f2", "sessionID": "ses_test123", "messageID": "msg_a2"}
      ]
    },
    {
      "info": {"role": "user", "id": "msg_u2", "time": {"created": 1787000000400}},
      "parts": [
        {"type": "text", "text": "continue", "id": "prt_u2", "sessionID": "ses_test123", "messageID": "msg_u2"}
      ]
    },
    {
      "info": {"role": "assistant", "id": "msg_a3", "time": {"created": 1787000000500}},
      "parts": [
        {"type": "text", "text": "done", "id": "prt_t3a", "sessionID": "ses_test123", "messageID": "msg_a3"},
        {"type": "text", "text": " second chunk", "id": "prt_t3b", "sessionID": "ses_test123", "messageID": "msg_a3"}
      ]
    }
  ]
}`

func mustParse(t *testing.T) *Session {
	t.Helper()
	s, err := parseExport([]byte(fixture))
	if err != nil {
		t.Fatalf("parseExport: %v", err)
	}
	return s
}

func TestParseExportMeta(t *testing.T) {
	s := mustParse(t)
	if s.ID != "ses_test123" || s.Directory != "/home/me/proj" {
		t.Fatalf("bad info: %+v", s)
	}
	if s.ModelProviderID != "kimi-for-coding" || s.ModelID != "k3" {
		t.Fatalf("bad model: %s/%s", s.ModelProviderID, s.ModelID)
	}
	if len(s.Messages) != 5 {
		t.Fatalf("want 5 messages, got %d", len(s.Messages))
	}
}

func TestToEventsShape(t *testing.T) {
	s := mustParse(t)
	evs := s.ToEvents()
	// user hello / assistant thinking / assistant text / assistant tool_use /
	// user tool_result / user continue / assistant text done / assistant chunk
	if len(evs) != 8 {
		t.Fatalf("want 8 events, got %d: %s", len(evs), eventKinds(evs))
	}
	wantKinds := []string{"user", "stream", "stream", "stream", "stream", "user", "stream", "stream"}
	for i, w := range wantKinds {
		if evs[i].Kind != w {
			t.Fatalf("event %d kind = %s, want %s", i, evs[i].Kind, w)
		}
	}

	// Event 0: user prompt echo, plain text payload.
	var u map[string]string
	if err := json.Unmarshal(evs[0].Data, &u); err != nil || u["text"] != "hello" {
		t.Fatalf("user event payload: %s err=%v", evs[0].Data, err)
	}

	// Event 1: the reasoning part, as a claude-shaped thinking block. step-*
	// and patch stay skipped.
	think := streamObj(t, evs[1])["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if think["type"] != "thinking" || think["thinking"] != "let me think" || think["id"] != "prt_r1" {
		t.Fatalf("thinking block: %v", think)
	}

	// Event 2: assistant text of msg_a1.
	asst := streamObj(t, evs[2])
	if asst["type"] != "assistant" {
		t.Fatalf("event 2 type: %v", asst["type"])
	}
	msg := asst["message"].(map[string]any)
	if msg["id"] != "msg_a1" {
		t.Fatalf("assistant msg id: %v", msg["id"])
	}
	blocks := msg["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(blocks))
	}
	b0 := blocks[0].(map[string]any)
	if b0["type"] != "text" || b0["text"] != "hi there" || b0["id"] != "prt_t1" {
		t.Fatalf("text block: %v", b0)
	}

	// Events 3+4: tool_use then tool_result paired by callID.
	ev3 := streamObj(t, evs[3])
	tu := ev3["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "tool_1" || tu["name"] != "bash" {
		t.Fatalf("tool_use block: %v", tu)
	}
	if in := tu["input"].(map[string]any); in["command"] != "echo hi" {
		t.Fatalf("tool input: %v", tu["input"])
	}
	ev4 := streamObj(t, evs[4])
	if ev4["type"] != "user" {
		t.Fatalf("tool_result event type: %v", ev4["type"])
	}
	tr := ev4["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "tool_1" {
		t.Fatalf("tool_result block: %v", tr)
	}
	if trContent := tr["content"].([]any)[0].(map[string]any); trContent["text"] != "hi\n" {
		t.Fatalf("tool_result content: %v", tr["content"])
	}

	// Events 6+7: two text parts of one step stay two distinct events with the
	// same message id (fingerprints differ via the part id on text blocks).
	for i := 6; i <= 7; i++ {
		a := streamObj(t, evs[i])
		am := a["message"].(map[string]any)
		if am["id"] != "msg_a3" {
			t.Fatalf("event %d msg id: %v", i, am["id"])
		}
	}
}

// TestLiveDedup simulates the live path where a tool part arrives first as
// running and later as completed: the tool_use card is emitted once and the
// tool_result arrives only with the completed state.
func TestLiveDedup(t *testing.T) {
	ts := time.UnixMilli(1787000000300)
	running := map[string]any{
		"type": "tool", "tool": "bash", "callID": "tool_1",
		"state": map[string]any{"status": "running", "input": map[string]any{"command": "sleep 1"}},
	}
	completed := map[string]any{
		"type": "tool", "tool": "bash", "callID": "tool_1",
		"state": map[string]any{"status": "completed", "input": map[string]any{"command": "sleep 1"}, "output": "ok\n"},
	}
	tx := NewToolTx()
	out1 := AdaptPart("msg_x", running, ts, tx)
	if len(out1) != 1 {
		t.Fatalf("running tool part: want 1 event (tool_use only), got %d", len(out1))
	}
	out2 := AdaptPart("msg_x", completed, ts, tx)
	if len(out2) != 1 {
		t.Fatalf("completed tool part: want 1 event (tool_result only), got %d", len(out2))
	}
	// A repeated completed event for the same call id must not duplicate.
	if out3 := AdaptPart("msg_x", completed, ts, tx); len(out3) != 0 {
		t.Fatalf("repeated completed part: want 0 events, got %d", len(out3))
	}
}

// Reasoning is announced empty and streamed through message.part.delta, so the
// live path sees it twice: once with no text, once assembled. The empty one must
// render nothing at all, and a repeated assembled one must not grow a second
// thinking card.
func TestLiveReasoningDedup(t *testing.T) {
	ts := time.UnixMilli(1787000000300)
	empty := map[string]any{"type": "reasoning", "text": "", "id": "prt_r1"}
	full := map[string]any{"type": "reasoning", "text": "let me think", "id": "prt_r1"}

	tx := NewToolTx()
	if out := AdaptPart("msg_x", empty, ts, tx); len(out) != 0 {
		t.Fatalf("empty reasoning part: want 0 events, got %d", len(out))
	}
	out := AdaptPart("msg_x", full, ts, tx)
	if len(out) != 1 {
		t.Fatalf("assembled reasoning part: want 1 event, got %d", len(out))
	}
	blk := streamObjFrom(t, out[0])["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if blk["type"] != "thinking" || blk["thinking"] != "let me think" {
		t.Fatalf("reasoning block: %v", blk)
	}
	if again := AdaptPart("msg_x", full, ts, tx); len(again) != 0 {
		t.Fatalf("repeated reasoning part: want 0 events, got %d", len(again))
	}
	// Whitespace-only reasoning must not open an empty card either.
	if out := AdaptPart("msg_y", map[string]any{
		"type": "reasoning", "text": "  \n ", "id": "prt_r2",
	}, ts, tx); len(out) != 0 {
		t.Fatalf("blank reasoning part: want 0 events, got %d", len(out))
	}
}

func TestToolErrorFlag(t *testing.T) {
	ts := time.UnixMilli(1787000000300)
	part := map[string]any{
		"type": "tool", "tool": "webfetch", "callID": "tool_e",
		"state": map[string]any{"status": "error", "input": map[string]any{"url": "http://x"},
			"output": "fetch failed"},
	}
	out := AdaptPart("msg_e", part, ts, NewToolTx())
	if len(out) != 2 {
		t.Fatalf("error tool part: want 2 events, got %d", len(out))
	}
	tr := streamObjFrom(t, out[1])
	block := tr["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if block["is_error"] != true {
		t.Fatalf("tool_result should carry is_error: %v", block)
	}
}

func TestPreviewOrder(t *testing.T) {
	s := mustParse(t)
	p := s.Preview(3)
	if len(p) != 3 {
		t.Fatalf("want 3 preview messages, got %d", len(p))
	}
	// Newest-first walk (limit 3, msg_a2 is tool-only and skipped): collected
	// msg_a3 + user continue + msg_a1, then reversed to chronological order.
	// The first user message (hello) is beyond the limit.
	if p[0].Role != "assistant" || p[0].Text != "hi there" {
		t.Fatalf("p[0]: %+v", p[0])
	}
	// Echo quotes are stripped on the way out (see stripEchoQuotes) — user
	// messages carry opencode's double-quote echo wrap on export.
	if len(p) > 1 && p[1].Role == "user" {
		if strings.HasPrefix(p[1].Text, "\"") {
			t.Fatalf("user preview text still quoted: %q", p[1].Text)
		}
	}
	if p[1].Role != "user" || p[1].Text != "continue" {
		t.Fatalf("p[1]: %+v", p[1])
	}
	if p[2].Role != "assistant" || p[2].Text != "done\n second chunk" {
		t.Fatalf("p[2]: %+v", p[2])
	}
}

// TestUserAttachmentFilter: a user message with `-f` attachments carries
// extra expansion parts before the prompt echo — they must not leak into the
// user bubble, and the remaining echo text must stay exactly the prompt.
func TestUserAttachmentFilter(t *testing.T) {
	raw := `{
	  "info": {"id": "ses_att", "title": "att", "directory": "/tmp/x",
	    "model": {"id": "m1", "providerID": "p1"}, "time": {"created": 1787000000000, "updated": 1787000000100}},
	  "messages": [
	    {"info": {"role": "user", "id": "msg_u", "time": {"created": 1787000000100}}, "parts": [
	      {"type": "text", "text": "Called the Read tool with the following input: {\"filePath\":\"/tmp/x/a.txt\"}", "id": "p1"},
	      {"type": "text", "text": "<path>/tmp/x/a.txt</path>\n<type>file</type>\n<content>\n1: hello world\n\n(End of file)", "id": "p2"},
	      {"type": "file", "path": "/tmp/x/a.txt", "id": "p3"},
	      {"type": "text", "text": "\"what does the file say?\"", "id": "p4"}
	    ]}
	  ]
	}`
	s, err := parseExport([]byte(raw))
	if err != nil {
		t.Fatalf("parseExport: %v", err)
	}
	evs := s.ToEvents()
	if len(evs) != 1 {
		t.Fatalf("want exactly 1 user event, got %d", len(evs))
	}
	var u map[string]string
	if err := json.Unmarshal(evs[0].Data, &u); err != nil || u["text"] != "what does the file say?" {
		t.Fatalf("user text = %q (err=%v), want the bare prompt", u["text"], err)
	}
	// Preview must apply the same filter.
	p := s.Preview(2)
	if len(p) != 1 || p[0].Role != "user" || p[0].Text != "what does the file say?" {
		t.Fatalf("preview: %+v", p)
	}
}

func TestStripEchoQuotes(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"hello world"`, "hello world"}, // opencode echo wrap
		{`""quoted""`, `"quoted"`},       // user genuinely quoted + wrap
		{`plain`, "plain"},               // no wrap (not echoed)
		{`"`, `"`},                       // lone quote stays
		{"  \"pad\"  ", "pad"},           // whitespace trimmed
		{`mixed "quotes" inside`, `mixed "quotes" inside`},
	}
	for _, c := range cases {
		if got := stripEchoQuotes(c.in); got != c.want {
			t.Errorf("stripEchoQuotes(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func streamObj(t *testing.T, ev *store.Event) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	return m
}

func streamObjFrom(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func eventKinds(evs []*store.Event) string {
	out := ""
	for _, e := range evs {
		out += e.Kind + " "
	}
	return out
}

// Regression: opencode's first tool part arrives as status=pending with
// state.input == {}, and the arguments only land on the next tick. Emitting the
// tool_use block there produced a card with the tool name and no arguments,
// and ToolTx marked the call as sent so it was never re-emitted.
func TestAdaptToolWaitsForInput(t *testing.T) {
	tx := NewToolTx()

	// pending, no arguments: must emit nothing.
	pending := map[string]any{
		"type": "tool", "tool": "bash", "callID": "c1",
		"state": map[string]any{"status": "pending", "input": map[string]any{}},
	}
	if got := AdaptPart("m1", pending, time.Now(), tx); len(got) != 0 {
		t.Fatalf("pending tool emitted %d events, want 0", len(got))
	}

	// running WITH arguments: now the block carries the command.
	running := map[string]any{
		"type": "tool", "tool": "bash", "callID": "c1",
		"state": map[string]any{
			"status": "running",
			"input":  map[string]any{"command": "ls -la"},
		},
	}
	got := AdaptPart("m1", running, time.Now(), tx)
	if len(got) != 1 {
		t.Fatalf("running tool emitted %d events, want 1", len(got))
	}
	msg := got[0].(map[string]any)["message"].(map[string]any)
	blk := msg["content"].([]any)[0].(map[string]any)
	if blk["type"] != "tool_use" {
		t.Fatalf("block type = %v", blk["type"])
	}
	in, _ := blk["input"].(map[string]any)
	if in == nil || in["command"] != "ls -la" {
		t.Fatalf("tool_use input = %v, want the bash command", blk["input"])
	}

	// Later ticks must not duplicate the block.
	if got := AdaptPart("m1", running, time.Now(), tx); len(got) != 0 {
		t.Fatalf("repeat running emitted %d events, want 0", len(got))
	}
}
