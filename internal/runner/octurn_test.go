package runner

import (
	"testing"

	"cc-webui/internal/config"
	"cc-webui/internal/opencodeapi"
)

// The opencode engine now runs on `opencode serve`, so its permission mode and
// question/answer mapping are pure functions worth pinning: both decide
// whether the user gets an approval card at all.

func TestOpenCodePermissionRuleset(t *testing.T) {
	// bypassPermissions means "never interrupt me": everything allowed, so no
	// card is ever raised.
	rs := openCodePermissionRuleset("bypassPermissions")
	if len(rs) != 1 {
		t.Fatalf("bypassPermissions rules = %+v, want one catch-all rule", rs)
	}
	if rs[0].Name != "*" || rs[0].Pattern != "*" || rs[0].Action != "allow" {
		t.Fatalf("rule = %+v, want {*, *, allow}", rs[0])
	}
	// Every other mode keeps opencode's own default (ask), which is exactly
	// what the card bridge consumes. Pinning anything else here would silently
	// allow or deny tools cc-webui does not know about.
	for _, mode := range []string{"", "default", "acceptEdits", "plan", "dontAsk"} {
		if got := openCodePermissionRuleset(mode); got != nil {
			t.Errorf("mode %q rules = %+v, want nil (keep opencode's ask default)", mode, got)
		}
	}
}

func TestMapBehavior(t *testing.T) {
	// "allow_tool_session" has no direct opencode equivalent: "once" approves
	// this call, and the session allowance is enforced cc-webui-side by
	// short-circuiting before the card is raised.
	for in, want := range map[string]string{
		"allow":              "once",
		"allow_tool_session": "once",
		"deny":               "reject",
		"anything else":      "reject",
	} {
		if got := mapBehavior(in); got != want {
			t.Errorf("mapBehavior(%q) = %q, want %q", in, got, want)
		}
	}
}

// opencode's question tool and Claude's AskUserQuestion must produce the same
// card input, otherwise the frontend needs an engine branch it does not have.
func TestOpenCodeQuestionsToClaude(t *testing.T) {
	got := openCodeQuestionsToClaude([]opencodeapi.QuestionPrompt{
		{
			Question: "Which database?",
			Header:   "DB",
			Multiple: true,
			Options: []opencodeapi.QuestionOption{
				{Label: "postgres", Description: "primary store"},
				{Label: "sqlite"},
			},
		},
	})
	if len(got) != 1 {
		t.Fatalf("questions = %d, want 1", len(got))
	}
	q := got[0]
	if q["question"] != "Which database?" {
		t.Errorf("question = %v", q["question"])
	}
	if q["header"] != "DB" {
		t.Errorf("header = %v, want DB", q["header"])
	}
	// opencode's `multiple` maps onto Claude's `multiSelect`, the flag the
	// existing card renders as checkboxes.
	if q["multiSelect"] != true {
		t.Errorf("multiSelect = %v, want true", q["multiSelect"])
	}
	opts, ok := q["options"].([]map[string]any)
	if !ok || len(opts) != 2 {
		t.Fatalf("options = %v", q["options"])
	}
	if opts[0]["label"] != "postgres" || opts[0]["description"] != "primary store" {
		t.Errorf("options[0] = %v", opts[0])
	}
}

// The frontend answers with {question text: comma-joined labels}; opencode
// wants one label array per question, in question order.
func TestOpenCodeAnswersFromClaude(t *testing.T) {
	questions := []opencodeapi.QuestionPrompt{
		{Question: "Pick one"},
		{Question: "Pick many", Multiple: true},
	}
	answers := map[string]string{
		"Pick one":  "postgres",
		"Pick many": "a，b",
	}
	got := openCodeAnswersFromClaude(answers, questions)
	if len(got) != 2 {
		t.Fatalf("answers = %+v, want 2 entries", got)
	}
	if len(got[0]) != 1 || got[0][0] != "postgres" {
		t.Errorf("answers[0] = %v, want [postgres]", got[0])
	}
	// Both the full-width and ASCII comma must split, since the card joins
	// multi-selects with either depending on the render path.
	if len(got[1]) != 2 || got[1][0] != "a" || got[1][1] != "b" {
		t.Errorf("answers[1] = %v, want [a b]", got[1])
	}
	// An unanswered question must still occupy its slot: opencode indexes the
	// array by question position.
	got = openCodeAnswersFromClaude(map[string]string{"Pick one": "x"}, questions)
	if len(got) != 2 || len(got[1]) != 0 {
		t.Errorf("partial answers = %+v, want second entry empty", got)
	}
	if got := openCodeAnswersFromClaude(nil, questions); got != nil {
		t.Errorf("no answers = %+v, want nil (caller then rejects the question)", got)
	}
}

func TestSplitAnswerLabels(t *testing.T) {
	for in, want := range map[string][]string{
		"a":       {"a"},
		"a,b":     {"a", "b"},
		"a，b":     {"a", "b"},
		"a, b ,c": {"a", "b", "c"},
		"a\nb":    {"a", "b"},
		"a,,b":    {"a", "b"},
		",":       {},
	} {
		got := splitAnswerLabels(in)
		if len(got) != len(want) {
			t.Errorf("splitAnswerLabels(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("splitAnswerLabels(%q)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}

// The card payload must carry the tool's real arguments when opencode hands
// them over — that is what makes an approval card informative instead of just
// naming a tool.
func TestPermissionCardInput(t *testing.T) {
	withTool := permissionCardInput(opencodeapi.PermissionRequest{
		Permission: "bash",
		Patterns:   []string{"ls -la"},
		Tool: map[string]any{
			"state": map[string]any{
				"input": map[string]any{"command": "ls -la"},
			},
		},
	})
	if withTool["command"] != "ls -la" {
		t.Errorf("input = %v, want the real tool arguments", withTool)
	}

	// No tool payload: fall back to the permission's patterns so the card can
	// still explain what was requested.
	fallback := permissionCardInput(opencodeapi.PermissionRequest{
		Permission: "bash",
		Patterns:   []string{"rm -rf /"},
	})
	pat, ok := fallback["patterns"].([]string)
	if !ok || len(pat) != 1 || pat[0] != "rm -rf /" {
		t.Errorf("fallback input = %v, want the permission patterns", fallback)
	}

	// Nothing at all: an empty payload is still a valid (if terse) card.
	if got := permissionCardInput(opencodeapi.PermissionRequest{Permission: "edit"}); len(got) != 0 {
		t.Errorf("empty input = %v, want empty map", got)
	}
}

// opencode binds model + variant at session-creation time, so a hot-switched
// profile has to be pushed explicitly or the UI silently lies about which
// model is answering.
func TestOpenCodeModelRef(t *testing.T) {
	prov := &config.Provider{Name: "sensenova"}
	if got := openCodeModelRef(prov, "", "max"); got != nil {
		t.Errorf("empty model = %+v, want nil (leave the session's own model)", got)
	}
	got := openCodeModelRef(prov, "m1", "")
	if got.ID != "m1" || got.ProviderID != "sensenova" || got.Variant != "" {
		t.Errorf("ref = %+v, want m1/sensenova with no variant", got)
	}
	got = openCodeModelRef(prov, "m1", "max")
	if got.Variant != "max" {
		t.Errorf("variant = %q, want max", got.Variant)
	}
}

func TestSameModelRef(t *testing.T) {
	base := opencodeapi.ModelRef{ID: "m1", ProviderID: "p", Variant: "high"}
	same := []opencodeapi.ModelRef{
		{ID: "m1", ProviderID: "p", Variant: "high"},
	}
	for _, s := range same {
		if !sameModelRef(base, s) {
			t.Errorf("expected %+v == %+v", base, s)
		}
	}
	// Every field is load-bearing: a changed effort must count as a change,
	// or dragging the effort dropdown would never reach opencode.
	diff := []opencodeapi.ModelRef{
		{ID: "m2", ProviderID: "p", Variant: "high"},
		{ID: "m1", ProviderID: "q", Variant: "high"},
		{ID: "m1", ProviderID: "p", Variant: "low"},
		{},
	}
	for _, d := range diff {
		if sameModelRef(base, d) {
			t.Errorf("expected %+v != %+v", base, d)
		}
	}
}

// Regression: opencode streams the user's own prompt back as a text part on
// the same bus as assistant output. Rendering it makes the UI echo the
// question back as the model's reply — the part carries no role, so the turn
// must correlate message ids with message.updated.
func TestOpenCodeTurnSkipsUserEchoParts(t *testing.T) {
	tu := &openCodeTurn{msgRoles: map[string]string{}}

	// message.updated lands first, declaring the id to be the user's.
	tu.mu.Lock()
	tu.msgRoles["msg_user"] = "user"
	tu.msgRoles["msg_asst"] = "assistant"
	tu.mu.Unlock()

	if !tu.isUserMessage("msg_user") {
		t.Error("msg_user should be recognised as a user message")
	}
	if tu.isUserMessage("msg_asst") {
		t.Error("msg_asst must not be filtered as a user message")
	}
	// Unknown ids must pass through: a missed message.updated must not silently
	// swallow the model's own output.
	if tu.isUserMessage("msg_unknown") {
		t.Error("unknown message id must not be treated as user")
	}
	if tu.isUserMessage("") {
		t.Error("empty id must not be treated as user")
	}
}

// Regression: a reasoning part streams with the same field:"text" as the
// answer, so the field cannot separate them — only the part announcement can.
// Forwarding those deltas shows the model's private thinking in the chat as if
// it were the reply, while AdaptPart drops it once the full part lands, so the
// transcript would also change under a reload.
//
// The event order below is what opencode 1.18.35 actually emits (captured from
// a live `opencode serve`): the part is announced empty, its deltas stream, then
// the assembled part lands.
func TestOpenCodeTurnSkipsReasoningDeltas(t *testing.T) {
	tu := &openCodeTurn{
		msgRoles:  map[string]string{"msg_asst": "assistant", "msg_user": "user"},
		partTypes: map[string]string{"prt_reason": "reasoning", "prt_text": "text"},
	}

	// The reasoning deltas: same field, same message, different part.
	for _, d := range []string{"Let me", " think", " about it"} {
		pd := opencodeapi.PartDelta{
			MessageID: "msg_asst", PartID: "prt_reason", Field: "text", Delta: d,
		}
		if tu.streamable(pd) {
			t.Errorf("reasoning delta %q must not stream", d)
		}
	}
	// The answer's deltas still stream — the fix must not cost token rendering.
	if !tu.streamable(opencodeapi.PartDelta{
		MessageID: "msg_asst", PartID: "prt_text", Field: "text", Delta: "1026",
	}) {
		t.Error("text delta must stream")
	}
	// A part never announced is not provably an answer, so it does not stream;
	// its assembled part still arrives and renders whole.
	if tu.streamable(opencodeapi.PartDelta{
		MessageID: "msg_asst", PartID: "prt_unknown", Field: "text", Delta: "?",
	}) {
		t.Error("delta for an unannounced part must not stream")
	}
	// The echoed user prompt stays filtered, whatever its part type.
	if tu.streamable(opencodeapi.PartDelta{
		MessageID: "msg_user", PartID: "prt_text", Field: "text", Delta: "hi",
	}) {
		t.Error("user echo delta must not stream")
	}
	// Tool parts carry no deltas today, but a future field must not slip past.
	if tu.streamable(opencodeapi.PartDelta{
		MessageID: "msg_asst", PartID: "prt_text", Field: "input", Delta: "{}",
	}) {
		t.Error("non-text field must not stream")
	}
	if tu.streamable(opencodeapi.PartDelta{
		MessageID: "", PartID: "prt_text", Field: "text", Delta: "x",
	}) {
		t.Error("delta without a message id must not stream")
	}
}

// The SSE bridge and the reconcile poller both observe the same opencode
// requests. Only one of them may open a card / reply: a duplicate question card
// answers the same request twice (the second POST 404s), and duplicate
// permission cards double-dock the same approval.
func TestOpenCodeTurnClaimPromptDedupes(t *testing.T) {
	tu := &openCodeTurn{seenPrompts: map[string]bool{}}
	if !tu.claimPrompt("q", "que_1") {
		t.Fatal("first claim of a question must win")
	}
	if tu.claimPrompt("q", "que_1") {
		t.Fatal("second claim of the same question must lose")
	}
	if !tu.claimPrompt("p", "que_1") {
		t.Fatal("question and permission ids are separate namespaces")
	}
	// Distinct request ids are independent.
	if !tu.claimPrompt("q", "que_2") {
		t.Fatal("a different question id must be claimable")
	}
	if !tu.claimPrompt("p", "per_1") {
		t.Fatal("permissions are tracked separately from questions")
	}
	if tu.claimPrompt("q", "") {
		t.Fatal("an empty id cannot identify a request")
	}
	if !tu.promptClaimed("p", "per_1") {
		t.Fatal("promptClaimed must reflect a prior claim")
	}
	if tu.promptClaimed("p", "missing") {
		t.Fatal("unknown ids are not claimed")
	}
}

// A stop that opencode never acknowledges must still release waitTurn, with the
// failure recorded so the UI shows an error rather than a clean interrupt.
func TestOpenCodeTurnForceStop(t *testing.T) {
	tu := newOpenCodeTurn(nil, nil, nil, "s")
	select {
	case <-tu.stopCh:
		t.Fatal("stopCh must start open")
	default:
	}
	tu.forceStop("boom")
	select {
	case <-tu.stopCh:
	default:
		t.Fatal("forceStop must close stopCh")
	}
	if got := tu.abortFailure(); got != "boom" {
		t.Fatalf("abortFailure = %q, want boom", got)
	}
	if !tu.isInterrupted() {
		t.Fatal("forceStop must mark the turn interrupted")
	}
	// Closing twice must not panic (abort success + grace timeout can race).
	tu.forceStop("again")
}
