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
// (reasoning / patch / step-*) are present in realistic positions.
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
	// user hello / assistant text / assistant tool_use / user tool_result /
	// user continue / assistant text done / assistant text chunk
	if len(evs) != 7 {
		t.Fatalf("want 7 events, got %d: %s", len(evs), eventKinds(evs))
	}
	wantKinds := []string{"user", "stream", "stream", "stream", "user", "stream", "stream"}
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

	// Event 1: assistant text of msg_a1 — reasoning/step parts skipped.
	asst := streamObj(t, evs[1])
	if asst["type"] != "assistant" {
		t.Fatalf("event 1 type: %v", asst["type"])
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

	// Events 2+3: tool_use then tool_result paired by callID.
	ev2 := streamObj(t, evs[2])
	tu := ev2["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "tool_1" || tu["name"] != "bash" {
		t.Fatalf("tool_use block: %v", tu)
	}
	if in := tu["input"].(map[string]any); in["command"] != "echo hi" {
		t.Fatalf("tool input: %v", tu["input"])
	}
	ev3 := streamObj(t, evs[3])
	if ev3["type"] != "user" {
		t.Fatalf("tool_result event type: %v", ev3["type"])
	}
	tr := ev3["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "tool_1" {
		t.Fatalf("tool_result block: %v", tr)
	}
	if trContent := tr["content"].([]any)[0].(map[string]any); trContent["text"] != "hi\n" {
		t.Fatalf("tool_result content: %v", tr["content"])
	}

	// Events 5+6: two text parts of one step stay two distinct events with the
	// same message id (fingerprints differ via the part id on text blocks).
	for i := 5; i <= 6; i++ {
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

func TestStdoutError(t *testing.T) {
	// Real shape captured from a failing `opencode run --format json`
	// (stderr empty, exit 1 — previously surfaced as bare "exit status 1").
	mustObj := func(raw string) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("unmarshal fixture: %v", err)
		}
		return m
	}
	cases := []struct {
		name       string
		obj        map[string]any
		wantOK     bool
		wantMsgHas string
	}{
		{
			name: "full error shape with ref",
			obj: mustObj(`{"type":"error","timestamp":1788854035991,"sessionID":"ses_x",` +
				`"error":{"name":"UnknownError","data":{"message":"Unexpected server error. Check server logs for details.","ref":"err_98e3619e"}}}`),
			wantOK:     true,
			wantMsgHas: "Unexpected server error",
		},
		{
			name:       "ref appended",
			obj:        mustObj(`{"type":"error","error":{"name":"UnknownError","data":{"message":"boom","ref":"err_1"}}}`),
			wantOK:     true,
			wantMsgHas: "err_1",
		},
		{
			name:       "error as plain string",
			obj:        mustObj(`{"type":"error","error":"Session not found"}`),
			wantOK:     true,
			wantMsgHas: "Session not found",
		},
		{
			name:       "name only falls back to name",
			obj:        mustObj(`{"type":"error","error":{"name":"RateLimited"}}`),
			wantOK:     true,
			wantMsgHas: "RateLimited",
		},
		{
			name:   "step line is not an error",
			obj:    mustObj(`{"type":"step_start","part":{"type":"step-start"}}`),
			wantOK: false,
		},
		{
			name:   "text line is not an error",
			obj:    mustObj(`{"type":"text","part":{"type":"text","text":"hi"}}`),
			wantOK: false,
		},
		{
			name:   "nil obj",
			obj:    nil,
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, ok := StdoutError(c.obj)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (msg %q)", ok, c.wantOK, msg)
			}
			if c.wantOK && !strings.Contains(msg, c.wantMsgHas) {
				t.Fatalf("msg = %q, want it to contain %q", msg, c.wantMsgHas)
			}
		})
	}
}
