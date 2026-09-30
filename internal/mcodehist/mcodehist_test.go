package mcodehist

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---- live stream adaptation ----

// realStreamLines is the stdout of
// `mcode exec --output-format stream-json --permission full "list the files…"`
// on 0.5.9, trimmed: one assistant step with a text block, a tool call that
// succeeds, and a following reasoning block.
var realStreamLines = []string{
	`{"schemaVersion":1,"sequence":1,"timestampMs":1790738206733,"runId":"exec_turn_x","sessionId":"mvs_d9caaf9039494df7bb31b882922e95d0","turnId":"turn_x","type":"exec.started"}`,
	`{"schemaVersion":1,"sequence":2,"timestampMs":1790738206733,"runId":"exec_turn_x","sessionId":"mvs_d9caaf9039494df7bb31b882922e95d0","turnId":"turn_x","type":"session.started"}`,
	`{"schemaVersion":1,"sequence":4,"timestampMs":1790738208813,"runId":"exec_turn_x","sessionId":"mvs_d9caaf9039494df7bb31b882922e95d0","turnId":"turn_x","type":"item.started","item":{"id":"35da:message","type":"agent_message","contentDelta":"I'll list the files"}}`,
	`{"schemaVersion":1,"sequence":6,"timestampMs":1790738208824,"runId":"exec_turn_x","sessionId":"mvs_d9caaf9039494df7bb31b882922e95d0","turnId":"turn_x","type":"item.started","item":{"id":"call_function_pbi1b2hxw685_1","type":"tool_call","toolCall":{"id":"call_function_pbi1b2hxw685_1","name":"glob","status":4}}}`,
	`{"schemaVersion":1,"sequence":9,"timestampMs":1790738208851,"runId":"exec_turn_x","sessionId":"mvs_d9caaf9039494df7bb31b882922e95d0","turnId":"turn_x","type":"item.updated","item":{"id":"call_function_pbi1b2hxw685_1","type":"tool_call","toolCall":{"id":"call_function_pbi1b2hxw685_1","name":"glob","status":1,"input":{"pattern":"*"}}}}`,
	`{"schemaVersion":1,"sequence":10,"timestampMs":1790738208945,"runId":"exec_turn_x","sessionId":"mvs_d9caaf9039494df7bb31b882922e95d0","turnId":"turn_x","type":"item.updated","item":{"id":"call_function_pbi1b2hxw685_1","type":"tool_call","toolCall":{"id":"call_function_pbi1b2hxw685_1","name":"glob","status":2,"input":{"pattern":"*"},"output":{"content":[{"type":"text","text":"No files matched"}],"details":{"matched":0}}}}}`,
	`{"schemaVersion":1,"sequence":11,"timestampMs":1790738209084,"runId":"exec_turn_x","sessionId":"mvs_d9caaf9039494df7bb31b882922e95d0","turnId":"turn_x","type":"item.completed","item":{"id":"35da:message","type":"agent_message","content":"I'll list the files in the working directory using the glob tool."}}`,
	`{"schemaVersion":1,"sequence":12,"timestampMs":1790738209084,"runId":"exec_turn_x","sessionId":"mvs_d9caaf9039494df7bb31b882922e95d0","turnId":"turn_x","type":"item.completed","item":{"id":"call_function_pbi1b2hxw685_1","type":"tool_call","toolCall":{"id":"call_function_pbi1b2hxw685_1","name":"glob","status":2,"input":{"pattern":"*"},"output":{"content":[{"type":"text","text":"No files matched"}],"details":{"matched":0}}}}}`,
	`{"schemaVersion":1,"sequence":13,"timestampMs":1790738210125,"runId":"exec_turn_x","sessionId":"mvs_d9caaf9039494df7bb31b882922e95d0","turnId":"turn_x","type":"item.started","item":{"id":"e63e:reasoning","type":"reasoning","contentDelta":"The directory appears"}}`,
	`{"schemaVersion":1,"sequence":14,"timestampMs":1790738210598,"runId":"exec_turn_x","sessionId":"mvs_d9caaf9039494df7bb31b882922e95d0","turnId":"turn_x","type":"item.completed","item":{"id":"e63e:reasoning","type":"reasoning","content":"The directory appears empty."}}`,
	`{"schemaVersion":1,"sequence":15,"timestampMs":1790738213300,"runId":"exec_turn_x","sessionId":"mvs_d9caaf9039494df7bb31b882922e95d0","turnId":"turn_x","type":"turn.completed","model":{"providerId":"minimax","modelId":"MiniMax-M3.1-Flash-Preview","variant":"thinking"},"usage":{"inputTokens":13779,"outputTokens":244,"cacheReadTokens":32371,"totalTokens":14023},"durationMs":6280}`,
}

func TestParseLiveCarriesSessionID(t *testing.T) {
	ev, ok := ParseLive([]byte(realStreamLines[1]))
	if !ok {
		t.Fatal("line 2 did not parse")
	}
	if ev.Type != "session.started" {
		t.Fatalf("type = %q", ev.Type)
	}
	if ev.SessionIDOf() != "mvs_d9caaf9039494df7bb31b882922e95d0" {
		t.Fatalf("sessionId = %q", ev.SessionIDOf())
	}
	if ev.TS.IsZero() {
		t.Fatal("timestampMs did not become a time")
	}
}

func TestParseLiveSkipsBlank(t *testing.T) {
	if _, ok := ParseLive([]byte("   ")); ok {
		t.Fatal("blank line should not parse")
	}
	if _, ok := ParseLive([]byte("{not json")); ok {
		t.Fatal("garbage should not parse")
	}
}

// TestAdaptFullStream is the end-to-end check on a real captured run: one
// tool call (running → completed, one card, one result), one text block and
// one reasoning block — the exact shapes the frontend renders.
//
// The order is the CLI's: a tool card is emitted as soon as the call has its
// input, which on a real run lands before the step's final text block (the
// text only completes at the end of the step). claude's split lines have the
// same tool-before-text ordering, so the frontend handles it unchanged.
func TestAdaptFullStream(t *testing.T) {
	tx := NewToolTx()
	var payloads []any
	var usage any
	for _, line := range realStreamLines {
		ev, ok := ParseLive([]byte(line))
		if !ok {
			t.Fatalf("line did not parse: %s", line)
		}
		payloads = append(payloads, ev.Adapt(tx)...)
		if ev.Type == "turn.completed" {
			usage = UsagePayload(ev.Usage, ev.Model, ev.TS)
		}
	}

	// tool_use, tool_result, text, reasoning = 4 events
	if len(payloads) != 4 {
		for i, p := range payloads {
			b, _ := json.Marshal(p)
			t.Logf("payload[%d] = %s", i, b)
		}
		t.Fatalf("got %d payloads, want 4", len(payloads))
	}

	// 1) tool_use — emitted once, at status 1 (input ready), not at the
	// pending statuses 4/5
	first := payloads[0].(map[string]any)
	if first["type"] != "assistant" {
		t.Fatalf("payload 0 type = %v", first["type"])
	}
	smsg := first["message"].(map[string]any)
	tb := smsg["content"].([]any)[0].(map[string]any)
	if tb["type"] != "tool_use" || tb["name"] != "glob" || tb["id"] != "call_function_pbi1b2hxw685_1" {
		t.Fatalf("tool_use block = %+v", tb)
	}
	if _, ok := tb["input"].(map[string]any); !ok {
		t.Fatalf("tool_use lost its input: %+v", tb)
	}

	// 2) tool_result
	second := payloads[1].(map[string]any)
	if second["type"] != "user" {
		t.Fatalf("payload 1 type = %v, want a user tool_result", second["type"])
	}
	rb := second["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if rb["type"] != "tool_result" || rb["tool_use_id"] != "call_function_pbi1b2hxw685_1" {
		t.Fatalf("tool_result block = %+v", rb)
	}
	if _, isErr := rb["is_error"]; isErr {
		t.Fatal("a succeeded call must not be marked is_error")
	}
	rtext := rb["content"].([]any)[0].(map[string]any)["text"]
	if rtext != "No files matched" {
		t.Fatalf("tool result text = %v", rtext)
	}

	// 3) assistant text block
	third := payloads[2].(map[string]any)
	tmsg := third["message"].(map[string]any)
	if tmsg["id"] != "35da" {
		t.Fatalf("message id = %v, want the item uuid prefix", tmsg["id"])
	}
	block := tmsg["content"].([]any)[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "I'll list the files in the working directory using the glob tool." {
		t.Fatalf("text block = %+v", block)
	}
	if block["id"] != "35da:message" {
		t.Fatalf("block id = %v, want the full item id so replay fingerprints differ", block["id"])
	}

	// 4) reasoning is forwarded (mcode streams it live, so replay keeps
	// parity with the live view)
	fourth := payloads[3].(map[string]any)
	fb := fourth["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if fb["type"] != "thinking" || fb["thinking"] != "The directory appears empty." {
		t.Fatalf("reasoning block = %+v", fb)
	}

	// usage rides its own assistant event with an empty content list
	if usage == nil {
		t.Fatal("turn.completed produced no usage payload")
	}
	um := usage.(map[string]any)["message"].(map[string]any)
	if len(um["content"].([]any)) != 0 {
		t.Fatal("usage event must carry no renderable content")
	}
	u := um["usage"].(map[string]any)
	if u["input_tokens"].(int64) != 13779 || u["output_tokens"].(int64) != 244 || u["cache_read_input_tokens"].(int64) != 32371 {
		t.Fatalf("usage = %+v", u)
	}
	if um["model"] != "MiniMax-M3.1-Flash-Preview" {
		t.Fatalf("model = %v", um["model"])
	}
}

// TestToolCallNotDuplicated pins the running→completed collapse: the same call
// crosses item.started, item.updated and item.completed, and must still
// produce exactly one tool_use and one tool_result.
func TestToolCallNotDuplicated(t *testing.T) {
	tx := NewToolTx()
	uses, results := 0, 0
	for _, line := range realStreamLines {
		ev, ok := ParseLive([]byte(line))
		if !ok {
			continue
		}
		for _, p := range ev.Adapt(tx) {
			m := p.(map[string]any)
			if m["type"] == "assistant" {
				uses++
			} else {
				results++
			}
		}
	}
	if uses != 3 { // one tool_use + one text + one reasoning
		t.Fatalf("assistant events = %d, want 3", uses)
	}
	if results != 1 {
		t.Fatalf("tool_result events = %d, want 1", results)
	}
}

func TestToolCallErrorMarked(t *testing.T) {
	line := `{"type":"item.completed","item":{"id":"c1","type":"tool_call","toolCall":{"id":"c1","name":"bash","status":3,"input":{"cmd":"x"},"output":{"content":[{"type":"text","text":"boom"}]}}}}`
	ev, ok := ParseLive([]byte(line))
	if !ok {
		t.Fatal("did not parse")
	}
	payloads := ev.Adapt(NewToolTx())
	if len(payloads) != 2 {
		t.Fatalf("got %d payloads, want tool_use + tool_result", len(payloads))
	}
	rb := payloads[1].(map[string]any)["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if rb["is_error"] != true {
		t.Fatalf("status 3 (failed) must mark is_error: %+v", rb)
	}
}

// ---- transcript replay ----

// realTranscript is a messages.jsonl body as mcode 0.5.9 writes it: the user
// line carries the injected <system-reminder> block ahead of the real prompt.
var realTranscript = `{"message_id":"msg-user-1","turn_id":"t1","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>\n<agent-context>\n  assistant: MiniMax Code\n</agent-context>\n</system-reminder>"}],"timestamp":1790738206634}}
{"message_id":"msg-a1","turn_id":"t1","message":{"role":"assistant","content":[{"type":"text","text":"I'll list the files."},{"type":"toolCall","id":"call_1","name":"glob","arguments":{"pattern":"*"}}],"api":"anthropic-messages","provider":"minimax","model":"MiniMax-M3.1-Flash-Preview","usage":{"input":13717,"output":51,"cacheRead":1564,"cacheWrite":0,"totalTokens":15332},"timestamp":1790738206640}}
{"message_id":"msg-t1","turn_id":"t1","message":{"role":"toolResult","toolCallId":"call_1","toolName":"glob","content":[{"type":"text","text":"No files matched"}],"isError":false,"timestamp":1790738206650}}
{"message_id":"msg-a2","turn_id":"t1","message":{"role":"assistant","content":[{"type":"text","text":"The directory is empty."}],"provider":"minimax","model":"MiniMax-M3.1-Flash-Preview","usage":{"input":100,"output":20,"cacheRead":0,"cacheWrite":0},"timestamp":1790738206700}}`

func writeTranscript(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	sid := "mvs_test00000000000000000000000000"
	// The directory name carries base64(sid) (mcode writes it unpadded as
	// "-session_<base64>"), and FindSessionDir matches on the manifest, so
	// compute the name rather than hard-coding it.
	name := filepath.Join(dir, "v2", "sessions", "2026", "09", "30",
		"03-16-46-634-session_"+base64.RawURLEncoding.EncodeToString([]byte(sid)))
	if err := os.MkdirAll(name, 0o700); err != nil {
		t.Fatal(err)
	}
	mf, _ := json.Marshal(map[string]any{
		"schemaVersion": 1, "sessionId": sid,
		"createdAtMs": 1790738206634, "updatedAtMs": 1790738206700,
	})
	if err := os.WriteFile(filepath.Join(name, "manifest.json"), mf, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(name, "messages.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(dataDirEnv, dir)
}

func TestFetchSessionStripsSystemReminder(t *testing.T) {
	writeTranscript(t, realTranscript)
	sess, err := FetchSession("mvs_test00000000000000000000000000")
	if err != nil {
		t.Fatalf("FetchSession: %v", err)
	}
	// A message that is nothing but the injected reminder is not a prompt.
	for _, m := range sess.Messages {
		if m.Role != "user" {
			continue
		}
		if got := firstUserText(m); got != "" {
			t.Fatalf("pure system-reminder message leaked as a prompt: %q", got)
		}
	}
	if sess.ModelID != "MiniMax-M3.1-Flash-Preview" {
		t.Fatalf("ModelID = %q", sess.ModelID)
	}
	if sess.ProviderID != "minimax" {
		t.Fatalf("ProviderID = %q", sess.ProviderID)
	}
}

func TestFirstUserTextKeepsRealPrompt(t *testing.T) {
	m := SessionMessage{Blocks: []map[string]any{
		{"type": "text", "text": "<system-reminder>\nruntime context\n</system-reminder>"},
		{"type": "text", "text": "please refactor the parser"},
	}}
	if got := firstUserText(m); got != "please refactor the parser" {
		t.Fatalf("firstUserText = %q", got)
	}
	// An xml-ish remainder is injection, not a typed prompt.
	only := SessionMessage{Blocks: []map[string]any{
		{"type": "text", "text": "<system-reminder>x</system-reminder>"},
		{"type": "text", "text": "<task-notification>done</task-notification>"},
	}}
	if got := firstUserText(only); got != "" {
		t.Fatalf("injection leaked: %q", got)
	}
}

func TestToEventsReplaysToolPairing(t *testing.T) {
	writeTranscript(t, realTranscript)
	sess, err := FetchSession("mvs_test00000000000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	evs := sess.ToEvents()
	// 2 assistant steps × text, 1 tool_use, 1 tool_result, 0 user (pure injection)
	if len(evs) != 4 {
		for i, e := range evs {
			t.Logf("event[%d] kind=%s data=%s", i, e.Kind, e.Data)
		}
		t.Fatalf("got %d events, want 4", len(evs))
	}
	// the tool_use must be on the assistant event that carries the call
	var use, result bool
	for _, e := range evs {
		var obj map[string]any
		if err := json.Unmarshal(e.Data, &obj); err != nil {
			t.Fatal(err)
		}
		msg, _ := obj["message"].(map[string]any)
		if msg == nil {
			continue
		}
		for _, c := range msg["content"].([]any) {
			b := c.(map[string]any)
			if b["type"] == "tool_use" && b["id"] == "call_1" {
				use = true
			}
			if b["type"] == "tool_result" && b["tool_use_id"] == "call_1" {
				result = true
			}
		}
	}
	if !use || !result {
		t.Fatalf("replay lost the tool pair: use=%v result=%v", use, result)
	}
}

func TestScanSessionCounts(t *testing.T) {
	writeTranscript(t, realTranscript+"\n"+
		`{"message_id":"msg-u2","turn_id":"t2","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>"},{"type":"text","text":"thanks"}],"timestamp":1790738210000}}`)
	sess, err := FetchSession("mvs_test00000000000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	fa := ScanSession(sess)
	if !fa.Active {
		t.Fatal("session should be active")
	}
	// 2 assistant steps + 1 real user prompt
	if fa.Counted != 3 {
		t.Fatalf("Counted = %d, want 3", fa.Counted)
	}
	if fa.ToolUsage["glob"] != 1 {
		t.Fatalf("ToolUsage = %+v, want one glob call", fa.ToolUsage)
	}
	day := sess.Messages[0].TS.Format("2006-01-02")
	if fa.DayTool[day] != 1 {
		t.Fatalf("DayTool = %+v", fa.DayTool)
	}
	mu, ok := fa.ModelUsage["minimax/MiniMax-M3.1-Flash-Preview"]
	if !ok {
		t.Fatalf("ModelUsage = %+v", fa.ModelUsage)
	}
	// 13717+100 input, 51+20 output, 1564 cacheRead
	if mu.InputTokens != 13817 || mu.OutputTokens != 71 || mu.CacheReadTokens != 1564 {
		t.Fatalf("model usage = %+v", mu)
	}
	if mu.Messages != 2 {
		t.Fatalf("model messages = %d, want 2 (one per step)", mu.Messages)
	}
}

// ---- model catalog ----

const realConfig = `logLevel: info
defaultModel: minimax/MiniMax-M3.1-Flash-Preview
provider:
  minimax:
    name: MiniMax
    models:
      MiniMax-M2.7:
        name: M2.7
        attachment: false
        reasoning: true
        thinking_config:
          mode: forced_on
      MiniMax-M3:
        name: M3
        attachment: true
        reasoning: true
        modalities:
          input:
            - text
            - image
        variants:
          none-thinking:
            thinking:
              type: disabled
          thinking:
            thinking:
              type: adaptive
      MiniMax-M3.1-Flash-Preview:
        name: M3.1-Flash-Preview
        attachment: true
        reasoning: true
        modalities:
          input:
            - text
            - image
        thinking:
          effortOptions:
            - default
            - low
            - medium
            - high
            - xhigh
            - max
          defaultEffort: default
    model_order:
      - MiniMax-M3.1-Flash-Preview
      - MiniMax-M3
      - MiniMax-M2.7
defaultModelVariant: thinking
`

func writeConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(configEnv, dir)
}

func TestCatalogOrderAndEfforts(t *testing.T) {
	writeConfig(t, realConfig)
	cat, err := LoadCatalog("minimax")
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	// model_order decides the dropdown order, not map order.
	want := []string{"MiniMax-M3.1-Flash-Preview", "MiniMax-M3", "MiniMax-M2.7"}
	got := cat.ModelIDs()
	if len(got) != len(want) {
		t.Fatalf("models = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("models = %v, want %v", got, want)
		}
	}
	if cat.DefaultModel != "MiniMax-M3.1-Flash-Preview" {
		t.Fatalf("DefaultModel = %q", cat.DefaultModel)
	}

	// effortOptions wins where declared…
	m31 := cat.Find("MiniMax-M3.1-Flash-Preview")
	if m31 == nil || len(m31.Efforts) != 6 || m31.Efforts[0] != "default" || m31.Efforts[5] != "max" {
		t.Fatalf("M3.1 efforts = %+v", m31)
	}
	if m31.DefaultEffort != "default" {
		t.Fatalf("DefaultEffort = %q", m31.DefaultEffort)
	}
	if len(m31.Input) != 2 || m31.Input[1] != "image" {
		t.Fatalf("M3.1 input modalities = %v", m31.Input)
	}
	// …variant keys stand in where they are not…
	m3 := cat.Find("MiniMax-M3")
	if m3 == nil || len(m3.Efforts) != 2 || m3.Efforts[0] != "none-thinking" {
		t.Fatalf("M3 efforts = %+v", m3)
	}
	// …and a forced-on model has no switchable depth, so no dropdown.
	m27 := cat.Find("MiniMax-M2.7")
	if m27 == nil || len(m27.Efforts) != 0 {
		t.Fatalf("M2.7 efforts = %+v, want none", m27)
	}
	if m27.Attachment {
		t.Fatal("M2.7 declares attachment: false")
	}
}

func TestFindAcceptsQualifiedModelID(t *testing.T) {
	writeConfig(t, realConfig)
	cat, err := LoadCatalog("minimax")
	if err != nil {
		t.Fatal(err)
	}
	if cat.Find("minimax/MiniMax-M3") == nil {
		t.Fatal("a provider-qualified model id should resolve")
	}
}

func TestEffortOK(t *testing.T) {
	writeConfig(t, realConfig)
	cat, err := LoadCatalog("minimax")
	if err != nil {
		t.Fatal(err)
	}
	if !EffortOK(cat, "MiniMax-M3.1-Flash-Preview", "xhigh") {
		t.Fatal("xhigh is a declared level")
	}
	if EffortOK(cat, "MiniMax-M3.1-Flash-Preview", "ultra") {
		t.Fatal("ultra is not a declared level — the CLI would reject it after spawn")
	}
	if EffortOK(cat, "MiniMax-M3", "max") {
		t.Fatal("M3 only has none-thinking/thinking")
	}
	// inherit is always allowed
	if !EffortOK(cat, "MiniMax-M2.7", "") {
		t.Fatal("empty means inherit and must be accepted")
	}
	// unknown model: do not block the session over a stale catalog
	if !EffortOK(cat, "SomeNewModel", "whatever") {
		t.Fatal("an unknown model should not be rejected here")
	}
	// nil catalog (config unreadable) must not block saving either
	if !EffortOK(nil, "m", "x") {
		t.Fatal("a nil catalog must accept the value and let the CLI decide")
	}
}

func TestBareModelID(t *testing.T) {
	if got := BareModelID("minimax/MiniMax-M3"); got != "MiniMax-M3" {
		t.Fatalf("BareModelID = %q", got)
	}
	if got := BareModelID("MiniMax-M3"); got != "MiniMax-M3" {
		t.Fatalf("BareModelID on a bare id = %q", got)
	}
}

func TestParseTimestampUnits(t *testing.T) {
	if got := parseTimestamp(float64(1790738206634)).UnixMilli(); got != 1790738206634 {
		t.Fatalf("millisecond timestamp mangled: %d", got)
	}
	if got := parseTimestamp(float64(1790738206)).Unix(); got != 1790738206 {
		t.Fatalf("second timestamp mangled: %d", got)
	}
	if !parseTimestamp(nil).IsZero() {
		t.Fatal("nil timestamp should stay zero")
	}
	_ = time.Now()
}

func TestMessageIDOf(t *testing.T) {
	if got := messageIDOf("35da9a65-…:message"); got != "35da9a65-…" {
		t.Fatalf("messageIDOf = %q", got)
	}
	if got := messageIDOf("call_function_x"); got != "call_function_x" {
		t.Fatalf("a colon-less id passes through: %q", got)
	}
}
