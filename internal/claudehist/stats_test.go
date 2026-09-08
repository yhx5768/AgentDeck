package claudehist

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cc-webui/internal/statsagg"
)

// writeStatsHist 在 root/<slug>/<sid>.jsonl 写入原始行（root 为聚合根目录
// 的替身，与 AggregateStats(root) 的参数对应）。
func writeStatsHist(t *testing.T, root, slug, sid string, lines []string) {
	t.Helper()
	dir := filepath.Join(root, slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := ""
	for _, l := range lines {
		raw += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestStatsCacheIncremental 验证增量刷新与全量刷新结果一致：首扫全量、
// 改一个文件后增量只重扫该文件、删一个文件后增量剔除，结果都等于 fresh 全扫。
func TestStatsCacheIncremental(t *testing.T) {
	root := t.TempDir()
	linesA := []string{
		`{"type":"user","uuid":"u1","timestamp":"2026-08-30T10:00:00+08:00","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-08-30T10:00:01+08:00","message":{"id":"m1","model":"opus","role":"assistant","content":[{"type":"text","text":"yo"}],"usage":{"input_tokens":10,"output_tokens":5}}}`,
	}
	linesB := []string{
		`{"type":"user","uuid":"u2","timestamp":"2026-08-31T09:00:00+08:00","message":{"role":"user","content":"hey"}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-08-31T09:00:01+08:00","message":{"id":"m2","model":"sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":3,"output_tokens":2}}}`,
	}
	writeStatsHist(t, root, "p1", "aaa", linesA)
	writeStatsHist(t, root, "p2", "bbb", linesB)

	c := NewStatsCache()
	// 首次刷新（非 fresh）：缓存为空，等同于全扫。
	s1 := statsagg.Recompute(c.Refresh(root, false))
	if !reflect.DeepEqual(s1, AggregateStats(root)) {
		t.Fatal("首次刷新应等于全量扫描结果")
	}
	if s1.TotalSessions != 2 || s1.TotalMessages != 4 {
		t.Fatalf("首次 = %d sessions / %d msgs, want 2/4", s1.TotalSessions, s1.TotalMessages)
	}

	// 给 A 追加一条消息（mtime+size 变化），增量刷新应只重扫 A。
	writeStatsHist(t, root, "p1", "aaa", append(linesA,
		`{"type":"user","uuid":"u3","timestamp":"2026-08-30T10:05:00+08:00","message":{"role":"user","content":"more"}}`))
	s2 := statsagg.Recompute(c.Refresh(root, false))
	s2fresh := AggregateStats(root)
	if !reflect.DeepEqual(s2, s2fresh) {
		t.Fatalf("增量刷新 != 全量刷新：got %+v, want %+v", s2, s2fresh)
	}
	if s2.TotalMessages != 5 || s2.TotalSessions != 2 {
		t.Fatalf("增量后 = %d sessions / %d msgs, want 2/5", s2.TotalSessions, s2.TotalMessages)
	}

	// 删除 B 的文件，增量刷新应剔除 B。
	if err := os.Remove(filepath.Join(root, "p2", "bbb.jsonl")); err != nil {
		t.Fatal(err)
	}
	s3 := statsagg.Recompute(c.Refresh(root, false))
	if !reflect.DeepEqual(s3, AggregateStats(root)) {
		t.Fatal("删除文件后增量刷新 != 全量刷新")
	}
	if s3.TotalSessions != 1 {
		t.Fatalf("删除后 sessions = %d, want 1", s3.TotalSessions)
	}

	// fresh=true 强制清空缓存全扫，仍一致。
	if got := statsagg.Recompute(c.Refresh(root, true)); !reflect.DeepEqual(got, s3) {
		t.Fatal("fresh 强制全扫结果与增量不一致")
	}
}

func TestAggregateStatsDailyAndTokens(t *testing.T) {
	root := t.TempDir()
	// 会话 aaa：跨天（8-30 与 8-31 都有消息），assistant 拆成 3 行共享 message.id m1
	writeStatsHist(t, root, "proj1", "aaa", []string{
		`{"type":"user","uuid":"u1","timestamp":"2026-08-30T10:00:00+08:00","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-08-30T10:00:01+08:00","message":{"id":"m1","model":"claude-opus-4-5","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":5,"cache_creation_input_tokens":3}}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-08-30T10:00:01+08:00","message":{"id":"m1","model":"claude-opus-4-5","role":"assistant","content":[{"type":"thinking","thinking":"..."}],"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":5,"cache_creation_input_tokens":3}}}`,
		`{"type":"assistant","uuid":"a3","timestamp":"2026-08-30T10:00:02+08:00","message":{"id":"m1","model":"claude-opus-4-5","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash"}]}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-08-31T09:00:00+08:00","message":{"role":"user","content":"continue"}}`,
		`{"type":"assistant","uuid":"a4","timestamp":"2026-08-31T09:00:30+08:00","message":{"id":"m2","model":"claude-opus-4-5","role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":1}}}`,
	})
	// 会话 bbb：只有 8-31 一天
	writeStatsHist(t, root, "proj2", "bbb", []string{
		`{"type":"user","uuid":"u3","timestamp":"2026-08-31T11:00:00+08:00","message":{"role":"user","content":"other"}}`,
		`{"type":"assistant","uuid":"a5","timestamp":"2026-08-31T11:00:01+08:00","message":{"id":"m3","model":"small","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":100,"output_tokens":50}}}`,
	})

	got := AggregateStats(root)

	if got.TotalSessions != 2 {
		t.Fatalf("TotalSessions = %d, want 2", got.TotalSessions)
	}
	if got.TotalMessages != 6 {
		t.Fatalf("TotalMessages = %d, want 6（u1+a1+u2+a2? -> u1,m1,u2,m2,u3,m3）", got.TotalMessages)
	}
	if got.ActiveDays != 2 || got.TotalDays != 2 {
		t.Fatalf("ActiveDays/TotalDays = %d/%d, want 2/2", got.ActiveDays, got.TotalDays)
	}
	if got.FirstSessionDate != "2026-08-30" || got.LastSessionDate != "2026-08-31" {
		t.Fatalf("first/last = %q/%q", got.FirstSessionDate, got.LastSessionDate)
	}
	if len(got.DailyActivity) != 2 {
		t.Fatalf("dailyActivity len = %d, want 2", len(got.DailyActivity))
	}
	d30, d31 := got.DailyActivity[0], got.DailyActivity[1]
	if d30.Date != "2026-08-30" || d30.SessionCount != 1 || d30.MessageCount != 2 || d30.ToolCallCount != 1 {
		t.Fatalf("day30 = %+v", d30)
	}
	if d31.Date != "2026-08-31" || d31.SessionCount != 2 || d31.MessageCount != 4 || d31.ToolCallCount != 0 {
		t.Fatalf("day31 = %+v", d31)
	}
	// m1 的 usage 被 3 行重复携带，应只计一次：10+20+5+3=38
	if len(got.DailyModelTokens) != 2 {
		t.Fatalf("dailyModelTokens len = %d, want 2", len(got.DailyModelTokens))
	}
	if tok := got.DailyModelTokens[0].TokensByModel["claude-opus-4-5"]; tok != 38 {
		t.Fatalf("day30 tokens = %d, want 38（usage 需按 message.id 去重）", tok)
	}
	if tok := got.DailyModelTokens[1].TokensByModel["small"]; tok != 150 {
		t.Fatalf("day31 small tokens = %d, want 150", tok)
	}
	if got.ModelUsage["claude-opus-4-5"].Messages != 2 || got.ModelUsage["claude-opus-4-5"].InputTokens != 17 {
		t.Fatalf("opus usage = %+v", got.ModelUsage["claude-opus-4-5"])
	}
	if got.ModelUsage["small"].InputTokens != 100 || got.ModelUsage["small"].OutputTokens != 50 {
		t.Fatalf("small usage = %+v", got.ModelUsage["small"])
	}
	if got.ToolUsage["Bash"] != 1 {
		t.Fatalf("ToolUsage[Bash] = %d, want 1", got.ToolUsage["Bash"])
	}
	if got.FavoriteModel != "small" {
		t.Fatalf("FavoriteModel = %q, want small（150 > opus 51）", got.FavoriteModel)
	}
	if got.PeakActivityDay != "2026-08-31" {
		t.Fatalf("PeakActivityDay = %q", got.PeakActivityDay)
	}
	if got.PeakActivityHour == nil || *got.PeakActivityHour != 10 {
		t.Fatalf("PeakActivityHour = %v, want 10（aaa 首条消息 10 点）", got.PeakActivityHour)
	}
	if got.LongestSession == nil || got.LongestSession.DurationMs != 31000 {
		t.Fatalf("LongestSession = %+v, want 31000ms（aaa 三条 30s 间隔内）", got.LongestSession)
	}
}

func TestAggregateStatsIgnoresNoiseLines(t *testing.T) {
	root := t.TempDir()
	writeStatsHist(t, root, "proj", "n1", []string{
		`{"type":"user","uuid":"u1","timestamp":"2026-08-30T10:00:00+08:00","message":{"role":"user","content":"hello"},"isSidechain":true}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-08-30T10:00:01+08:00","message":{"role":"user","content":"<task-notification>..."}}`,
		`{"type":"user","uuid":"u3","timestamp":"2026-08-30T10:00:02+08:00","message":{"role":"user","content":"[Request interrupted by user"}}`,
		`{"type":"user","uuid":"u4","timestamp":"2026-08-30T10:00:03+08:00","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"done"}]}}`,
		`{"type":"user","uuid":"u5","timestamp":"2026-08-30T10:01:00+08:00","message":{"role":"user","content":"ok"}}`,
	})
	got := AggregateStats(root)
	if got.TotalSessions != 1 || got.TotalMessages != 1 {
		t.Fatalf("TotalSessions/Messages = %d/%d, want 1/1", got.TotalSessions, got.TotalMessages)
	}
	if got.DailyActivity[0].MessageCount != 1 {
		t.Fatalf("messageCount = %d, want 1（全被过滤只剩 u5）", got.DailyActivity[0].MessageCount)
	}
}

func TestAggregateStatsEmptyRoot(t *testing.T) {
	got := AggregateStats(t.TempDir())
	if got.TotalSessions != 0 || got.ActiveDays != 0 || len(got.DailyActivity) != 0 {
		t.Fatalf("empty stats = %+v", got)
	}
}

func TestCalculateStreaks(t *testing.T) {
	// 以真实今日为锚（calculateStreaks 内部用 time.Now()，硬编码日期会随日历漂移）
	today := time.Now().Format("2006-01-02")
	// 合法 key 由 helpers 计算（避免测试里手写日期偏移出错）
	yesterday := statsagg.AddDateKey(today, -1)
	day3 := statsagg.AddDateKey(today, -3)

	// today, yesterday, day3 → 当前连续 2，最长 2
	got := statsagg.CalculateStreaks([]string{day3, yesterday, today})
	if got.Current != 2 || got.Longest != 2 {
		t.Fatalf("streaks = %+v, want {2,2}", got)
	}
	// 只有 day3（今天/昨天无活动）→ 当前 0
	got = statsagg.CalculateStreaks([]string{day3})
	if got.Current != 0 || got.Longest != 1 {
		t.Fatalf("streaks = %+v, want {0,1}", got)
	}
	// 连续三天 → {3,3}
	got = statsagg.CalculateStreaks([]string{statsagg.AddDateKey(today, -2), yesterday, today})
	if got.Current != 3 || got.Longest != 3 {
		t.Fatalf("streaks = %+v, want {3,3}", got)
	}
}

func TestPreviewLastMessages(t *testing.T) {
	root := t.TempDir()
	writeStatsHist(t, root, "proj", "sid1", []string{
		`{"type":"user","uuid":"u1","timestamp":"2026-08-30T10:00:00+08:00","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-08-30T10:00:01+08:00","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"reply one"}]}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-08-30T10:00:01+08:00","message":{"id":"m1","role":"assistant","content":[{"type":"thinking","thinking":"..."}]}}`,
		`{"type":"user","uuid":"tr1","timestamp":"2026-08-30T10:00:02+08:00","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"result"}]}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-08-31T09:00:00+08:00","message":{"role":"user","content":"second"}}`,
		`{"type":"assistant","uuid":"a3","timestamp":"2026-08-31T09:00:10+08:00","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"final answer"}]}}`,
	})
	got := previewFile(filepath.Join(root, "proj", "sid1.jsonl"), 4)
	if len(got) != 4 {
		t.Fatalf("Preview len = %d, want 4（u1, m1, u2, m2）", len(got))
	}
	if got[0].Role != "user" || got[0].Text != "first" {
		t.Fatalf("got[0] = %+v", got[0])
	}
	if got[1].Role != "assistant" || got[1].Text != "reply one" {
		t.Fatalf("got[1] = %+v（m1 折叠为一条）", got[1])
	}
	if got[2].Role != "user" || got[2].Text != "second" {
		t.Fatalf("got[2] = %+v", got[2])
	}
	if got[3].Role != "assistant" || got[3].Text != "final answer" {
		t.Fatalf("got[3] = %+v", got[3])
	}
	// max 截断取最近
	short := previewFile(filepath.Join(root, "proj", "sid1.jsonl"), 2)
	if len(short) != 2 || short[1].Text != "final answer" {
		t.Fatalf("short = %+v", short)
	}
}
