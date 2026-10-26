package runner

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"cc-webui/internal/config"
	"cc-webui/internal/opencodeapi"
	"cc-webui/internal/opencodehist"
	"cc-webui/internal/store"
)

// openCodeTurn drives one opencode turn over the serve API: it subscribes to
// the session's SSE slice, adapts parts to the canonical cc-webui stream shape,
// and turns opencode's permission/question prompts into the same approval and
// AskUserQuestion cards the claude engine produces.
//
// The two prompt kinds are asymmetric in opencode and the difference is
// load-bearing:
//
//   - permission is answered by POSTing a reply; opencode's own client
//     auto-answers when nobody does, but under `serve` nothing answers, so the
//     turn would block forever without this bridge.
//   - question is the model's built-in `question` tool. Its Deferred is a
//     bare await: no timeout, no auto-answer, and on server dispose every
//     pending request is rejected. So every card here carries a deadline, and
//     every turn-end path resolves whatever is still open.
type openCodeTurn struct {
	srv         *opencodeapi.Server
	hub         *opencodeapi.Hub
	mgr         *Manager
	sessionID   string // cc-webui session id
	ocSessionID string // opencode session id (ses_…)
	// dir is the session's working directory. Permission/question replies are
	// directory-scoped in opencode 1.18.35, so it must ride the reply calls.
	dir string

	tx *opencodehist.ToolTx

	mu sync.Mutex
	// cards are the outstanding prompts of this turn, keyed by cc-webui id.
	cards map[string]*ocCard
	// questionSeq disambiguates cc-webui ids when opencode hands us its own.
	questionSeq int
	sessionErr  string
	interrupted bool
	// lastUserMessageID is the opencode msg_… id of the user message this
	// turn ran, seen on the SSE stream. It is what the rewind button targets.
	lastUserMessageID string
	// msgRoles maps opencode message ids to their role. opencode streams the
	// user's own prompt back as a text part on the SAME part bus as assistant
	// output, and a part event carries no role of its own — so without this
	// table the echo of "介绍下当前的项目吧" is rendered as if the model had
	// said it. message.updated arrives before the parts of that message, so
	// looking the id up here is reliable.
	msgRoles map[string]string
	// partTypes maps opencode part ids to their announced `part.type`. A
	// message.part.delta carries only {partID, field, delta} — and reasoning
	// parts stream with the same field:"text" as the answer — so the only way
	// to tell them apart is the part announcement, which opencode always
	// publishes first (verified live: every delta is preceded by a
	// message.part.updated for that partID).
	partTypes map[string]string
	// drained is closed by consume when the SSE stream ends.
	drained chan struct{}
	// doneCh is closed by finishTurn once no more card answers can land.
	doneCh chan struct{}
	// stopCh is closed by forceStop when the turn has to stop waiting even
	// though opencode never reported idle.
	stopCh chan struct{}
	// turnDoneCh is closed when the session goes idle — the turn boundary on
	// the v1 prompt path (prompt_async returns 204 immediately, so this event
	// is the only completion signal).
	turnDoneCh chan struct{}
	turnOnce   sync.Once
	// abortErr records why the last stop could not be delivered to opencode.
	// Empty means the turn ended normally (or was aborted successfully).
	abortErr string
	// seenPrompts dedupes the SSE bridge and the reconcile poller, which can
	// both observe the same opencode request. Keyed "q:"+id / "p:"+id.
	seenPrompts map[string]bool
	// ctx is the turn context; the reconcile poller stops with it.
	ctx context.Context
	// lastReconcileErr throttles list-failure logging to one line per distinct
	// error (the poller runs every 2s).
	lastReconcileErr string
}

type ocCard struct {
	// kind is "permission" or "question".
	kind string
	// rid is opencode's request id, used in the reply URLs.
	rid string
	// tool is the card title ("Bash", "Edit", "question").
	tool string
	// input is the cc-webui permission-card payload (Claude-shaped).
	input map[string]any
	// toolUseID links an approval card back to its tool_use block so the
	// frontend can anchor it to the right tool call.
	toolUseID string
	// answers is the per-question answer the user submitted, replayed into the
	// resolved event so the frontend can render the pick without waiting for
	// the model's echo.
	answers map[string]string
	// done is closed once the card has been answered or expired.
	done chan struct{}
}

func newOpenCodeTurn(srv *opencodeapi.Server, hub *opencodeapi.Hub, m *Manager, sessionID string) *openCodeTurn {
	return &openCodeTurn{
		srv: srv, hub: hub, mgr: m, sessionID: sessionID,
		tx:          opencodehist.NewToolTx(),
		cards:       map[string]*ocCard{},
		msgRoles:    map[string]string{},
		partTypes:   map[string]string{},
		seenPrompts: map[string]bool{},
		drained:     make(chan struct{}),
		doneCh:      make(chan struct{}),
		stopCh:      make(chan struct{}),
		turnDoneCh:  make(chan struct{}),
	}
}

// claimPrompt reserves one opencode permission/question request. The SSE
// bridge and the reconcile poller can both see the same request; only the
// first caller may open a card or reply to it.
func (t *openCodeTurn) claimPrompt(kind, rid string) bool {
	if rid == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := kind + ":" + rid
	if t.seenPrompts[key] {
		return false
	}
	t.seenPrompts[key] = true
	return true
}

// promptClaimed reports whether a request was already picked up, without
// claiming it.
func (t *openCodeTurn) promptClaimed(kind, rid string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seenPrompts[kind+":"+rid]
}

// reconcileLoop recovers prompts whose SSE event was lost.
//
// opencode waits forever on a question Deferred, and the 55s budget lives in
// the card handler — so a dropped `question.asked` means no card, no timeout,
// and a session that stays "running" until someone aborts it. The /event bus
// is intentionally lossy for delta traffic (256-entry per-session channel), so
// this poller is the safety net that makes prompt delivery at-least-once.
func (t *openCodeTurn) reconcileLoop() {
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

func (t *openCodeTurn) reconcileOnce() {
	listCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 5*time.Second)
	}

	ctx, cancel := listCtx()
	questions, qerr := t.srv.ListPendingQuestions(ctx, t.dir)
	cancel()
	if qerr != nil {
		t.noteReconcileErr("questions", qerr)
	} else {
		for _, qr := range questions {
			if qr.SessionID != "" && qr.SessionID != t.ocSessionID {
				continue
			}
			if t.promptClaimed("q", qr.ID) {
				continue
			}
			log.Printf("opencode: recovering question %s for session %s (SSE event not seen)", qr.ID, t.sessionID)
			go t.openQuestionCard(qr)
		}
	}

	ctx, cancel = listCtx()
	perms, perr := t.srv.ListPendingPermissions(ctx, t.dir)
	cancel()
	if perr != nil {
		t.noteReconcileErr("permissions", perr)
	} else {
		for _, pr := range perms {
			if pr.SessionID != "" && pr.SessionID != t.ocSessionID {
				continue
			}
			if t.promptClaimed("p", pr.ID) {
				continue
			}
			log.Printf("opencode: recovering permission %s for session %s (SSE event not seen)", pr.ID, t.sessionID)
			go t.openPermissionCard(pr)
		}
	}
}

// noteReconcileErr logs a list failure once per distinct message. A dead
// serve would otherwise print every 2s for the rest of the turn.
func (t *openCodeTurn) noteReconcileErr(what string, err error) {
	msg := err.Error()
	t.mu.Lock()
	dup := t.lastReconcileErr == msg
	t.lastReconcileErr = msg
	t.mu.Unlock()
	if !dup {
		log.Printf("opencode: list pending %s (session %s): %v", what, t.sessionID, err)
	}
}

// openCodeModelRef composes the model reference cc-webui's profile maps onto.
// Reasoning depth is opencode's per-model variant key (the field the old
// `--variant` flag took); the UI only ever offers keys the model metadata
// lists.
func openCodeModelRef(prov *config.Provider, model, effort string) *opencodeapi.ModelRef {
	if model == "" {
		return nil
	}
	ref := &opencodeapi.ModelRef{ID: model, ProviderID: prov.Name}
	if effort != "" {
		ref.Variant = effort
	}
	return ref
}

// createSession creates the opencode session for a new cc-webui session,
// mapping cc-webui's permission_mode onto opencode's ruleset.
func (t *openCodeTurn) createSession(sess *store.Session, cli *config.CLI, prov *config.Provider, model, dir string) (string, error) {
	in := opencodeapi.CreateSessionInput{
		Title: sess.Name,
		// opencode scopes per-project state off this, so the session is pinned
		// to the same directory the one-shot `run` child used to run in.
		Query: map[string]any{"directory": dir},
	}
	in.Model = openCodeModelRef(prov, model, sess.EffortOverride)
	permMode := effectivePermissionMode(sess.PermissionMode, cli.PermissionMode)
	in.Permission = openCodePermissionRuleset(permMode)
	created, err := t.srv.CreateSession(context.Background(), in)
	if err != nil {
		return "", err
	}
	return created.ID, nil
}

// ensureModel aligns an existing opencode session with the session's current
// cc-webui profile.
//
// This is the opencode counterpart of the other engines re-reading
// sess.ModelOverride on every turn: opencode binds the model (and the
// reasoning variant) when the session is created, so a profile change made
// after the first turn — switching model, switching provider, dragging the
// effort dropdown — would never reach opencode, and the UI would silently lie
// about which model is answering. POST /api/session/{id}/model fixes that.
//
// Returns the model that is now in effect (opencode's answer wins, so a
// provider that silently substituted a model still reads correctly).
func (t *openCodeTurn) ensureModel(ctx context.Context, prov *config.Provider, model, effort string) (string, error) {
	want := openCodeModelRef(prov, model, effort)
	if want == nil {
		// No model configured: leave the session's own recorded model alone
		// rather than resetting it to opencode's default.
		return "", nil
	}
	cur, err := t.srv.SessionModel(ctx, t.ocSessionID)
	if err == nil && sameModelRef(cur, *want) {
		return cur.ID, nil
	}
	if err := t.srv.SwitchModel(ctx, t.ocSessionID, *want); err != nil {
		return "", fmt.Errorf("切换 opencode 模型失败：%w", err)
	}
	log.Printf("opencode session %s model %s/%s -> %s/%s (variant %q)",
		t.ocSessionID, cur.ProviderID, cur.ID, want.ProviderID, want.ID, want.Variant)
	return want.ID, nil
}

// sameModelRef reports whether two model references address the same model and
// carry the same reasoning variant.
func sameModelRef(a, b opencodeapi.ModelRef) bool {
	return a.ID == b.ID && a.ProviderID == b.ProviderID && a.Variant == b.Variant
}

// openCodeModelV1 composes the v1 prompt's model reference
// ({providerID, modelID}), which is a different shape from the v2 ModelRef
// used by session create / model switch.
func openCodeModelV1(prov *config.Provider, model string) *opencodeapi.ModelV1 {
	if model == "" {
		return nil
	}
	return &opencodeapi.ModelV1{ProviderID: prov.Name, ModelID: model}
}

// openCodePermissionRuleset maps cc-webui's permission_mode onto opencode's
// session permission ruleset.
//
// cc-webui modes: default | acceptEdits | plan | bypassPermissions | dontAsk
// opencode actions: allow | ask | deny (a ruleset accepts only these three)
//
// The mapping only fires for `bypassPermissions`, whose entire meaning is
// "never interrupt me": every permission is allowed, so no card is ever
// raised. Every other mode returns nil, leaving opencode on its own default —
// which is to ASK — and that is precisely what the approval card consumes.
//
// Nothing else is pinned on purpose. A ruleset scoped to the permissions
// cc-webui happens to know about would silently allow (or deny) every tool
// opencode decides for itself.
func openCodePermissionRuleset(mode string) []opencodeapi.PermissionRule {
	if mode != "bypassPermissions" {
		return nil
	}
	return []opencodeapi.PermissionRule{
		{Name: "*", Pattern: "*", Action: opencodeapi.ActionAllow},
	}
}

// consume reads the session's SSE events until the turn ends.
//
// Two event dialects are handled, because opencode 1.18.35 ships both on
// /event: the v1 `message.part.updated` stream (a whole part per event, mapped
// through the same opencodehist.AdaptPart the replay path uses) and the current
// `session.next.*` lifecycle bus (step/text deltas). Supporting both keeps the
// turn renderer independent of which one the running server speaks, and makes a
// live run and a reload produce identical shapes.
func (t *openCodeTurn) consume(events <-chan opencodeapi.Event) {
	for ev := range events {
		// Track which message each id belongs to BEFORE any of its parts are
		// adapted: the user's prompt is replayed on the part bus and would
		// otherwise render as a model reply.
		if id, role, ok := ev.MessageRole(); ok {
			t.mu.Lock()
			t.msgRoles[id] = role
			t.mu.Unlock()
		}
		// Streamed increments arrive before the assembled part. Forward them so
		// the UI paints token by token; the later message.part.updated carries
		// the authoritative full text and supersedes what was streamed.
		if pd, ok := ev.PartDelta(); ok {
			t.streamDelta(pd)
			continue
		}
		// Part events carry the whole part, so the canonical mapping is the
		// same one the replay path uses.
		if pe, ok := ev.Part(); ok {
			if pe.Part == nil {
				continue
			}
			// Record the type before anything else can bail out: the deltas
			// that follow are bare {partID, field, delta} and can only be
			// classified through this announcement.
			if id, _ := pe.Part["id"].(string); id != "" {
				tp, _ := pe.Part["type"].(string)
				t.mu.Lock()
				t.partTypes[id] = tp
				t.mu.Unlock()
			}
			messageID := pe.MessageID
			if messageID == "" {
				messageID = ev.MessageIDOf()
			}
			if t.isUserMessage(messageID) {
				// The user's own prompt echoed back on the part bus.
				// cc-webui stores that echo itself at enqueue time
				// (see the echo rule in Enqueue), so rendering it here would
				// duplicate it as a model reply.
				if messageID != "" {
					t.mu.Lock()
					t.lastUserMessageID = messageID
					t.mu.Unlock()
				}
				continue
			}
			ts := partTime(pe.Part)
			for _, payload := range opencodehist.AdaptPart(messageID, pe.Part, ts, t.tx) {
				t.mgr.emit(t.sessionID, "stream", payload)
			}
			continue
		}
		if pr, ok := ev.PermissionAsked(); ok {
			go t.openPermissionCard(pr)
			continue
		}
		if qr, ok := ev.QuestionAsked(); ok {
			go t.openQuestionCard(qr)
			continue
		}
		if np, ok := ev.Next(); ok {
			t.handleNext(np)
			continue
		}
		if msg, ok := ev.SessionError(); ok {
			t.recordError(msg)
		}
	}
	// The subscription closed (turn finished, or the serve process went away):
	// let drain() return instead of waiting out its grace period.
	t.mu.Lock()
	select {
	case <-t.drained:
	default:
		close(t.drained)
	}
	t.mu.Unlock()
}

// handleNext consumes one session.next.* lifecycle event.
func (t *openCodeTurn) handleNext(np opencodeapi.NextPart) {
	switch np.Kind {
	case "text":
		if np.Text == "" {
			return
		}
		// Text deltas are streamed as ordinary assistant blocks keyed by the
		// assistant message id, so the frontend's trajectory merge behaves the
		// same as on the claude engine.
		msgID := np.MessageID
		if msgID == "" {
			return
		}
		block := map[string]any{"type": "text", "text": np.Text}
		t.mgr.emit(t.sessionID, "stream", map[string]any{
			"type":    "assistant",
			"message": map[string]any{"role": "assistant", "id": msgID, "content": []any{block}},
		})
	case "step_failed":
		t.recordError(np.ErrMsg)
	case "busy":
		// The session started working: the prompt landed. Nothing to emit, but
		// it is the signal that the turn is genuinely running (as opposed to
		// the v2 endpoint's admit-and-forget, which never emits this).
	case "idle":
		// The turn boundary. With a shared serve process there is no child
		// process to exit, so the turn closes on the session's idle event.
		t.markTurnDone()
	}
}

func (t *openCodeTurn) recordError(msg string) {
	if msg == "" {
		return
	}
	t.mu.Lock()
	if t.sessionErr == "" {
		t.sessionErr = msg
	}
	t.mu.Unlock()
}

// partTime reads a part's own timestamp, falling back to now.
func partTime(part map[string]any) time.Time {
	if ms, ok := part["time"].(map[string]any); ok {
		if v, _ := ms["start"].(float64); v > 0 {
			return time.UnixMilli(int64(v))
		}
	}
	if v, ok := part["time"].(float64); ok && v > 0 {
		return time.UnixMilli(int64(v))
	}
	return time.Now()
}

// ---- permission cards ----

// ocCardTimeout is how long a card waits for the user before auto-resolving.
//
// opencode has no server-side deadline on either prompt type: permission.asked
// and the question Deferred both wait forever. The claude engine's 55s budget
// came from Claude Code's own 60s hook cap; here the only constraint is our
// own UX, so keep it a little tighter and always resolve explicitly.
const ocCardTimeout = 55 * time.Second

// openPermissionCard bridges one `permission.asked` event onto the existing
// cc-webui approval card, then answers opencode with the user's decision.
func (t *openCodeTurn) openPermissionCard(pr opencodeapi.PermissionRequest) {
	// A card opened after the turn is over would linger in a session the agent
	// can no longer act on (the reconcile poller can race the turn's end).
	select {
	case <-t.doneCh:
		return
	default:
	}
	if !t.claimPrompt("p", pr.ID) {
		return
	}
	// opencode 1.18.35's permission replies are directory-scoped, not
	// session-scoped: the flat v1 /permission route has no session path
	// segment, so t.dir is what pins the reply to this session's project.
	// Same-tool session allowance ("本次会话都允许"): skip the card and reply
	// `always` straight away, but still broadcast a transient pair so the
	// timeline shows why there was no card.
	if t.mgr.PermissionAllowed(t.sessionID, pr.Permission) {
		t.mgr.BroadcastTransient(t.sessionID, "permission_request", map[string]any{
			"type": "permission_request", "id": "oc-" + pr.ID, "tool": pr.Permission,
			"patterns": pr.Patterns, "auto_allowed": true, "ts": time.Now().UnixMilli(),
		})
		_ = t.srv.ReplyPermission(context.Background(), t.dir, pr.ID,
			opencodeapi.PermissionReply{Reply: "always"})
		t.mgr.BroadcastTransient(t.sessionID, "permission_resolved", map[string]any{
			"type": "permission_resolved", "id": "oc-" + pr.ID,
			"behavior": "allow", "auto_allowed": true,
		})
		return
	}

	// Render the tool's real call arguments when opencode supplied them, so the
	// card shows what is about to happen (the command, the path, the diff)
	// rather than only a tool name.
	input := permissionCardInput(pr)
	pid := "oc-" + pr.ID
	pr2 := t.mgr.PermissionRequest(t.sessionID, pid, pr.Permission, input, pr.CallID)
	t.mgr.BroadcastTransient(t.sessionID, "permission_request", map[string]any{
		"type": "permission_request", "id": pr2.ID, "tool": pr2.Tool,
		"input": pr2.Input, "tool_use_id": pr2.ToolUseID,
		"patterns": pr.Patterns, "ts": pr2.CreatedAt.UnixMilli(),
	})

	ch := t.mgr.PermissionWaitChannel(pr2.ID)
	if ch == nil {
		// Should not happen (we just registered it), but returning here would
		// leave opencode waiting forever. Reject so the tool can move on.
		if err := t.srv.ReplyPermission(context.Background(), t.dir, pr.ID,
			opencodeapi.PermissionReply{Reply: "reject", Message: "审批卡注册失败"}); err != nil {
			log.Printf("opencode permission reject failed (session %s, rid %s): %v", t.sessionID, pr.ID, err)
		}
		return
	}
	var d PermissionDecision
	select {
	case d = <-ch:
	case <-time.After(ocCardTimeout):
		if t.mgr.PermissionTimeoutRemove(pr2.ID) {
			t.mgr.BroadcastTransient(t.sessionID, "permission_resolved", map[string]any{
				"type": "permission_resolved", "id": pid,
				"behavior": "deny", "timeout": true,
			})
		}
		// Reject rather than allow: a silent auto-approve after a timeout is
		// the worst failure mode for an approval card.
		if err := t.srv.ReplyPermission(context.Background(), t.dir, pr.ID,
			opencodeapi.PermissionReply{Reply: "reject", Message: "用户未在时限内审批"}); err != nil {
			log.Printf("opencode permission timeout-reject failed (session %s, rid %s): %v", t.sessionID, pr.ID, err)
		}
		return
	case <-t.turnDone():
		t.mgr.removePending(pr2.ID)
		return
	}

	reply := opencodeapi.PermissionReply{Reply: mapBehavior(d.Behavior)}
	if d.Behavior == "deny" && d.Message != "" {
		reply.Message = d.Message
	}
	if err := t.srv.ReplyPermission(context.Background(), t.dir, pr.ID, reply); err != nil {
		log.Printf("opencode permission reply failed (session %s, rid %s): %v", t.sessionID, pr.ID, err)
	}
	t.mgr.BroadcastTransient(t.sessionID, "permission_resolved", map[string]any{
		"type": "permission_resolved", "id": pid, "behavior": d.Behavior,
	})
}

// mapBehavior maps cc-webui's decision vocabulary onto opencode's three.
//
// allow_tool_session becomes `always`: opencode's "always" is the closest
// equivalent (remember the grant), though it lives in the server process
// memory rather than on disk and is scoped to that server instance.
func mapBehavior(behavior string) string {
	switch behavior {
	case "allow", "allow_tool_session":
		return "once"
	default:
		return "reject"
	}
}

// permissionCardInput builds the cc-webui card payload from opencode's
// request. When opencode hands over the actual tool call input, use it
// verbatim (that is what makes the card informative); otherwise fall back to
// the permission's patterns.
func permissionCardInput(pr opencodeapi.PermissionRequest) map[string]any {
	if len(pr.Tool) > 0 {
		if state, ok := pr.Tool["state"].(map[string]any); ok {
			if in, ok := state["input"].(map[string]any); ok && len(in) > 0 {
				return in
			}
		}
		if in, ok := pr.Tool["input"].(map[string]any); ok && len(in) > 0 {
			return in
		}
	}
	if len(pr.Patterns) > 0 {
		return map[string]any{"patterns": pr.Patterns}
	}
	if pr.Title != "" {
		return map[string]any{"title": pr.Title}
	}
	return map[string]any{}
}

// ---- question cards (the opencode `question` tool) ----

// openQuestionCard renders one `question.asked` event as an AskUserQuestion
// card. opencode's question shape (question / header / options[{label,
// description}] / multiple / custom) lines up with Claude's AskUserQuestion
// input closely enough that the existing card parses it unchanged.
func (t *openCodeTurn) openQuestionCard(qr opencodeapi.QuestionRequest) {
	select {
	case <-t.doneCh:
		return
	default:
	}
	if !t.claimPrompt("q", qr.ID) {
		return
	}
	// Same as permissions: the flat v1 /question route is directory-scoped,
	// so replies carry t.dir rather than a session id.
	questions := qr.Questions
	if len(questions) == 0 {
		// No card is possible, so nothing would ever answer or time out the
		// Deferred — reject right away instead of stranding the turn.
		log.Printf("opencode question %s has no questions; rejecting (session %s)", qr.ID, t.sessionID)
		if err := t.srv.RejectQuestion(context.Background(), t.dir, qr.ID); err != nil {
			log.Printf("opencode question empty-reject failed (session %s, rid %s): %v", t.sessionID, qr.ID, err)
		}
		return
	}
	// Build the Claude-shaped input the frontend already understands.
	input := map[string]any{"questions": openCodeQuestionsToClaude(questions)}

	t.mu.Lock()
	t.questionSeq++
	pid := fmt.Sprintf("ocq-%d-%d", time.Now().UnixNano(), t.questionSeq)
	t.mu.Unlock()
	log.Printf("opencode question card (session %s, rid %s, %d question(s))", t.sessionID, qr.ID, len(questions))

	// Tool name "AskUserQuestion" is the existing frontend's switch for the
	// question card; using it keeps one renderer instead of two.
	t.mgr.BroadcastTransient(t.sessionID, "permission_request", map[string]any{
		"type": "permission_request", "id": pid, "tool": "AskUserQuestion",
		"input": input, "ts": time.Now().UnixMilli(),
	})

	// Answer collection: reuse the same pending registry so the existing
	// resolve endpoint (/api/sessions/{id}/permissions/{pid}) and its timeout
	// semantics apply unchanged.
	t.mgr.createPending(t.sessionID, "AskUserQuestion", input, "", pid)
	ch := t.mgr.PermissionWaitChannel(pid)
	if ch == nil {
		if err := t.srv.RejectQuestion(context.Background(), t.dir, qr.ID); err != nil {
			log.Printf("opencode question reject failed (session %s, rid %s): %v", t.sessionID, qr.ID, err)
		}
		return
	}
	var d PermissionDecision
	select {
	case d = <-ch:
	case <-time.After(ocCardTimeout):
		if t.mgr.PermissionTimeoutRemove(pid) {
			t.mgr.BroadcastTransient(t.sessionID, "permission_resolved", map[string]any{
				"type": "permission_resolved", "id": pid,
				"behavior": "deny", "timeout": true,
			})
		}
		// Reject, not answer: the model then sees "Unanswered" and can move
		// on or re-ask. Denying a question is the honest outcome.
		if err := t.srv.RejectQuestion(context.Background(), t.dir, qr.ID); err != nil {
			// Leaving it pending would block the turn forever, and the 12h
			// waitTurn safety net is far too long to be useful.
			log.Printf("opencode question timeout-reject failed (session %s, rid %s): %v", t.sessionID, qr.ID, err)
		}
		return
	case <-t.turnDone():
		// Turn ended while the card was open (interrupt / server gone).
		t.mgr.removePending(pid)
		if err := t.srv.RejectQuestion(context.Background(), t.dir, qr.ID); err != nil {
			log.Printf("opencode question turn-end reject failed (session %s, rid %s): %v", t.sessionID, qr.ID, err)
		}
		return
	}

	answers := openCodeAnswersFromClaude(d.Answers, questions)
	if len(answers) == 0 {
		if err := t.srv.RejectQuestion(context.Background(), t.dir, qr.ID); err != nil {
			log.Printf("opencode question reject failed (session %s, rid %s): %v", t.sessionID, qr.ID, err)
		}
	} else if err := t.srv.ReplyQuestion(context.Background(), t.dir, qr.ID,
		opencodeapi.QuestionReply{Answers: answers}); err != nil {
		log.Printf("opencode question reply failed (session %s, rid %s): %v", t.sessionID, qr.ID, err)
	}
	t.mgr.BroadcastTransient(t.sessionID, "permission_resolved", map[string]any{
		"type": "permission_resolved", "id": pid, "behavior": "allow",
		"answers": d.Answers,
	})
}

// openCodeQuestionsToClaude rewrites opencode's question prompts into the
// Claude AskUserQuestion input shape the frontend parses: options as
// {label, description}, multiple under `multiSelect`, custom under nothing
// (the card always offers "其他").
func openCodeQuestionsToClaude(in []opencodeapi.QuestionPrompt) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, q := range in {
		opts := make([]map[string]any, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, map[string]any{"label": o.Label, "description": o.Description})
		}
		m := map[string]any{"question": q.Question, "options": opts}
		if q.Header != "" {
			m["header"] = q.Header
		}
		if q.Multiple {
			m["multiSelect"] = true
		}
		out = append(out, m)
	}
	return out
}

// openCodeAnswersFromClaude turns the frontend's answer map (question text ->
// comma-joined labels) back into opencode's per-question array of labels,
// which is the shape the question tool's metadata.answers expects.
func openCodeAnswersFromClaude(answers map[string]string, questions []opencodeapi.QuestionPrompt) [][]string {
	if len(answers) == 0 {
		return nil
	}
	out := make([][]string, 0, len(questions))
	for _, q := range questions {
		raw, ok := answers[q.Question]
		if !ok || raw == "" {
			out = append(out, []string{})
			continue
		}
		var labels []string
		for _, l := range splitAnswerLabels(raw) {
			if l != "" {
				labels = append(labels, l)
			}
		}
		out = append(out, labels)
	}
	return out
}

// splitAnswerLabels splits the frontend's comma-joined answer value. The
// frontend joins multiple picks with "，" (full-width) or "," depending on the
// card path, so accept both plus newlines.
func splitAnswerLabels(s string) []string {
	out := []string{}
	cur := ""
	flush := func() {
		if cur = trimSpaceFast(cur); cur != "" {
			out = append(out, cur)
		}
		cur = ""
	}
	for _, r := range s {
		switch r {
		case ',', '，', '\n':
			flush()
		default:
			cur += string(r)
		}
	}
	flush()
	return out
}

func trimSpaceFast(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// ---- turn lifecycle ----

// turnDone is closed when the turn can no longer accept card answers.
func (t *openCodeTurn) turnDone() <-chan struct{} {
	return t.doneCh
}

// finishTurn closes the done channel exactly once.
func (t *openCodeTurn) finishTurn() {
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case <-t.doneCh:
	default:
		close(t.doneCh)
	}
}

// drain waits briefly for the SSE consumer to catch up with the turn's final
// events, so the last assistant text is not lost between the POST returning
// and the stream being read.
func (t *openCodeTurn) drain() {
	select {
	case <-t.drained:
	case <-time.After(300 * time.Millisecond):
	}
}

// failPendingCards resolves every card still open when the turn ends. opencode
// has no client-side cancellation of its own Deferreds; leaving a card open
// would strand the frontend on a prompt the agent can no longer act on.
func (t *openCodeTurn) failPendingCards() {
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

// interrupt is invoked when the user presses stop mid-turn. It marks the turn
// interrupted; the idle event from opencode's abort is what unblocks waitTurn.
func (t *openCodeTurn) interrupt() {
	t.mu.Lock()
	t.interrupted = true
	t.mu.Unlock()
}

// forceStop releases waitTurn when opencode never acknowledged the abort.
// abortErr records why, so the caller can surface an error instead of a clean
// "interrupted".
func (t *openCodeTurn) forceStop(errMsg string) {
	t.mu.Lock()
	t.interrupted = true
	if errMsg != "" {
		t.abortErr = errMsg
	}
	t.mu.Unlock()
	select {
	case <-t.stopCh:
	default:
		close(t.stopCh)
	}
}

// abortFailure returns the reason a stop could not be delivered, or "".
func (t *openCodeTurn) abortFailure() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.abortErr
}

// isInterrupted reports whether stop was pressed for this turn.
func (t *openCodeTurn) isInterrupted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.interrupted
}

// interruptOpenCodeTurn asks opencode to stop the session's running turn. It
// returns false for any session that is not a live opencode turn, so the
// caller falls back to the process-kill path.
func (m *Manager) interruptOpenCodeTurn(sessionID string) bool {
	if t := m.ocTurnsV2.get(sessionID); t != nil {
		// Mark first: the execution terminal event can race the interrupt's
		// HTTP response, and the turn must still be reported as interrupted
		// (a clean finish would fire Session Goal).
		t.interrupt()
		go func() {
			var err error
			for attempt := 1; attempt <= 3; attempt++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err = t.srv.Interrupt(ctx, t.ocSessionID)
				cancel()
				if err == nil {
					break
				}
				log.Printf("opencode v2 interrupt failed (session %s, attempt %d): %v", sessionID, attempt, err)
				if attempt < 3 {
					time.Sleep(300 * time.Millisecond)
				}
			}
			if err != nil {
				t.forceStop("停止失败：" + err.Error())
				return
			}
			select {
			case <-t.doneCh:
			case <-t.ctx.Done():
			case <-time.After(10 * time.Second):
				log.Printf("opencode v2 interrupt acknowledged but session %s never closed; releasing turn wait", sessionID)
				t.forceStop("")
			}
		}()
		return true
	}
	t := m.ocTurns.get(sessionID)
	if t == nil {
		return false
	}
	// Mark it first: opencode's idle event can race the abort's HTTP response,
	// and the turn must still be reported as interrupted (not as a clean finish
	// that could trigger a Session Goal).
	t.interrupt()
	// POST the abort so opencode actually stops the agent loop; the
	// context cancel that follows only unwinds our own HTTP call.
	go func() {
		var err error
		for attempt := 1; attempt <= 3; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err = t.srv.Interrupt(ctx, t.dir, t.ocSessionID)
			cancel()
			if err == nil {
				break
			}
			log.Printf("opencode interrupt failed (session %s, attempt %d): %v", sessionID, attempt, err)
			if attempt < 3 {
				time.Sleep(300 * time.Millisecond)
			}
		}
		if err != nil {
			// We cannot stop the agent loop, but leaving the card dock and
			// the "running" badge up forever is worse: release the wait and
			// tell the user what happened.
			t.forceStop("停止失败：" + err.Error())
			return
		}
		// An acknowledged abort should produce idle within a moment. If it
		// does not (opencode bug, dropped event), release the turn rather than
		// leaving the stop button permanently inert.
		select {
		case <-t.doneCh:
		case <-t.ctx.Done():
		case <-time.After(10 * time.Second):
			log.Printf("opencode abort acknowledged but session %s never went idle; releasing turn wait", sessionID)
			t.forceStop("")
		}
	}()
	return true
}

// markTurnDone records that the session went idle. Safe to call repeatedly:
// opencode emits idle more than once per turn (once per step boundary).
func (t *openCodeTurn) markTurnDone() {
	t.turnOnce.Do(func() { close(t.turnDoneCh) })
}

// turnWaitTimeout bounds how long a turn may run before cc-webui stops
// waiting. It is a safety net, not a policy: opencode has no turn timeout, and
// a long tool call legitimately exceeds any fixed budget. Only a turn that
// never emits idle is released by this — the event path closes the turn in
// milliseconds.
const turnWaitTimeout = 12 * time.Hour

// waitTurn blocks until the session reports idle (turn finished), the user
// interrupts, or the safety timeout expires. The prompt POST itself returns
// immediately on this path, so this is what makes the turn lifecycle
// synchronous from cc-webui's point of view.
func (t *openCodeTurn) waitTurn(ctx context.Context) {
	// Give opencode a moment to publish the first status event before
	// trusting an already-closed channel from a previous turn.
	select {
	case <-t.turnDoneCh:
	case <-t.doneCh:
	case <-t.stopCh:
	case <-ctx.Done():
	case <-time.After(turnWaitTimeout):
		return
	}
	// Let the SSE consumer flush the last parts, which arrive just before
	// (or with) the idle event and would otherwise be dropped when this turn's
	// subscription is torn down.
	t.drain()
}

// isUserMessage reports whether an opencode message id belongs to the user
// side of the conversation.
func (t *openCodeTurn) isUserMessage(id string) bool {
	if id == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.msgRoles[id] == "user"
}

// streamDelta forwards one opencode text increment as a live assistant block.
//
// opencode emits message.part.delta (field+delta only) while a text part is
// being generated, and the complete part in a later message.part.updated.
// Only the latter survives AdaptPart, so without this the whole answer appears
// in one jump at the end. Each increment is emitted as its own assistant block
// carrying the SAME part id: the frontend keys partial blocks by id and
// replaces the buffer, so repeated ids merge into one rendered paragraph while
// the final updated event settles it.
func (t *openCodeTurn) streamDelta(pd opencodeapi.PartDelta) {
	if !t.streamable(pd) {
		return
	}
	// A dedicated event kind rather than a canonical assistant block: the
	// frontend renders each canonical text block as its OWN bubble, so
	// emitting one per increment would produce a pile of fragments. This
	// carries the part id, letting the frontend accumulate into a single
	// bubble that the later full message.part.updated supersedes.
	t.mgr.emit(t.sessionID, "stream", map[string]any{
		"type":      "oc_text_delta",
		"messageID": pd.MessageID,
		"partID":    pd.PartID,
		"delta":     pd.Delta,
	})
}

// streamable decides whether one increment belongs in the transcript.
//
// Only `text` parts pass. A reasoning part streams with the very same
// field:"text", so the field alone cannot tell the two apart — forwarding those
// deltas paints the model's private thinking into the transcript as if it were
// the answer. AdaptPart already drops reasoning once the full part lands
// (opencodehist.adaptPart's default branch), so skipping the deltas is what
// keeps the live view and the post-reload replay identical.
//
// A part never seen announced is dropped as well: a missing or reordered
// announcement costs at most the token-by-token paint, since the assembled part
// still arrives and renders whole, while a leaked reasoning stream cannot be
// taken back once it is on screen.
//
// A user-side message (the echoed prompt) is skipped, mirroring the whole-part
// path — otherwise the question would stream back as the model's answer. Its
// role is only known if message.updated arrived, and an unknown role passes, so
// a dropped role event costs a duplicated echo rather than a swallowed answer.
func (t *openCodeTurn) streamable(pd opencodeapi.PartDelta) bool {
	if pd.Field != "" && pd.Field != "text" {
		return false // opencode reserves fields other than text
	}
	if pd.MessageID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.msgRoles[pd.MessageID] == "user" {
		return false
	}
	return t.partTypes[pd.PartID] == "text"
}
