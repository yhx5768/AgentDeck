package opencodev2

import (
	"encoding/json"
	"testing"
)

// toolExport is a real `session export --standalone` document (trimmed to the
// fields the reader uses): a user prompt, an assistant step with reasoning +
// a completed shell tool, a second assistant step with text, and an idle
// message.
const toolExport = `{
 "info":{"id":"ses_tool","title":"tool probe","agent":"build",
   "model":{"id":"big-pickle","providerID":"opencode"},
   "time":{"created":1793004622768,"updated":1793004628999},
   "location":{"directory":"/work"}},
 "messages":[
  {"id":"msg_u","time":{"created":1793004624847},"text":"run echo hello-tool","type":"user"},
  {"id":"msg_a1","time":{"created":1793004624866,"streamed":1793004627360,"completed":1793004627427},
   "type":"assistant","agent":"build","model":{"id":"big-pickle","providerID":"opencode"},
   "content":[
     {"type":"reasoning","text":"I should use the shell tool.","time":{"created":1,"completed":2}},
     {"type":"tool","id":"call_1","name":"shell","executed":false,
      "state":{"status":"completed","input":{"command":"echo hello-tool"},
        "content":[{"type":"text","text":"hello-tool\n"}]},
      "time":{"created":3,"ran":4,"completed":5}}
   ],
   "finish":"tool-calls","cost":0,
   "tokens":{"input":70,"output":40,"reasoning":75,"cache":{"read":5760,"write":0}}},
  {"id":"msg_a2","time":{"created":1793004627442,"streamed":1793004628997,"completed":1793004628999},
   "type":"assistant","agent":"build","model":{"id":"big-pickle","providerID":"opencode"},
   "content":[{"type":"text","text":"done"}],
   "finish":"stop","cost":0,
   "tokens":{"input":201,"output":10,"reasoning":0,"cache":{"read":5760,"write":0}}},
  {"id":"msg_idle","time":{"created":1793004629002},"type":"idle","outcome":"succeeded"}
 ]}`

func TestParseExport(t *testing.T) {
	s, err := parseExport([]byte(toolExport))
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "ses_tool" || s.Title != "tool probe" || s.Directory != "/work" {
		t.Fatalf("info = %+v", s)
	}
	if s.ModelID != "big-pickle" || s.ModelProviderID != "opencode" {
		t.Fatalf("model = %s/%s", s.ModelProviderID, s.ModelID)
	}
	if len(s.Messages) != 4 {
		t.Fatalf("messages = %d", len(s.Messages))
	}
	a1 := s.Messages[1]
	if a1.Role != "assistant" || len(a1.Content) != 2 {
		t.Fatalf("a1 = %+v", a1)
	}
	if a1.Tokens.Output != 40 || a1.Tokens.Reasoning != 75 || a1.Tokens.CacheRead != 5760 {
		t.Fatalf("a1 tokens = %+v", a1.Tokens)
	}
	tool := a1.Content[1]
	if tool.Type != "tool" || tool.CallID != "call_1" || tool.Status != "completed" || tool.Output != "hello-tool\n" {
		t.Fatalf("tool = %+v", tool)
	}
	if tool.Input["command"] != "echo hello-tool" {
		t.Fatalf("tool input = %+v", tool.Input)
	}
}

func TestToEventsCanonical(t *testing.T) {
	s, _ := parseExport([]byte(toolExport))
	evs := s.ToEvents()
	// user + reasoning + tool_use + tool_result + text = 5
	if len(evs) != 5 {
		t.Fatalf("events = %d, want 5", len(evs))
	}
	if evs[0].Kind != "user" {
		t.Fatalf("first event kind = %s", evs[0].Kind)
	}
	var u struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(evs[0].Data, &u)
	if u.Text != "run echo hello-tool" {
		t.Fatalf("user text = %q", u.Text)
	}

	kinds := make([]string, 0, len(evs))
	for _, e := range evs[1:] {
		var d map[string]any
		_ = json.Unmarshal(e.Data, &d)
		msg, _ := d["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		for _, c := range content {
			cm, _ := c.(map[string]any)
			kinds = append(kinds, cm["type"].(string))
		}
	}
	want := []string{"thinking", "tool_use", "tool_result", "text"}
	if len(kinds) != len(want) {
		t.Fatalf("blocks = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("blocks = %v, want %v", kinds, want)
		}
	}
}

func TestToEventsUsageShape(t *testing.T) {
	s, _ := parseExport([]byte(toolExport))
	evs := s.ToEvents()
	var d map[string]any
	_ = json.Unmarshal(evs[1].Data, &d) // first assistant block
	msg := d["message"].(map[string]any)
	if msg["id"] != "msg_a1" || msg["model"] != "big-pickle" {
		t.Fatalf("message = %+v", msg)
	}
	usage, _ := msg["usage"].(map[string]any)
	if usage == nil || usage["output_tokens"].(float64) != 40 || usage["reasoning_tokens"].(float64) != 75 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestPreview(t *testing.T) {
	s, _ := parseExport([]byte(toolExport))
	pv := s.Preview(10)
	if len(pv) != 2 {
		t.Fatalf("preview = %+v", pv)
	}
	if pv[0].Role != "user" || pv[1].Role != "assistant" || pv[1].Text != "done" {
		t.Fatalf("preview = %+v", pv)
	}
}

func TestParseCatalog(t *testing.T) {
	raw := []byte(`{"data":[
	  {"id":"big-pickle","modelID":"big-pickle","providerID":"opencode","status":"active",
	   "capabilities":{"input":["text"]},"variants":[]},
	  {"id":"kimi-k3","modelID":"kimi-k3","providerID":"opencode","status":"active",
	   "capabilities":{"input":["text","image"]},"variants":[{"id":"max"},{"id":"low"}]},
	  {"id":"other","modelID":"other","providerID":"other","status":"active","capabilities":{"input":["text"]}},
	  {"id":"old","modelID":"old","providerID":"opencode","status":"deprecated","capabilities":{"input":["text"]}}
	]}`)
	cat, err := parseCatalog(raw, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Models) != 2 {
		t.Fatalf("models = %+v (deprecated/other-provider must be filtered)", cat.Models)
	}
	if !cat.Find("big-pickle") || !cat.Find("kimi-k3") || cat.Find("old") {
		t.Fatalf("Find wrong: %+v", cat.Models)
	}
	got := cat.Variants["kimi-k3"]
	if len(got) != 2 || got[0] != "low" || got[1] != "max" {
		t.Fatalf("variants = %v, want [low max] sorted", got)
	}
	if cat.Models[1].Input[1] != "image" {
		t.Fatalf("image capability lost: %+v", cat.Models[1])
	}
}
