package runner

import (
	"testing"
	"time"

	"cc-webui/internal/store"
)

func assistantEvent(text string) map[string]any {
	return map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": text},
			},
		},
	}
}

func TestAssistantTextOf(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"单个 text 块", assistantEvent("hi"), "hi"},
		{"多个 text 块拼接", map[string]any{"message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "a"},
			map[string]any{"type": "thinking", "thinking": "跳过"},
			map[string]any{"type": "text", "text": "b"},
		}}}, "ab"},
		{"没有 message", map[string]any{"type": "assistant"}, ""},
		{"不是 map", "garbage", ""},
		{"content 不是数组", map[string]any{"message": map[string]any{"content": "x"}}, ""},
	}
	for _, c := range cases {
		if got := assistantTextOf(c.in); got != c.want {
			t.Errorf("%s: assistantTextOf = %q, want %q", c.name, got, c.want)
		}
	}
}

// A goal only watches the turn it injected. A marker that leaks in from an
// unrelated turn must not be able to end the goal.
func TestNoteGoalOutputOnlyWatchesInjectedTurn(t *testing.T) {
	m := NewManager(nil, nil)
	const sid = "s1"

	// User turn: goalAwait is false, so a stray marker is ignored.
	m.noteGoalOutput(sid, "随便说一句 "+goalMarkerMet)
	if v := m.rt(sid).goalVerdict; v != goalVerdictNone {
		t.Fatalf("非注入轮次不应采信 marker, verdict = %q", v)
	}

	r := m.rt(sid)
	r.mu.Lock()
	r.goalAwait = true
	r.mu.Unlock()

	m.noteGoalOutput(sid, "干了活，结论是"+goalMarkerMet)
	if v := m.rt(sid).goalVerdict; v != goalVerdictMet {
		t.Fatalf("注入轮次应采信 met marker, verdict = %q", v)
	}
}

func TestGoalVerdictBothWays(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{"met", "做完了\n" + goalMarkerMet, goalVerdictMet},
		{"unmet 带理由", "还差测试\n" + goalMarkerUnmet + " 单测没过", goalVerdictUnmet},
		{"什么都没说", "我在干活", goalVerdictNone},
	} {
		m := NewManager(nil, nil)
		sid := "s-" + tc.name
		r := m.rt(sid)
		r.goalAwait = true
		m.noteGoalOutput(sid, tc.text)
		if got := m.rt(sid).goalVerdict; got != tc.want {
			t.Errorf("%s: verdict = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The marker can straddle streamed deltas, so scanning only the newest chunk
// would miss it. The rolling tail is what makes this work.
func TestGoalMarkerSplitAcrossDeltas(t *testing.T) {
	m := NewManager(nil, nil)
	const sid = "split"
	m.rt(sid).goalAwait = true

	full := "一段很长的收尾说明。" + goalMarkerMet
	// 故意在 marker 中间切开
	m.noteGoalOutput(sid, full[:len(full)-8])
	if v := m.rt(sid).goalVerdict; v != goalVerdictNone {
		t.Fatalf("前半段不应判定, verdict = %q", v)
	}
	m.noteGoalOutput(sid, full[len(full)-8:])
	if v := m.rt(sid).goalVerdict; v != goalVerdictMet {
		t.Fatalf("跨 delta 的 marker 应被识别, verdict = %q", v)
	}
}

func TestGoalTailIsBounded(t *testing.T) {
	m := NewManager(nil, nil)
	const sid = "tail"
	m.rt(sid).goalAwait = true
	m.noteGoalOutput(sid, string(make([]byte, goalTailBudget*3)))
	if n := len(m.rt(sid).goalTail); n > goalTailBudget {
		t.Fatalf("tail 超过预算: %d > %d", n, goalTailBudget)
	}
}

func TestResetGoalTurnKeepsAwait(t *testing.T) {
	m := NewManager(nil, nil)
	const sid = "keep"
	r := m.rt(sid)
	r.goalAwait = true
	r.goalVerdict = goalVerdictMet
	r.goalTail = "残留"

	m.resetGoalTurn(sid)

	if !m.rt(sid).goalAwait {
		t.Fatal("resetGoalTurn 不该清掉 goalAwait：worker 可能在另一条 goroutine 上已取到该轮")
	}
	if m.rt(sid).goalVerdict != goalVerdictNone || m.rt(sid).goalTail != "" {
		t.Fatal("resetGoalTurn 应清掉上一轮的 verdict/tail")
	}

	m.clearGoalState(sid)
	if m.rt(sid).goalAwait {
		t.Fatal("clearGoalState 应连 goalAwait 一起清掉")
	}
}

func TestGoalPromptCarriesContractAndLimits(t *testing.T) {
	g := &store.Goal{Text: "go test ./... 全绿且 ./build 退出码为 0", MaxTurns: 7, MaxHours: 1.5}
	if err := g.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	p := goalPrompt(g, 3)
	for _, want := range []string{g.Text, "第 3/7 轮", goalMarkerMet, goalMarkerUnmet, "最多 7 轮"} {
		if !contains(p, want) {
			t.Errorf("续跑 prompt 缺少 %q\n---\n%s", want, p)
		}
	}
}

func TestCircuitBreakerStops(t *testing.T) {
	now := time.Now()
	g := &store.Goal{Text: "x", MaxTurns: 3, MaxHours: 1, CreatedAt: now}

	if g.Exhausted(now) {
		t.Fatal("刚开始不该算耗尽")
	}
	g.Rounds = 3
	if !g.Exhausted(now) || g.StopReason(now) == "" {
		t.Fatal("达到 MaxTurns 应熔断并给出原因")
	}
	g.Rounds = 0
	if !g.Exhausted(now.Add(2 * time.Hour)) {
		t.Fatal("超过 MaxHours 应熔断")
	}
	if r := g.StopReason(now.Add(2 * time.Hour)); r == "" {
		t.Fatal("超时熔断应给出原因")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// 回归：advanceGoal 由 setIdle 在 work() 内部触发，此时 r.working 仍为 true。
// 若用 Busy() 判「用户有消息在排队」，Busy 恒为真，目标永远不会推进。
func TestGoalSeesQueuedLengthNotBusy(t *testing.T) {
	m := NewManager(nil, nil)
	const sid = "busy"
	r := m.rt(sid)
	// 复刻 work() 中途的状态：working=true 但队列已空
	r.working = true
	if !m.Busy(sid) {
		t.Fatal("前置：working=true 时 Busy 应为真")
	}
	r.mu.Lock()
	queued := len(r.queue)
	r.mu.Unlock()
	if queued != 0 {
		t.Fatalf("队列为空时 queued 应为 0, got %d", queued)
	}

	// 用户自己排了一条时，queued 才非零
	r.mu.Lock()
	r.queue = append(r.queue, Message{Text: "user"})
	r.mu.Unlock()
	r.mu.Lock()
	queued = len(r.queue)
	r.mu.Unlock()
	if queued == 0 {
		t.Fatal("有排队消息时 queued 应非零")
	}
}
