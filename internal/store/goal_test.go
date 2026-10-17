package store

import (
	"testing"
	"time"
)

func TestGoalValidateDefaults(t *testing.T) {
	g := &Goal{Text: "  测试全绿  "}
	if err := g.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if g.Text != "测试全绿" {
		t.Errorf("Text = %q, want 去空格后的值", g.Text)
	}
	if g.MaxTurns != DefaultGoalMaxTurns || g.MaxHours != DefaultGoalMaxHours {
		t.Errorf("默认熔断没填上: turns=%d hours=%g", g.MaxTurns, g.MaxHours)
	}
	if g.Status != GoalActive {
		t.Errorf("Status = %q, want %q", g.Status, GoalActive)
	}
}

func TestGoalValidateRejectsEmpty(t *testing.T) {
	if err := (&Goal{Text: "   "}).Validate(); err == nil {
		t.Fatal("空条件应报错")
	}
}

// Get/List 返回的是 session 的浅拷贝。Goal 若是指针而不深拷贝，调用方改一下
// 返回值里的 goal 就会直接写穿到库里——这是最容易埋进去、也最难查的一类 bug。
func TestGoalIsNotAliasedOutByGetOrList(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert(&Session{ID: "a", Name: "a"}, nil); err != nil {
		t.Fatal(err)
	}
	g := &Goal{Text: "原条件", MaxTurns: 5, MaxHours: 1, Status: GoalActive, CreatedAt: time.Now()}
	if err := st.SetGoal("a", g); err != nil {
		t.Fatal(err)
	}

	// 1) Get
	got := st.Get("a")
	got.Goal.Text = "被调用方改了"
	got.Goal.Rounds = 99
	if again := st.Get("a"); again.Goal.Text != "原条件" || again.Goal.Rounds != 0 {
		t.Fatalf("Get 泄漏了 goal 指针: %+v", again.Goal)
	}

	// 2) List
	for _, s := range st.List() {
		if s.Goal != nil {
			s.Goal.Text = "又被改了"
		}
	}
	if again := st.Get("a"); again.Goal.Text != "原条件" {
		t.Fatalf("List 泄漏了 goal 指针: %+v", again.Goal)
	}

	// 3) SetGoal 也要拷贝入参，否则调用方后续改动会反写
	in := &Goal{Text: "第二个条件", Status: GoalActive, CreatedAt: time.Now()}
	if err := st.SetGoal("a", in); err != nil {
		t.Fatal(err)
	}
	in.Text = "调用方事后改的"
	if again := st.Get("a"); again.Goal.Text != "第二个条件" {
		t.Fatalf("SetGoal 未拷贝入参: %+v", again.Goal)
	}
}

func TestMutateGoal(t *testing.T) {
	st, _ := Open(t.TempDir())
	_ = st.Upsert(&Session{ID: "a"}, nil)
	_ = st.SetGoal("a", &Goal{Text: "x", Status: GoalActive, MaxTurns: 3, MaxHours: 1, CreatedAt: time.Now()})

	g, err := st.MutateGoal("a", func(g *Goal) { g.Rounds++; g.LastReason = "还差一步" })
	if err != nil {
		t.Fatal(err)
	}
	if g.Rounds != 1 || g.LastReason != "还差一步" {
		t.Fatalf("mutate 结果不对: %+v", g)
	}
	if again := st.Get("a"); again.Goal.Rounds != 1 {
		t.Fatalf("mutate 没落库: %+v", again.Goal)
	}

	// 先清掉 goal，再确认 mutate 不会凭空造一个出来
	if err := st.SetGoal("a", nil); err != nil {
		t.Fatal(err)
	}
	if g, err := st.MutateGoal("a", func(g *Goal) { g.Rounds = 7 }); err != nil || g != nil {
		t.Fatalf("无 goal 时 MutateGoal 应是 no-op, got %+v err=%v", g, err)
	}
	if st.Get("a").Goal != nil {
		t.Fatal("无 goal 的 mutate 不该造出 goal")
	}
	// 会话不存在时也不能造
	if g, err := st.MutateGoal("nope", func(g *Goal) {}); err != nil || g != nil {
		t.Fatalf("不存在会话的 MutateGoal 应是 no-op, got %+v err=%v", g, err)
	}
}

func TestSetGoalNilClears(t *testing.T) {
	st, _ := Open(t.TempDir())
	_ = st.Upsert(&Session{ID: "a"}, nil)
	_ = st.SetGoal("a", &Goal{Text: "x", Status: GoalActive, CreatedAt: time.Now()})
	if st.Get("a").Goal == nil {
		t.Fatal("前置：goal 应存在")
	}
	if err := st.SetGoal("a", nil); err != nil {
		t.Fatal(err)
	}
	if st.Get("a").Goal != nil {
		t.Fatal("SetGoal(nil) 应清掉 goal")
	}
}

// 目标每轮都会动，但不该把会话顶到列表最前面。
func TestGoalUpdatesDoNotBumpUpdatedAt(t *testing.T) {
	st, _ := Open(t.TempDir())
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = st.Upsert(&Session{ID: "a", UpdatedAt: base}, nil)
	_ = st.SetGoal("a", &Goal{Text: "x", Status: GoalActive, CreatedAt: base})
	if got := st.Get("a").UpdatedAt; !got.Equal(base) {
		t.Fatalf("SetGoal 动了 UpdatedAt: %v", got)
	}
	if _, err := st.MutateGoal("a", func(g *Goal) { g.Rounds++ }); err != nil {
		t.Fatal(err)
	}
	if got := st.Get("a").UpdatedAt; !got.Equal(base) {
		t.Fatalf("MutateGoal 动了 UpdatedAt: %v", got)
	}
}

func TestGoalPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir)
	_ = st.Upsert(&Session{ID: "a"}, nil)
	_ = st.SetGoal("a", &Goal{Text: "持久化条件", MaxTurns: 9, MaxHours: 2, Rounds: 3, Status: GoalActive, CreatedAt: time.Now()})

	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := st2.Get("a").Goal
	if g == nil || g.Text != "持久化条件" || g.MaxTurns != 9 || g.Rounds != 3 {
		t.Fatalf("重开后 goal 丢了: %+v", g)
	}
}

func TestGoalExhaustedAndReason(t *testing.T) {
	now := time.Now()
	var nilGoal *Goal
	if nilGoal.Exhausted(now) || nilGoal.Active() || nilGoal.StopReason(now) != "" {
		t.Fatal("nil goal 应处处安全返回零值")
	}
	g := &Goal{Text: "x", MaxTurns: 2, MaxHours: 1, Status: GoalActive, CreatedAt: now}
	if !g.Active() {
		t.Fatal("active goal 应 Active()")
	}
	if g.Exhausted(now) {
		t.Fatal("未到上限不该熔断")
	}
	g.Rounds = 2
	if !g.Exhausted(now) || g.StopReason(now) == "" {
		t.Fatal("轮数用尽应熔断")
	}
	// 终态目标不该再 Active
	g.Status = GoalMet
	if g.Active() {
		t.Fatal("met 目标不该 Active")
	}
}
