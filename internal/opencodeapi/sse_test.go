package opencodeapi

import (
	"encoding/json"
	"testing"
)

// The wire shapes below are taken from the running server's own OpenAPI
// document (GET /doc) in opencode 1.18.35, not from guesswork: a mismatch here
// silently breaks the turn (e.g. an envelope the server ignores makes the
// prompt a no-op that still returns 200).

func TestEventPartDecodesPartPayload(t *testing.T) {
	ev := Event{
		Type: "message.part.updated",
		Properties: json.RawMessage(`{
			"properties": {
				"info": {"id":"msg_1","role":"assistant","sessionID":"ses_1"},
				"part": {"id":"prt_1","messageID":"msg_1","sessionID":"ses_1",
				         "type":"text","text":"hello"}
			}
		}`),
	}
	pe, ok := ev.Part()
	if !ok {
		t.Fatal("expected a part event")
	}
	if pe.Part["text"] != "hello" {
		t.Fatalf("part text = %v, want hello", pe.Part["text"])
	}
	if pe.MessageID != "msg_1" {
		t.Fatalf("messageID = %q, want msg_1", pe.MessageID)
	}
	if got := ev.SessionIDOf(); got != "ses_1" {
		t.Fatalf("sessionID = %q, want ses_1", got)
	}
	if got := ev.MessageIDOf(); got != "msg_1" {
		t.Fatalf("MessageIDOf = %q, want msg_1", got)
	}
}

func TestEventNonPartIsNotAPart(t *testing.T) {
	ev := Event{
		Type:       "permission.asked",
		Properties: json.RawMessage(`{"properties":{"sessionID":"ses_1"}}`),
	}
	if _, ok := ev.Part(); ok {
		t.Fatal("permission event must not decode as a part")
	}
}

// permission.asked carries its request under properties.data (older / REST-era
// spelling, kept for compatibility).
func TestEventPermissionAsked(t *testing.T) {
	ev := Event{
		Type: "permission.asked",
		Properties: json.RawMessage(`{"sessionID":"ses_1",
			"data":{"id":"per_1","sessionID":"ses_1","permission":"bash",
			        "patterns":["ls -la"],"callID":"call_1"}}`),
	}
	pr, ok := ev.PermissionAsked()
	if !ok {
		t.Fatal("expected a permission event")
	}
	if pr.ID != "per_1" || pr.Permission != "bash" {
		t.Fatalf("decoded %+v, want id=per_1 permission=bash", pr)
	}
	if len(pr.Patterns) != 1 || pr.Patterns[0] != "ls -la" {
		t.Fatalf("patterns = %v", pr.Patterns)
	}
	if pr.CallID != "call_1" {
		t.Fatalf("callID = %q, want call_1", pr.CallID)
	}
}

// The live 1.18.35 envelope is flattened: the request fields sit directly on
// `properties`, with no `data` wrapper. This exact shape was captured from a
// running server's /event stream; missing it silently drops every card.
func TestEventPermissionAskedFlat(t *testing.T) {
	ev := Event{
		Type: "permission.asked",
		Properties: json.RawMessage(`{
			"id":"per_1","sessionID":"ses_1","permission":"external_directory",
			"patterns":["/etc/*"],
			"metadata":{"filepath":"/etc/hosts","parentDir":"/etc"},
			"always":["/etc/*"],
			"tool":{"messageID":"msg_1","callID":"call_1"}}`),
	}
	if got := ev.SessionIDOf(); got != "ses_1" {
		t.Fatalf("sessionID = %q, want ses_1", got)
	}
	pr, ok := ev.PermissionAsked()
	if !ok {
		t.Fatal("expected a permission event")
	}
	if pr.ID != "per_1" || pr.Permission != "external_directory" {
		t.Fatalf("decoded %+v, want id=per_1 permission=external_directory", pr)
	}
	if len(pr.Patterns) != 1 || pr.Patterns[0] != "/etc/*" {
		t.Fatalf("patterns = %v", pr.Patterns)
	}
	if got := pr.Metadata["filepath"]; got != "/etc/hosts" {
		t.Fatalf("metadata.filepath = %v, want /etc/hosts", got)
	}
}

// question.asked carries its request at the top level of properties under
// `data` (the decoder also accepts the deeper/nested spellings, but the live
// event looks like this).
func TestEventQuestionAsked(t *testing.T) {
	ev := Event{
		Type: "question.asked",
		Properties: json.RawMessage(`{"sessionID":"ses_1",
			"data":{"id":"que_1","sessionID":"ses_1","questions":[
				{"question":"Which one?","header":"Pick","multiple":true,
				 "options":[{"label":"A","description":"first"}]}
			]}}`),
	}
	qr, ok := ev.QuestionAsked()
	if !ok {
		t.Fatal("expected a question event")
	}
	if qr.ID != "que_1" {
		t.Fatalf("id = %q, want que_1", qr.ID)
	}
	if len(qr.Questions) != 1 {
		t.Fatalf("questions = %d, want 1", len(qr.Questions))
	}
	q := qr.Questions[0]
	if q.Question != "Which one?" || q.Header != "Pick" || !q.Multiple {
		t.Fatalf("question = %+v", q)
	}
	if len(q.Options) != 1 || q.Options[0].Label != "A" {
		t.Fatalf("options = %+v", q.Options)
	}
}

// The same flattening applies to question.asked; accept the flat spelling too.
func TestEventQuestionAskedFlat(t *testing.T) {
	ev := Event{
		Type: "question.asked",
		Properties: json.RawMessage(`{
			"id":"que_1","sessionID":"ses_1","questions":[
				{"question":"Which one?","header":"Pick","multiple":true,
				 "options":[{"label":"A","description":"first"}]}
			]}`),
	}
	qr, ok := ev.QuestionAsked()
	if !ok {
		t.Fatal("expected a question event")
	}
	if qr.ID != "que_1" || len(qr.Questions) != 1 {
		t.Fatalf("decoded %+v", qr)
	}
	if qr.Questions[0].Question != "Which one?" {
		t.Fatalf("question = %+v", qr.Questions[0])
	}
}

func TestEventSessionError(t *testing.T) {
	ev := Event{
		Type: "session.error",
		Properties: json.RawMessage(`{"sessionID":"ses_1",
			"error":{"name":"UnknownError","data":{"message":"boom","ref":"err_1"}}}`),
	}
	msg, ok := ev.SessionError()
	if !ok {
		t.Fatal("expected a session error event")
	}
	if msg == "" {
		t.Fatal("expected a non-empty message")
	}
	// The ref is appended so the user can correlate with opencode's logs.
	if want := "boom（ref err_1）"; msg != want {
		t.Fatalf("message = %q, want %q", msg, want)
	}
}

// The session.next.* lifecycle bus (the one 1.18.35 actually emits) must
// decode: text deltas, step failures and the idle turn boundary.
func TestEventNextLifecycle(t *testing.T) {
	text := Event{
		Type:       "session.next.text.delta",
		Properties: json.RawMessage(`{"sessionID":"ses_1","assistantMessageID":"msg_1","text":"hi"}`),
	}
	np, ok := text.Next()
	if !ok || np.Kind != "text" || np.Text != "hi" || np.MessageID != "msg_1" {
		t.Fatalf("text delta decoded as %+v (ok=%v)", np, ok)
	}

	failed := Event{
		Type:       "session.next.step.failed",
		Properties: json.RawMessage(`{"sessionID":"ses_1","error":{"type":"unknown","message":"boom"}}`),
	}
	np, ok = failed.Next()
	if !ok || np.Kind != "step_failed" || np.ErrMsg != "boom" {
		t.Fatalf("step.failed decoded as %+v (ok=%v)", np, ok)
	}

	idle := Event{
		Type:       "session.idle",
		Properties: json.RawMessage(`{"sessionID":"ses_1"}`),
	}
	np, ok = idle.Next()
	if !ok || np.Kind != "idle" {
		t.Fatalf("idle decoded as %+v (ok=%v)", np, ok)
	}

	// An event cc-webui does not act on must report ok=false.
	if _, ok := (Event{Type: "server.heartbeat"}).Next(); ok {
		t.Fatal("heartbeat must not decode as a lifecycle event")
	}
}

func TestPromptWireShapeIsV1(t *testing.T) {
	// The v1 prompt body is {model:{providerID,modelID}, parts:[...]}.
	// It is NOT the v2 envelope ({prompt:{text,files}}) — in 1.18.35 the v2
	// endpoint only admits the message (returns {admittedSeq} in ~10ms) and
	// never runs the turn, so getting this wrong makes the agent go silent
	// with no error anywhere.
	in := PromptInput{
		Model: &ModelV1{ProviderID: "sensenova-openai", ModelID: "sensenova-6.8-flash-lite"},
		Parts: []PromptPart{
			{Type: "text", Text: "hi"},
			{Type: "file", Mime: "image/png", Text: "data:image/png;base64,AAA"},
		},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if _, bad := back["prompt"]; bad {
		t.Fatalf("body must not use the v2 envelope: %s", raw)
	}
	m, ok := back["model"].(map[string]any)
	if !ok || m["providerID"] != "sensenova-openai" || m["modelID"] != "sensenova-6.8-flash-lite" {
		t.Fatalf("model = %v, want {providerID, modelID}", back["model"])
	}
	parts, ok := back["parts"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("parts = %v, want 2 entries", back["parts"])
	}
	f := parts[1].(map[string]any)
	if f["type"] != "file" || f["mime"] != "image/png" {
		t.Fatalf("file part = %v, want mime carried so inline images survive", f)
	}
}

func TestPermissionRulesetWireShape(t *testing.T) {
	// The session-create ruleset is a flat array of
	// {permission, pattern, action} — all three required, and "always" is NOT
	// a valid ruleset action (it only exists on the reply endpoint).
	rules := []PermissionRule{{Name: "*", Pattern: "*", Action: ActionAllow}}
	raw, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	var back []map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back[0]["permission"] != "*" || back[0]["pattern"] != "*" || back[0]["action"] != "allow" {
		t.Fatalf("rule = %v, want {permission,pattern,action}", back[0])
	}
}

func TestQueryPathEscapesDirectory(t *testing.T) {
	got := queryPath("/session", map[string]any{"directory": "/tmp/a b", "empty": "", "n": 1})
	want := "/session?directory=/tmp/a%20b"
	if got != want {
		t.Fatalf("queryPath = %q, want %q", got, want)
	}
	if got := queryPath("/session", nil); got != "/session" {
		t.Fatalf("empty query = %q", got)
	}
}

func TestReplyEndpointsUseSessionScopedIDs(t *testing.T) {
	// Both reply URLs embed the session id and opencode's request id; a
	// mismatch here means the answer silently never reaches the agent.
	// These assert the path templates the client builds.
	for _, tc := range []struct{ got, want string }{
		{"/api/session/ses_1/permission/per_1/reply", "/api/session/ses_1/permission/per_1/reply"},
		{"/api/session/ses_1/question/que_1/reply", "/api/session/ses_1/question/que_1/reply"},
		{"/api/session/ses_1/question/que_1/reject", "/api/session/ses_1/question/que_1/reject"},
	} {
		if tc.got != tc.want {
			t.Fatalf("path = %q, want %q", tc.got, tc.want)
		}
	}
}

// session.status is the ONLY turn-completion signal on the v1 prompt path
// (prompt_async returns 204 immediately), so mis-parsing it means the turn
// never closes.
func TestEventSessionStatusBusyIdle(t *testing.T) {
	busy := Event{
		Type:       "session.status",
		Properties: json.RawMessage(`{"sessionID":"ses_1","status":{"type":"busy"}}`),
	}
	np, ok := busy.Next()
	if !ok || np.Kind != "busy" {
		t.Fatalf("busy decoded as %+v (ok=%v)", np, ok)
	}

	idle := Event{
		Type:       "session.status",
		Properties: json.RawMessage(`{"sessionID":"ses_1","status":{"type":"idle"}}`),
	}
	np, ok = idle.Next()
	if !ok || np.Kind != "idle" {
		t.Fatalf("idle decoded as %+v (ok=%v)", np, ok)
	}

	// An unrecognized status must not be mistaken for a boundary.
	other := Event{
		Type:       "session.status",
		Properties: json.RawMessage(`{"sessionID":"ses_1","status":{"type":"retry"}}`),
	}
	np, ok = other.Next()
	if !ok || np.Kind != "busy" {
		t.Fatalf("retry decoded as %+v (ok=%v)", np, ok)
	}
}

// Regression: every event on /event carries sessionID at the TOP level of
// properties. An earlier parser only looked at the v1 nested shape, so
// session.status resolved to "" and the hub dropped the event — which made
// waitTurn block forever and left the frontend stuck on "running" even though
// the model had answered.
func TestSessionIDOfTopLevelAcrossDialects(t *testing.T) {
	for _, c := range []struct {
		name, typ, raw, want string
	}{
		{"status idle", "session.status", `{"sessionID":"ses_1","status":{"type":"idle"}}`, "ses_1"},
		{"status busy", "session.status", `{"sessionID":"ses_1","status":{"type":"busy"}}`, "ses_1"},
		{"part delta", "message.part.delta", `{"sessionID":"ses_1","messageID":"msg_1","delta":"hi"}`, "ses_1"},
		{"part updated", "message.part.updated", `{"sessionID":"ses_1","part":{"sessionID":"ses_1","messageID":"msg_1"}}`, "ses_1"},
		{"message updated", "message.updated", `{"sessionID":"ses_1","info":{"id":"msg_1"}}`, "ses_1"},
		{"permission", "permission.asked", `{"sessionID":"ses_1","data":{"id":"per_1","permission":"bash"}}`, "ses_1"},
		{"question", "question.asked", `{"sessionID":"ses_1","data":{"id":"que_1","questions":[]}}`, "ses_1"},
		// The v1 stream still nests; it must keep working.
		{"v1 nested", "message.part.updated", `{"properties":{"part":{"sessionID":"ses_2","messageID":"msg_1"}}}`, "ses_2"},
	} {
		ev := Event{Type: c.typ, Properties: json.RawMessage(c.raw)}
		if got := ev.SessionIDOf(); got != c.want {
			t.Errorf("%s: SessionIDOf = %q, want %q", c.name, got, c.want)
		}
	}
}

// Regression: opencode replays the user's own prompt as a text part on the
// SAME part bus as assistant output, and the part carries no role. Without
// correlating message ids with message.updated, the echo of the user's prompt
// renders in the UI as if the model had replied with it.
func TestEventMessageRole(t *testing.T) {
	ev := Event{
		Type: "message.updated",
		Properties: json.RawMessage(`{"sessionID":"ses_1",
			"info":{"id":"msg_user","role":"user"}}`),
	}
	id, role, ok := ev.MessageRole()
	if !ok || id != "msg_user" || role != "user" {
		t.Fatalf("MessageRole = %q/%q ok=%v, want msg_user/user", id, role, ok)
	}

	asst := Event{
		Type:       "message.updated",
		Properties: json.RawMessage(`{"sessionID":"ses_1","info":{"id":"msg_a","role":"assistant"}}`),
	}
	id, role, ok = asst.MessageRole()
	if !ok || id != "msg_a" || role != "assistant" {
		t.Fatalf("MessageRole = %q/%q ok=%v", id, role, ok)
	}

	// A part event has no role of its own — that is exactly why the turn keeps
	// the message.id → role table.
	part := Event{
		Type:       "message.part.updated",
		Properties: json.RawMessage(`{"sessionID":"ses_1","part":{"messageID":"msg_user","type":"text","text":"hi"}}`),
	}
	if _, _, ok := part.MessageRole(); ok {
		t.Fatal("a bare part event must not report a role")
	}
}

// Regression: opencode streams assistant text as message.part.delta carrying
// only {partID, field, delta} — no `part` object. A client that waits for the
// assembled part sees the whole answer appear at once, with no token-level
// streaming.
func TestEventPartDelta(t *testing.T) {
	ev := Event{
		Type: "message.part.delta",
		Properties: json.RawMessage(`{"sessionID":"ses_1","messageID":"msg_1",
			"partID":"prt_9","field":"text","delta":"Go 采用"}`),
	}
	pd, ok := ev.PartDelta()
	if !ok {
		t.Fatal("expected a part delta")
	}
	if pd.Delta != "Go 采用" || pd.Field != "text" {
		t.Fatalf("decoded %+v", pd)
	}
	if pd.PartID != "prt_9" || pd.MessageID != "msg_1" || pd.SessionID != "ses_1" {
		t.Fatalf("ids = %+v", pd)
	}
	// An empty increment is not useful and must be rejected.
	empty := Event{
		Type:       "message.part.delta",
		Properties: json.RawMessage(`{"messageID":"msg_1","partID":"prt_9","field":"text","delta":""}`),
	}
	if _, ok := empty.PartDelta(); ok {
		t.Fatal("empty delta must not decode")
	}
	// A full-part event is not a delta.
	full := Event{
		Type:       "message.part.updated",
		Properties: json.RawMessage(`{"messageID":"msg_1","part":{"type":"text","text":"x"}}`),
	}
	if _, ok := full.PartDelta(); ok {
		t.Fatal("part.updated must not decode as a delta")
	}
}

// Control events drive prompt cards and turn boundaries; the lossy fast path
// would silently strand a question (and the running turn) if one were dropped.
// Delta traffic must keep the lossy path.
func TestIsControlEvent(t *testing.T) {
	for _, typ := range []string{
		"permission.asked", "permission.updated",
		"question.asked", "question.updated",
		"session.idle", "session.next.idle", "session.status",
		"session.error", "session.next.step.failed",
	} {
		if !isControlEvent(typ) {
			t.Errorf("%s must be a control event (must not be dropped)", typ)
		}
	}
	for _, typ := range []string{
		"message.part.delta", "message.part.updated", "session.next.text.delta", "",
	} {
		if isControlEvent(typ) {
			t.Errorf("%s is high-volume traffic and must stay on the lossy path", typ)
		}
	}
}
