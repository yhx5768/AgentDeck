package runner

// Session Goals — "keep turning until this condition holds".
//
// The mechanism is deliberately dumb, because everything hard about this
// feature is a judgement call we deliberately do NOT automate:
//
//   - The model judges its own work. There is no independent verifier, so a
//     model that declares victory on a half-finished task ends the goal early.
//     That is a known, accepted trade-off: the user writes the condition, and a
//     precise condition ("tests pass AND ./build exits 0") is far more robust
//     than a vague one. The circuit breaker is what keeps a wrong judgement
//     from costing anything but one round.
//   - Nothing runs unattended. Every continuation is a normal queued turn, so
//     permissions, AskUserQuestion, peer messages and Stop all keep working
//     exactly as they do for a typed message.
//
// Flow: a clean turn ends -> advanceGoal judges the goal -> injects an
// evaluation prompt that demands a marker -> the next turn ends -> the marker
// decides met / unmet / exhausted. The marker is read off the broadcast stream
// rather than the event log because stream events are never persisted (see
// emit), so the runtime keeps a rolling tail of assistant text per turn.

import (
	"fmt"
	"log"
	"strings"
	"time"

	"cc-webui/internal/store"
)

const (
	goalMarkerMet   = "<!--goal:met-->"
	goalMarkerUnmet = "<!--goal:unmet"
	// The marker may be split across streamed deltas, so we keep a rolling tail
	// of assistant text and test the whole tail rather than each chunk.
	goalTailBudget = 256
)

// goalVerdict values.
const (
	goalVerdictNone  = ""
	goalVerdictMet   = "met"
	goalVerdictUnmet = "unmet"
)

// resetGoalTurn clears the per-turn verdict state. Called at the top of every
// turn so a stale marker from the previous round can never decide this one.
// goalAwait is deliberately preserved: advanceGoal sets it before enqueueing and
// the worker picks that turn up on another goroutine, so clearing it here would
// make every injected turn look like a user turn.
func (m *Manager) resetGoalTurn(sessionID string) {
	r := m.rt(sessionID)
	r.mu.Lock()
	r.goalVerdict = goalVerdictNone
	r.goalTail = ""
	r.mu.Unlock()
}

// clearGoalState drops the whole per-turn state, verdict AND goalAwait. Used when
// a goal reaches a terminal state and must stop being watched.
func (m *Manager) clearGoalState(sessionID string) {
	r := m.rt(sessionID)
	r.mu.Lock()
	r.goalVerdict = goalVerdictNone
	r.goalTail = ""
	r.goalAwait = false
	r.mu.Unlock()
}

// noteGoalOutput folds assistant text into the turn's rolling tail and records
// a verdict the moment the marker shows up. Called from emit for every stream
// event, so it sees partial deltas and complete blocks alike.
func (m *Manager) noteGoalOutput(sessionID, text string) {
	if text == "" {
		return
	}
	r := m.rt(sessionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.goalAwait {
		return // only the injected evaluation turn is watched
	}
	r.goalTail += text
	if len(r.goalTail) > goalTailBudget {
		r.goalTail = r.goalTail[len(r.goalTail)-goalTailBudget:]
	}
	switch {
	case strings.Contains(r.goalTail, goalMarkerMet):
		r.goalVerdict = goalVerdictMet
	case strings.Contains(r.goalTail, goalMarkerUnmet):
		r.goalVerdict = goalVerdictUnmet
	}
}

// assistantTextOf pulls every text block out of a canonical assistant stream
// event. The runner normalizes all four engines onto this shape (see AGENTS.md
// on the canonical event model), so one extractor covers every engine.
func assistantTextOf(data any) string {
	m, ok := data.(map[string]any)
	if !ok {
		return ""
	}
	msg, _ := m["message"].(map[string]any)
	if msg == nil {
		return ""
	}
	content, _ := msg["content"].([]any)
	var b strings.Builder
	for _, c := range content {
		blk, ok := c.(map[string]any)
		if !ok || blk["type"] != "text" {
			continue
		}
		if s, ok := blk["text"].(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

// advanceGoal is called after every clean turn end. It either stops the goal
// (met / circuit breaker) or queues the next continuation turn.
func (m *Manager) advanceGoal(sessionID string) {
	// setIdle calls this from a fresh goroutine on every clean turn end, so it
	// must be total: a nil store would otherwise take the whole process down
	// from a goroutine, not just fail the turn.
	if m.st == nil {
		return
	}
	sess := m.st.Get(sessionID)
	if sess == nil {
		return
	}
	goal := sess.Goal
	if !goal.Active() {
		return
	}
	now := time.Now()

	r := m.rt(sessionID)
	r.mu.Lock()
	verdict := r.goalVerdict
	wasAwait := r.goalAwait
	// Queue length, NOT Busy(): advanceGoal runs from setIdle, which the worker
	// calls from inside work(), so r.working is still true here and Busy() would
	// always report true and kill the whole feature. What we actually want to
	// know is whether the user queued something of their own that should win
	// the slot.
	queued := len(r.queue)
	r.mu.Unlock()

	// A turn we did not inject (the user's own message, or a peer-injected
	// turn): there is no marker to read, so start the evaluation cycle.
	// A turn we did inject and where the model never emitted a marker means the
	// instruction was ignored — stop rather than spin, and say why.
	if wasAwait && verdict == goalVerdictNone {
		m.finishGoal(sessionID, store.GoalStopped, "模型没有给出达标判定，已停止自动续跑")
		return
	}
	if verdict == goalVerdictMet {
		m.finishGoal(sessionID, store.GoalMet, "已达标")
		return
	}
	if goal.Exhausted(now) {
		m.finishGoal(sessionID, store.GoalExhausted, goal.StopReason(now))
		return
	}
	if queued > 0 {
		// The user queued their own message; let it run first. The goal resumes
		// on the next turn end rather than racing the user for the slot.
		return
	}

	round := goal.Rounds + 1
	if _, err := m.st.MutateGoal(sessionID, func(g *store.Goal) {
		g.Rounds = round
	}); err != nil {
		log.Printf("goal %s: bump round: %v", sessionID, err)
		return
	}

	// Mark the goalAwait flag BEFORE enqueueing: the turn may start on another
	// goroutine the moment the worker picks it up, and noteGoalOutput would
	// otherwise drop the marker.
	r.mu.Lock()
	r.goalAwait = true
	r.goalVerdict = goalVerdictNone
	r.goalTail = ""
	r.mu.Unlock()

	prompt := goalPrompt(goal, round)
	if err := m.Enqueue(sessionID, Message{Text: prompt}); err != nil {
		log.Printf("goal %s: enqueue round %d: %v", sessionID, round, err)
		m.finishGoal(sessionID, store.GoalStopped, "续跑入队失败："+err.Error())
		return
	}
	m.emit(sessionID, "status", map[string]any{
		"state": "goal_round",
		"round": round,
		"total": goal.MaxTurns,
		"text":  goal.Text,
	})
}

// finishGoal parks the goal in a terminal state and stops the loop.
func (m *Manager) finishGoal(sessionID, status, reason string) {
	if m.st == nil {
		return
	}
	if _, err := m.st.MutateGoal(sessionID, func(g *store.Goal) {
		g.Status = status
		g.LastReason = reason
	}); err != nil {
		log.Printf("goal %s: finish: %v", sessionID, err)
	}
	m.clearGoalState(sessionID)
	m.emit(sessionID, "status", map[string]any{
		"state":  "goal_end",
		"status": status,
		"reason": reason,
	})
}

// StopGoal parks an active goal without touching the running turn. The user
// hitting stop is an unambiguous "I want this to stop", so it must also stop the
// automation — otherwise the next turn end would queue yet another round.
func (m *Manager) StopGoal(sessionID string) {
	if m.st == nil {
		return
	}
	if sess := m.st.Get(sessionID); sess.Goal.Active() {
		m.finishGoal(sessionID, store.GoalStopped, "用户已停止")
	}
}

// InvalidateGoal drops a goal entirely. Rewind uses it: the work the goal was
// chasing has just been rolled back, so the old goal is meaningless and must not
// resume against the rewound transcript.
func (m *Manager) InvalidateGoal(sessionID string) {
	if m.st == nil {
		return
	}
	if sess := m.st.Get(sessionID); sess.Goal != nil {
		if err := m.st.SetGoal(sessionID, nil); err != nil {
			log.Printf("goal %s: invalidate: %v", sessionID, err)
		}
		m.clearGoalState(sessionID)
	}
}

// goalPrompt is the continuation turn. It is written as an instruction to judge
// first and act second, and it quotes the user's own condition verbatim so the
// model judges against the original wording rather than its memory of it.
func goalPrompt(g *store.Goal, round int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "【Session Goal 自检 · 第 %d/%d 轮】\n\n", round, g.MaxTurns)
	fmt.Fprintf(&b, "本次会话的终止条件是：\n%s\n\n", g.Text)
	b.WriteString("按顺序做两件事：\n")
	b.WriteString("1. 先自检：对照上面的终止条件，判断它此刻是否已经成立。如果不成立，说清楚还差什么。\n")
	b.WriteString("2. 如果不成立，就继续把没做完的做完；成立了就不要再动代码，只汇报。\n\n")
	b.WriteString("格式要求（必须遵守）：回复的最后一行只能是下面两者之一，不要有多余内容：\n")
	fmt.Fprintf(&b, "  条件成立 → %s\n", goalMarkerMet)
	fmt.Fprintf(&b, "  条件不成立 → %s 还差什么（简短一句）\n", goalMarkerUnmet)
	b.WriteString("\n注意：这是一个多轮自动续跑机制，未达标就会再来一轮，直到达标或触发上限（")
	fmt.Fprintf(&b, "最多 %d 轮 / %g 小时）", g.MaxTurns, g.MaxHours)
	b.WriteString("）。不要把未完成说成已完成——宁可多跑一轮，也不要错误地提前结束。")
	return b.String()
}
