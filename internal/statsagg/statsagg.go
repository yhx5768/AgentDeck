// Package statsagg aggregates per-session usage statistics into a single Stats
// document. It is engine-neutral: each engine contributes one FileAgg per
// session (claudehist scans Claude Code's own jsonl transcripts, opencodehist
// reads `opencode export`), and Recompute merges them into the final numbers.
// The output shapes mirror cc-haha's activity API so the frontend tiles and
// heatmap logic port over unchanged.
package statsagg

import (
	"sort"
	"time"
)

// ActiveSessionGapMs: gaps between adjacent messages up to this are counted
// toward a session's active duration, so overnight pauses are not.
const ActiveSessionGapMs = 30 * 60 * 1000

// DailyActivity 是单日的会话/消息/工具调用统计。日期按每条消息自身时间戳
// 的时区归桶，所以跨多天的长会话会摊到各个活跃日。
type DailyActivity struct {
	Date          string `json:"date"` // YYYY-MM-DD
	SessionCount  int    `json:"session_count"`
	MessageCount  int    `json:"message_count"`
	ToolCallCount int    `json:"tool_call_count"`
}

// DailyModelTokens 是单日的按模型 token 用量。assistant 回复按 message.id
// 去重——一个 API step 的多个内容块行会重复携带同一份 usage。
type DailyModelTokens struct {
	Date          string           `json:"date"`
	TokensByModel map[string]int64 `json:"tokens_by_model"`
}

// StreakInfo 是连续活跃天数。
type StreakInfo struct {
	Current int `json:"current"`
	Longest int `json:"longest"`
}

// ModelUsage 是单模型的累计 token 用量。ReasoningTokens 只有 opencode 引擎
// 会上报（Claude 的 stream-json usage 不含 reasoning 拆分），claude 恒为 0。
type ModelUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	Messages            int64 `json:"messages"`
}

// TotalTokens is the model's full token volume (the daily totals include
// reasoning, so the per-model total does too).
func (m ModelUsage) TotalTokens() int64 {
	return m.InputTokens + m.OutputTokens + m.ReasoningTokens + m.CacheReadTokens + m.CacheCreationTokens
}

// SessionDuration 记录最长会话的时长：按 30 分钟内相邻消息的间隔累计，
// 不含过夜等长时间停歇（last-first 会把跨天会话算成"436 小时"）。
type SessionDuration struct {
	SessionID    string `json:"session_id"`
	DurationMs   int64  `json:"duration_ms"`
	MessageCount int    `json:"message_count"`
}

// Stats 是全部会话（claude 与 opencode 两引擎合并）的使用统计。字段与
// cc-haha 的活动统计 API 对齐，方便热力图等前端逻辑直接移植。
type Stats struct {
	TotalSessions    int                   `json:"total_sessions"`
	TotalMessages    int                   `json:"total_messages"`
	TotalDays        int                   `json:"total_days"`
	ActiveDays       int                   `json:"active_days"`
	Streaks          StreakInfo            `json:"streaks"`
	DailyActivity    []DailyActivity       `json:"daily_activity"`
	DailyModelTokens []DailyModelTokens    `json:"daily_model_tokens"`
	ModelUsage       map[string]ModelUsage `json:"model_usage"`
	ToolUsage        map[string]int64      `json:"tool_usage"`
	FirstSessionDate string                `json:"first_session_date"`
	LastSessionDate  string                `json:"last_session_date"`
	PeakActivityDay  string                `json:"peak_activity_day"`
	PeakActivityHour *int                  `json:"peak_activity_hour"`
	LongestSession   *SessionDuration      `json:"longest_session"`
	FavoriteModel    string                `json:"favorite_model"`
	// Engines 是各引擎贡献的会话数（key 为引擎 kind，如 "claude"/"opencode"）。
	Engines map[string]int `json:"engines"`
}

// FileAgg is the self-contained per-session contribution to the global stats.
// Keeping each session's aggregate separate lets a cache recompute totals from
// per-session results and only rescan sessions that changed.
type FileAgg struct {
	FileID     string // session id; shown as LongestSession.SessionID
	Path       string // unique per-session key; per-day session-dedup key
	Engine     string // engine kind: "claude" | "opencode"
	Active     bool   // Counted > 0 → counts toward TotalSessions
	Counted    int    // user + deduped assistant messages
	DurationMs int64
	Hour       int                         // first counted message hour; -1 if none
	DayMsg     map[string]int              // date -> message count
	DayTool    map[string]int              // date -> tool call count
	DayTokens  map[string]map[string]int64 // date -> model -> total tokens
	ModelUsage map[string]ModelUsage       // model -> cumulative usage
	ToolUsage  map[string]int64            // tool -> call count
}

// NewFileAgg returns an initialised FileAgg with all maps allocated.
func NewFileAgg(fileID, path, engine string) FileAgg {
	return FileAgg{
		FileID:     fileID,
		Path:       path,
		Engine:     engine,
		Hour:       -1,
		DayMsg:     map[string]int{},
		DayTool:    map[string]int{},
		DayTokens:  map[string]map[string]int64{},
		ModelUsage: map[string]ModelUsage{},
		ToolUsage:  map[string]int64{},
	}
}

// Sorted returns the aggregates ordered by FileID, so ties between equally
// long sessions resolve the same way on every refresh.
func Sorted(fas []FileAgg) []FileAgg {
	out := make([]FileAgg, len(fas))
	copy(out, fas)
	sort.Slice(out, func(i, j int) bool { return out[i].FileID < out[j].FileID })
	return out
}

// Recompute merges per-session aggregates into the final Stats. Cheap: only
// arithmetic over sessions, no JSON parsing.
func Recompute(fas []FileAgg) *Stats {
	stats := &Stats{
		ModelUsage: map[string]ModelUsage{},
		ToolUsage:  map[string]int64{},
		Engines:    map[string]int{},
	}
	dayActivity := map[string]*DailyActivity{}
	daySessions := map[string]map[string]bool{} // date -> set(sessionKey)
	dayTokens := map[string]map[string]int64{}  // date -> model -> tokens
	hourCounts := map[int]int{}

	for _, fa := range Sorted(fas) {
		if !fa.Active {
			continue
		}
		stats.TotalSessions++
		if fa.Engine != "" {
			stats.Engines[fa.Engine]++
		}
		stats.TotalMessages += fa.Counted
		for date, n := range fa.DayMsg {
			dayActivityOf(date, dayActivity).MessageCount += n
			daySessionAdd(date, daySessions, fa.Path)
		}
		for date, n := range fa.DayTool {
			dayActivityOf(date, dayActivity).ToolCallCount += n
		}
		for date, tm := range fa.DayTokens {
			dst := dayTokens[date]
			if dst == nil {
				dst = map[string]int64{}
				dayTokens[date] = dst
			}
			for model, t := range tm {
				dst[model] += t
			}
		}
		for model, m := range fa.ModelUsage {
			cur := stats.ModelUsage[model]
			cur.InputTokens += m.InputTokens
			cur.OutputTokens += m.OutputTokens
			cur.ReasoningTokens += m.ReasoningTokens
			cur.CacheReadTokens += m.CacheReadTokens
			cur.CacheCreationTokens += m.CacheCreationTokens
			cur.Messages += m.Messages
			stats.ModelUsage[model] = cur
		}
		for tool, n := range fa.ToolUsage {
			stats.ToolUsage[tool] += n
		}
		if fa.Hour >= 0 {
			hourCounts[fa.Hour]++
		}
		if stats.LongestSession == nil || fa.DurationMs > stats.LongestSession.DurationMs ||
			(fa.DurationMs == stats.LongestSession.DurationMs && fa.FileID < stats.LongestSession.SessionID) {
			stats.LongestSession = &SessionDuration{
				SessionID:    fa.FileID,
				DurationMs:   fa.DurationMs,
				MessageCount: fa.Counted,
			}
		}
	}

	dayDates := sortedKeys(dayActivity)
	for _, date := range dayDates {
		dayActivity[date].SessionCount = len(daySessions[date])
		stats.DailyActivity = append(stats.DailyActivity, *dayActivity[date])
	}
	tokenDates := sortedKeys(dayTokens)
	for _, date := range tokenDates {
		stats.DailyModelTokens = append(stats.DailyModelTokens, DailyModelTokens{
			Date:          date,
			TokensByModel: dayTokens[date],
		})
	}

	if len(dayDates) == 0 && len(tokenDates) == 0 {
		return stats
	}
	activeDates := dayDates
	if len(activeDates) == 0 {
		activeDates = tokenDates
	}
	stats.FirstSessionDate = activeDates[0]
	stats.LastSessionDate = activeDates[len(activeDates)-1]
	stats.ActiveDays = len(activeDates)
	stats.TotalDays = daysBetween(activeDates[0], activeDates[len(activeDates)-1]) + 1
	stats.Streaks = CalculateStreaks(activeDates)

	// 峰值日：单日消息数最多的一天；峰值时段：每会话第一条消息所属小时
	peak := dayActivity[activeDates[0]]
	for _, date := range activeDates {
		d := dayActivity[date]
		if d != nil && peak != nil && d.MessageCount > peak.MessageCount {
			peak = d
		}
	}
	if peak != nil {
		stats.PeakActivityDay = peak.Date
	}
	maxHourCount, maxHour := -1, -1
	for hour, cnt := range hourCounts {
		if cnt > maxHourCount || (cnt == maxHourCount && hour < maxHour) {
			maxHourCount, maxHour = cnt, hour
		}
	}
	if maxHour >= 0 {
		stats.PeakActivityHour = &maxHour
	}

	// 常用模型：token 总量最大（按模型名排序遍历，并列时稳定取字典序靠前者）
	maxTokens := int64(0)
	for _, model := range sortedKeys(stats.ModelUsage) {
		if total := stats.ModelUsage[model].TotalTokens(); total > maxTokens {
			maxTokens = total
			stats.FavoriteModel = model
		}
	}
	return stats
}

func dayActivityOf(date string, dayActivity map[string]*DailyActivity) *DailyActivity {
	d := dayActivity[date]
	if d == nil {
		d = &DailyActivity{Date: date}
		dayActivity[date] = d
	}
	return d
}

func daySessionAdd(date string, daySessions map[string]map[string]bool, sessionKey string) {
	ids := daySessions[date]
	if ids == nil {
		ids = map[string]bool{}
		daySessions[date] = ids
	}
	ids[sessionKey] = true
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// daysBetween 返回两个 YYYY-MM-DD 日期相差的天数（from <= to）。
func daysBetween(from, to string) int {
	f, err1 := time.Parse("2006-01-02", from)
	t, err2 := time.Parse("2006-01-02", to)
	if err1 != nil || err2 != nil {
		return 0
	}
	return int(t.Sub(f).Hours() / 24)
}

// CalculateStreaks 计算当前/最长连续活跃天数。当前连续从今天起往前数
// （今天无活动则从昨天起数），中断后归零。
func CalculateStreaks(dates []string) StreakInfo {
	if len(dates) == 0 {
		return StreakInfo{}
	}
	active := map[string]bool{}
	for _, d := range dates {
		active[d] = true
	}
	longest, run := 0, 0
	prev := ""
	for _, d := range dates {
		if prev != "" && daysBetween(prev, d) == 1 {
			run++
		} else {
			run = 1
		}
		if run > longest {
			longest = run
		}
		prev = d
	}
	// 当前连续：今天活跃从今天数，不活跃从昨天数；昨天也不活跃则断签
	cursor := time.Now().Format("2006-01-02")
	if !active[cursor] {
		cursor = time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	}
	current := 0
	for active[cursor] {
		current++
		cursor = AddDateKey(cursor, -1)
	}
	return StreakInfo{Current: current, Longest: longest}
}

// AddDateKey 对 YYYY-MM-DD 日期做加减天。
func AddDateKey(key string, delta int) string {
	t, err := time.Parse("2006-01-02", key)
	if err != nil {
		return key
	}
	return t.AddDate(0, 0, delta).Format("2006-01-02")
}
