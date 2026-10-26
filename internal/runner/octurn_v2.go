package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"cc-webui/internal/config"
	"cc-webui/internal/opencodev2"
	"cc-webui/internal/store"
)

// openCodeTurnV2 drives one turn over OpenCode 2's server API.
//
// The shape differs from v1 in three ways that matter here:
//   - turns are bounded by session.execution.succeeded/failed/interrupted
//     (v1 used a session.status idle event);
//   - text and reasoning are separate event namespaces, so the v1 "which part
//     is the answer" ambiguity disappears;
//   - questions are forms (form.created) and permissions are session-scoped
//     (permission.asked), both answered on session-scoped routes.
type openCodeTurnV2 struct {
	srv *opencodev2.Server
	mgr *Manager

	sessionID   string
	ocSessionID string
	dir         string

	mu sync.Mutex
	// claimed dedupes the SSE bridge and the reconcile poller by request id.
	claimed     map[string]bool
	sessionErr  string
	interrupted bool

	// lastUserMessageID is the v2 user message id of this turn (prompt result);
	// it doubles as the rewind checkpoint.
	lastUserMessageID string

	useSent    map[string]bool
	resultSent map[string]bool

	ctx      context.Context
	drained  chan struct{}
	drainedO sync.Once

	doneCh  chan struct{}
	doneO   sync.Once
	turnCh  chan struct{} // closed when the execution terminal event lands
	turnO   sync.Once
	stopCh  chan struct{}
	stopO   sync.Once
	lastErr string
}

func newOpenCodeTurnV2(srv *opencodev2.Server, m *Manager, sessionID string) *openCodeTurnV2 {
	return &openCodeTurnV2{
		srv: srv, mgr: m, sessionID: sessionID,
		claimed:    map[string]bool{},
		useSent:    map[string]bool{},
		resultSent: map[string]bool{},
		drained:    make(chan struct{}),
		doneCh:     make(chan struct{}),
		turnCh:     make(chan struct{}),
		stopCh:     make(chan struct{}),
	}
}

// ---- event payloads ----

type v2TextDelta struct {
	AssistantMessageID string `json:"assistantMessageID"`
	Ordinal            int    `json:"ordinal"`
	Delta              string `json:"delta"`
}

type v2TextEnded struct {
	AssistantMessageID string `json:"assistantMessageID"`
	Ordinal            int    `json:"ordinal"`
	Text               string `json:"text"`
}

type v2ToolCalled struct {
	AssistantMessageID string         `json:"assistantMessageID"`
	ID                 string         `json:"id"`
	Name               string         `json:"name"`
	Input              map[string]any `json:"input"`
	Executed           bool           `json:"executed"`
}

type v2ToolDone struct {
	AssistantMessageID string `json:"assistantMessageID"`
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Text               string `json:"text"`
	Content            []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error    any            `json:"error"`
	Metadata map[string]any `json:"metadata"`
}

type v2Tokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

type v2StepEnded struct {
	AssistantMessageID string    `json:"assistantMessageID"`
	Cost               float64   `json:"cost"`
	Tokens             *v2Tokens `json:"tokens"`
}

type v2ExecFailed struct {
	SessionID string `json:"sessionID"`
	Error     any    `json:"error"`
	Message   string `json:"message"`
}

// consume adapts the session's event slice into canonical stream events.
func (t *openCodeTurnV2) consume(events <-chan opencodev2.Event) {
	defer t.markDrained()
	for ev := range events {
		switch ev.Type {
		case "session.text.delta":
			var d v2TextDelta
			if ev.Decode(&d) != nil || d.Delta == "" {
				continue
			}
			t.mgr.emit(t.sessionID, "stream", map[string]any{
				"type":      "oc_text_delta",
				"messageID": d.AssistantMessageID,
				"partID":    textPartID(d.AssistantMessageID, d.Ordinal),
				"delta":     d.Delta,
			})
		case "session.text.ended":
			var d v2TextEnded
			if ev.Decode(&d) != nil || d.Text == "" {
				continue
			}
			t.emitAssistant(d.AssistantMessageID, map[string]any{
				"type": "text", "text": d.Text, "id": textPartID(d.AssistantMessageID, d.Ordinal),
			})
		case "session.reasoning.ended":
			var d v2TextEnded
			if ev.Decode(&d) != nil || strings.TrimSpace(d.Text) == "" {
				continue
			}
			t.emitAssistant(d.AssistantMessageID, map[string]any{
				"type": "thinking", "thinking": d.Text,
				"id": fmt.Sprintf("%s:reasoning-%d", d.AssistantMessageID, d.Ordinal),
			})
		case "session.tool.called":
			var d v2ToolCalled
			if ev.Decode(&d) != nil {
				continue
			}
			t.emitToolUse(d.AssistantMessageID, d.ID, d.Name, d.Input)
		case "session.tool.success":
			var d v2ToolDone
			if ev.Decode(&d) != nil {
				continue
			}
			t.emitToolResult(d, false)
		case "session.tool.failed":
			var d v2ToolDone
			if ev.Decode(&d) != nil {
				continue
			}
			t.emitToolResult(d, true)
		case "session.step.ended":
			var d v2StepEnded
			if ev.Decode(&d) != nil || d.Tokens == nil {
				continue
			}
			t.emitUsage(d.Tokens)
		case "session.execution.succeeded":
			t.markTurnDone()
		case "session.execution.failed":
			var d v2ExecFailed
			_ = ev.Decode(&d)
			t.recordError(v2ErrorText(d.Error, d.Message))
			t.markTurnDone()
		case "session.execution.interrupted":
			t.markInterrupted()
			t.markTurnDone()
		case "session.error":
			var d v2ExecFailed
			_ = ev.Decode(&d)
			t.recordError(v2ErrorText(d.Error, d.Message))
		case "permission.asked":
			var pr opencodev2.PermissionRequest
			if ev.Decode(&pr) == nil {
				go t.openPermissionCard(pr)
			}
		case "form.created":
			var d struct {
				Form opencodev2.FormInfo `json:"form"`
			}
			if ev.Decode(&d) == nil {
				go t.openFormCard(d.Form)
			}
		}
	}
}

func textPartID(messageID string, ordinal int) string {
	return fmt.Sprintf("%s:text-%d", messageID, ordinal)
}

func (t *openCodeTurnV2) emitAssistant(messageID string, block map[string]any) {
	msg := map[string]any{"role": "assistant", "content": []any{block}}
	if messageID != "" {
		msg["id"] = messageID
	}
	t.mgr.emit(t.sessionID, "stream", map[string]any{"type": "assistant", "message": msg})
}

// emitUsage carries the step's token counts on a content-less assistant event
// (the same convention mcode/codex use). It deliberately has no message id:
// the step's text block already carries one, and the trajectory projection
// merges by id — an id-less carry event is skipped there, while the stats path
// still reads message.usage.
func (t *openCodeTurnV2) emitUsage(tok *v2Tokens) {
	usage := map[string]any{
		"input_tokens":                tok.Input,
		"output_tokens":               tok.Output,
		"reasoning_tokens":            tok.Reasoning,
		"cache_read_input_tokens":     tok.Cache.Read,
		"cache_creation_input_tokens": tok.Cache.Write,
	}
	t.mgr.emit(t.sessionID, "stream", map[string]any{
		"type":    "assistant",
		"message": map[string]any{"role": "assistant", "content": []any{}, "usage": usage},
	})
}

// toolDisplayName maps v2 tool ids onto the names the frontend's tool views
// recognize (v2 calls bash "shell").
func toolDisplayName(name string) string {
	switch strings.ToLower(name) {
	case "shell":
		return "Bash"
	case "":
		return "tool"
	}
	return name
}

func (t *openCodeTurnV2) emitToolUse(messageID, callID, name string, input map[string]any) {
	if callID == "" || t.useSent[callID] {
		return
	}
	if len(input) == 0 {
		// v2 may announce the call before the arguments land; wait for them.
		return
	}
	t.useSent[callID] = true
	block := map[string]any{
		"type": "tool_use", "id": callID,
		"name": toolDisplayName(name), "input": input,
	}
	t.emitAssistant(messageID, block)
}

func (t *openCodeTurnV2) emitToolResult(d v2ToolDone, failed bool) {
	if d.ID == "" || t.resultSent[d.ID] {
		return
	}
	t.resultSent[d.ID] = true
	// A result must never appear without its card: if the `called` event was
	// missed (or carried no input), synthesize the tool_use here.
	if !t.useSent[d.ID] {
		t.useSent[d.ID] = true
		t.emitAssistant(d.AssistantMessageID, map[string]any{
			"type": "tool_use", "id": d.ID,
			"name": toolDisplayName(d.Name), "input": map[string]any{},
		})
	}
	var texts []string
	for _, c := range d.Content {
		if c.Type == "text" && c.Text != "" {
			texts = append(texts, c.Text)
		}
	}
	if d.Text != "" {
		texts = append(texts, d.Text)
	}
	if len(texts) == 0 && failed {
		texts = append(texts, v2ErrorText(d.Error, ""))
	}
	content := []any{}
	if s := strings.Join(texts, "\n"); s != "" {
		content = append(content, map[string]any{"type": "text", "text": s})
	}
	block := map[string]any{"type": "tool_result", "tool_use_id": d.ID, "content": content}
	if failed {
		block["is_error"] = true
	}
	t.mgr.emit(t.sessionID, "stream", map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": []any{block}},
	})
}

func v2ErrorText(errVal any, fallback string) string {
	switch v := errVal.(type) {
	case nil:
		if fallback != "" {
			return fallback
		}
		return "opencode execution failed"
	case string:
		if v != "" {
			return v
		}
	case map[string]any:
		for _, k := range []string{"message", "error", "reason"} {
			if s, _ := v[k].(string); s != "" {
				return s
			}
		}
		raw, _ := json.Marshal(v)
		return string(raw)
	}
	if fallback != "" {
		return fallback
	}
	raw, _ := json.Marshal(errVal)
	return string(raw)
}

// ---- permission cards ----

func (t *openCodeTurnV2) openPermissionCard(pr opencodev2.PermissionRequest) {
	select {
	case <-t.doneCh:
		return
	default:
	}
	if !t.claim("p:" + pr.ID) {
		return
	}
	pid := "oc-" + pr.ID
	input := v2PermissionInput(pr)

	if t.mgr.PermissionAllowed(t.sessionID, pr.Action) {
		t.mgr.BroadcastTransient(t.sessionID, "permission_request", map[string]any{
			"type": "permission_request", "id": pid, "tool": pr.Action,
			"patterns": pr.Resources, "auto_allowed": true, "ts": time.Now().UnixMilli(),
		})
		_ = t.srv.ReplyPermission(context.Background(), t.ocSessionID, pr.ID, "always", "")
		t.mgr.BroadcastTransient(t.sessionID, "permission_resolved", map[string]any{
			"type": "permission_resolved", "id": pid, "behavior": "allow", "auto_allowed": true,
		})
		return
	}

	pr2 := t.mgr.PermissionRequest(t.sessionID, pid, pr.Action, input, pr.Source.ID)
	t.mgr.BroadcastTransient(t.sessionID, "permission_request", map[string]any{
		"type": "permission_request", "id": pr2.ID, "tool": pr2.Tool,
		"input": pr2.Input, "tool_use_id": pr2.ToolUseID,
		"patterns": pr.Resources, "ts": pr2.CreatedAt.UnixMilli(),
	})
	ch := t.mgr.PermissionWaitChannel(pr2.ID)
	if ch == nil {
		_ = t.srv.ReplyPermission(context.Background(), t.ocSessionID, pr.ID, "reject", "审批卡注册失败")
		return
	}
	var d PermissionDecision
	select {
	case d = <-ch:
	case <-time.After(ocCardTimeout):
		if t.mgr.PermissionTimeoutRemove(pr2.ID) {
			t.mgr.BroadcastTransient(t.sessionID, "permission_resolved", map[string]any{
				"type": "permission_resolved", "id": pid, "behavior": "deny", "timeout": true,
			})
		}
		_ = t.srv.ReplyPermission(context.Background(), t.ocSessionID, pr.ID, "reject", "用户未在时限内审批")
		return
	case <-t.doneCh:
		t.mgr.removePending(pr2.ID)
		return
	}

	decision := "once"
	if d.Behavior == "allow_tool_session" {
		decision = "always"
	} else if d.Behavior == "deny" {
		decision = "reject"
	}
	if err := t.srv.ReplyPermission(context.Background(), t.ocSessionID, pr.ID, decision, d.Message); err != nil {
		log.Printf("opencode v2 permission reply failed (session %s, rid %s): %v", t.sessionID, pr.ID, err)
	}
	t.mgr.BroadcastTransient(t.sessionID, "permission_resolved", map[string]any{
		"type": "permission_resolved", "id": pid, "behavior": d.Behavior,
	})
}

// v2PermissionInput renders the card payload: the command/path being approved.
func v2PermissionInput(pr opencodev2.PermissionRequest) map[string]any {
	in := map[string]any{"action": pr.Action}
	if len(pr.Resources) == 1 {
		if pr.Action == "shell" {
			in["command"] = pr.Resources[0]
		} else {
			in["path"] = pr.Resources[0]
		}
		in["resource"] = pr.Resources[0]
	} else if len(pr.Resources) > 0 {
		in["resources"] = pr.Resources
	}
	return in
}

// ---- form cards (the v2 replacement for the question tool) ----

func (t *openCodeTurnV2) openFormCard(f opencodev2.FormInfo) {
	select {
	case <-t.doneCh:
		return
	default:
	}
	if !t.claim("f:" + f.ID) {
		return
	}
	questions := formQuestions(f)
	if len(questions) == 0 {
		// No card is possible, so nothing would answer the form.
		_ = t.srv.RejectForm(context.Background(), t.ocSessionID, f.ID)
		return
	}
	input := map[string]any{"questions": questions}
	t.mu.Lock()
	pid := fmt.Sprintf("ocf-%d", time.Now().UnixNano())
	t.mu.Unlock()
	t.mgr.BroadcastTransient(t.sessionID, "permission_request", map[string]any{
		"type": "permission_request", "id": pid, "tool": "AskUserQuestion",
		"input": input, "ts": time.Now().UnixMilli(),
	})
	t.mgr.createPending(t.sessionID, "AskUserQuestion", input, "", pid)
	ch := t.mgr.PermissionWaitChannel(pid)
	if ch == nil {
		_ = t.srv.RejectForm(context.Background(), t.ocSessionID, f.ID)
		return
	}
	var d PermissionDecision
	select {
	case d = <-ch:
	case <-time.After(ocCardTimeout):
		if t.mgr.PermissionTimeoutRemove(pid) {
			t.mgr.BroadcastTransient(t.sessionID, "permission_resolved", map[string]any{
				"type": "permission_resolved", "id": pid, "behavior": "deny", "timeout": true,
			})
		}
		_ = t.srv.RejectForm(context.Background(), t.ocSessionID, f.ID)
		return
	case <-t.doneCh:
		t.mgr.removePending(pid)
		_ = t.srv.RejectForm(context.Background(), t.ocSessionID, f.ID)
		return
	}
	answer := formAnswer(f, d.Answers)
	if d.Behavior == "deny" || len(answer) == 0 {
		_ = t.srv.RejectForm(context.Background(), t.ocSessionID, f.ID)
	} else if err := t.srv.ReplyForm(context.Background(), t.ocSessionID, f.ID, answer); err != nil {
		log.Printf("opencode v2 form reply failed (session %s, form %s): %v", t.sessionID, f.ID, err)
	}
	t.mgr.BroadcastTransient(t.sessionID, "permission_resolved", map[string]any{
		"type": "permission_resolved", "id": pid, "behavior": "allow", "answers": d.Answers,
	})
}

// formQuestions maps form fields onto the Claude AskUserQuestion shape the
// existing card renderer understands: fields with options become choice
// questions, option-less fields become free-text questions.
func formQuestions(f opencodev2.FormInfo) []map[string]any {
	out := make([]map[string]any, 0, len(f.Fields))
	for _, fld := range f.Fields {
		if fld.Type == "external" {
			continue
		}
		title := fld.Title
		if title == "" {
			title = fld.Key
		}
		q := map[string]any{"question": title, "options": []map[string]any{}}
		if len(fld.Options) > 0 {
			opts := make([]map[string]any, 0, len(fld.Options))
			for _, o := range fld.Options {
				opts = append(opts, map[string]any{"label": o.Label, "description": o.Description})
			}
			q["options"] = opts
		}
		if fld.Type == "multiselect" {
			q["multiSelect"] = true
		}
		if fld.Description != "" {
			q["header"] = fld.Description
		}
		out = append(out, q)
	}
	return out
}

// formAnswer maps the card's answer map (question title -> label(s)) back onto
// the form's key/value shape.
func formAnswer(f opencodev2.FormInfo, answers map[string]string) map[string]any {
	if len(answers) == 0 {
		return nil
	}
	out := map[string]any{}
	for _, fld := range f.Fields {
		if fld.Type == "external" {
			continue
		}
		title := fld.Title
		if title == "" {
			title = fld.Key
		}
		raw, ok := answers[title]
		if !ok || raw == "" {
			continue
		}
		switch fld.Type {
		case "multiselect":
			vals := []string{}
			for _, l := range splitAnswerLabels(raw) {
				vals = append(vals, formOptionValue(fld, l))
			}
			out[fld.Key] = vals
		case "boolean":
			out[fld.Key] = strings.EqualFold(raw, "true") || raw == "是"
		case "number", "integer":
			if n, err := strconv.ParseFloat(raw, 64); err == nil {
				out[fld.Key] = n
			} else {
				out[fld.Key] = raw
			}
		default:
			out[fld.Key] = formOptionValue(fld, raw)
		}
	}
	return out
}

func formOptionValue(fld opencodev2.FormField, label string) string {
	for _, o := range fld.Options {
		if o.Label == label || o.Value == label {
			if o.Value != "" {
				return o.Value
			}
			return o.Label
		}
	}
	return label
}

// ---- lifecycle ----

func (t *openCodeTurnV2) claim(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.claimed[key] {
		return false
	}
	t.claimed[key] = true
	return true
}

func (t *openCodeTurnV2) recordError(msg string) {
	if msg == "" {
		return
	}
	t.mu.Lock()
	if t.sessionErr == "" {
		t.sessionErr = msg
	}
	t.lastErr = msg
	t.mu.Unlock()
}

func (t *openCodeTurnV2) markInterrupted() {
	t.mu.Lock()
	t.interrupted = true
	t.mu.Unlock()
}

func (t *openCodeTurnV2) isInterrupted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.interrupted
}

func (t *openCodeTurnV2) errText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionErr
}

func (t *openCodeTurnV2) markDrained() {
	t.drainedO.Do(func() { close(t.drained) })
}

func (t *openCodeTurnV2) markTurnDone() {
	t.turnO.Do(func() { close(t.turnCh) })
}

// finishTurn closes the done channel exactly once.
func (t *openCodeTurnV2) finishTurn() {
	t.doneO.Do(func() { close(t.doneCh) })
}

func (t *openCodeTurnV2) failPendingCards() {
	t.finishTurn()
	for _, pr := range t.mgr.PermissionPending(t.sessionID) {
		if t.mgr.PermissionTimeoutRemove(pr.ID) {
			t.mgr.BroadcastTransient(t.sessionID, "permission_resolved", map[string]any{
				"type": "permission_resolved", "id": pr.ID,
				"behavior": "deny", "timeout": true, "turn_ended": true,
			})
		}
	}
}

func (t *openCodeTurnV2) drain() {
	select {
	case <-t.drained:
	case <-time.After(300 * time.Millisecond):
	}
}

// waitTurn blocks until the execution terminal event, an interrupt, or the
// safety timeout.
func (t *openCodeTurnV2) waitTurn(ctx context.Context) {
	select {
	case <-t.turnCh:
	case <-t.doneCh:
	case <-t.stopCh:
	case <-ctx.Done():
	case <-time.After(turnWaitTimeout):
	}
	t.drain()
}

func (t *openCodeTurnV2) interrupt() {
	t.markInterrupted()
}

func (t *openCodeTurnV2) forceStop(errMsg string) {
	t.markInterrupted()
	t.mu.Lock()
	if errMsg != "" {
		t.sessionErr = errMsg
	}
	t.mu.Unlock()
	t.stopO.Do(func() { close(t.stopCh) })
}

// ---- entry point ----

// runOpenCodeTurnV2 runs one user message over a v2 `opencode serve`.
func (m *Manager) runOpenCodeTurnV2(sessionID string, msg Message, sess *store.Session, cli *config.CLI, prov *config.Provider, resolvedCwd string) {
	model := prov.EffectiveModel(sess.ModelOverride)
	permMode := effectivePermissionMode(sess.PermissionMode, cli.PermissionMode)

	h, release, err := m.ocPool.acquire(context.Background(), cli, prov, model, resolvedCwd)
	if err != nil {
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": err.Error()})
		m.setIdle(sessionID, err.Error())
		return
	}
	defer release()
	srv := h.V2
	if srv == nil {
		const e = "opencode v2 server 未启动"
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": e})
		m.setIdle(sessionID, e)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.setRunning(sessionID, cancel)

	turn := newOpenCodeTurnV2(srv, m, sessionID)
	turn.dir = resolvedCwd
	turn.ctx = ctx
	m.ocTurnsV2.set(sessionID, turn)
	defer m.ocTurnsV2.clear(sessionID, turn)

	ocSID := sess.ClaudeSessionID
	adopted := ocSID != ""
	if !adopted {
		created, err := srv.CreateSession(ctx, sess.Name, resolvedCwd,
			openCodeModelRefV2(prov, model, sess.EffortOverride),
			openCodePermissionRulesetV2(permMode))
		if err != nil {
			cancel()
			m.emit(sessionID, "status", map[string]string{"state": "error", "error": err.Error()})
			m.setIdle(sessionID, err.Error())
			return
		}
		ocSID = created.ID
		m.st.Upsert(sess, store.WithClaudeSID(ocSID))
		sess.ClaudeSessionID = ocSID
	}
	turn.ocSessionID = ocSID

	// v2 binds model+variant at creation, like v1; keep a hot-switched profile
	// from silently running the old model. An empty model means "let the CLI
	// default decide", which the switch endpoint cannot express — skip it.
	if adopted && model != "" {
		if err := srv.SwitchModel(ctx, ocSID, openCodeModelRefV2Value(prov, model, sess.EffortOverride)); err != nil {
			cancel()
			m.emit(sessionID, "status", map[string]string{"state": "error", "error": err.Error()})
			m.setIdle(sessionID, err.Error())
			return
		}
	}

	// Subscribe before prompting so no event of this turn can slip through the
	// gap between the POST and the stream.
	events := make(chan opencodev2.Event, 512)
	go func() {
		defer close(events)
		_ = srv.Stream(ctx, func(ev opencodev2.Event) {
			sid := ev.SessionID()
			if sid == "" && !ev.Control() {
				return
			}
			if sid != "" && sid != ocSID {
				return
			}
			select {
			case events <- ev:
				return
			default:
			}
			if ev.Control() {
				select {
				case events <- ev:
				case <-ctx.Done():
				case <-time.After(2 * time.Second):
				}
			}
		})
	}()
	go turn.consume(events)
	go turn.reconcileLoop()

	prompt := opencodev2.PromptInput{Text: msg.Text}
	for _, img := range msg.Images {
		prompt.Files = append(prompt.Files, opencodev2.PromptFile{
			URI: "data:" + img.MediaType + ";base64," + img.Data,
		})
	}
	for _, f := range msg.Files {
		prompt.Files = append(prompt.Files, opencodev2.PromptFile{
			URI: "file://" + f.Path, Name: f.Name,
		})
	}
	res, err := srv.Prompt(ctx, ocSID, prompt)
	if err != nil {
		cancel()
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": err.Error()})
		m.setIdle(sessionID, err.Error())
		return
	}
	turn.mu.Lock()
	turn.lastUserMessageID = res.ID
	turn.mu.Unlock()
	// The user message id is the rewind checkpoint, exactly as v1's
	// lastUserMessageID was.
	m.writeCheckpoint(sessionID, res.ID)

	turn.waitTurn(ctx)

	switch {
	case turn.isInterrupted():
		m.emit(sessionID, "status", map[string]string{"state": "interrupted"})
		m.setIdle(sessionID, "interrupted")
	case turn.errText() != "":
		errMsg := turn.errText()
		m.emit(sessionID, "status", map[string]string{"state": "error", "error": errMsg})
		m.setIdle(sessionID, errMsg)
	default:
		m.emit(sessionID, "status", map[string]string{"state": "turn_end"})
		m.setIdle(sessionID, "")
	}
	turn.failPendingCards()
	cancel()
}

// openCodeModelRefV2 builds a v2 model reference; nil when the profile names
// no model (let the CLI default decide).
func openCodeModelRefV2(prov *config.Provider, model, effort string) *opencodev2.ModelRef {
	if model == "" {
		return nil
	}
	return &opencodev2.ModelRef{ID: model, ProviderID: prov.Name, Variant: effort}
}

func openCodeModelRefV2Value(prov *config.Provider, model, effort string) opencodev2.ModelRef {
	ref := openCodeModelRefV2(prov, model, effort)
	if ref == nil {
		return opencodev2.ModelRef{}
	}
	return *ref
}

// openCodePermissionRulesetV2 is the v2 ruleset spelling (action/resource/
// effect) of v1's permission ruleset. Only bypassPermissions pins a ruleset;
// everything else lets opencode's own defaults ask.
func openCodePermissionRulesetV2(mode string) []opencodev2.PermissionRule {
	if mode != "bypassPermissions" {
		return nil
	}
	return []opencodev2.PermissionRule{{Action: "*", Resource: "*", Effect: "allow"}}
}

// reconcileLoop is the at-least-once safety net for prompts: the SSE bus is a
// single global stream, and a control event dropped on the floor would leave a
// permission/form pending with no card. Poll the session's pending lists and
// claim anything not yet seen.
func (t *openCodeTurnV2) reconcileLoop() {
	t.reconcileOnce()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-t.doneCh:
			return
		case <-tick.C:
			t.reconcileOnce()
		}
	}
}

func (t *openCodeTurnV2) reconcileOnce() {
	listCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 5*time.Second)
	}
	ctx, cancel := listCtx()
	perms, perr := t.srv.ListPermissions(ctx, t.ocSessionID)
	cancel()
	if perr == nil {
		for _, pr := range perms {
			if t.claim("p:" + pr.ID) {
				go t.openPermissionCard(pr)
			}
		}
	}
	ctx, cancel = listCtx()
	forms, ferr := t.srv.ListForms(ctx, t.ocSessionID)
	cancel()
	if ferr == nil {
		for _, f := range forms {
			if t.claim("f:" + f.ID) {
				go t.openFormCard(f)
			}
		}
	}
}
