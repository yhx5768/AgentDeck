package claudehist

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

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

// ModelUsage 是单模型的累计 token 用量。
type ModelUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	Messages            int64 `json:"messages"`
}

// SessionDuration 记录最长会话的时长：按 30 分钟内相邻消息的间隔累计，
// 不含过夜等长时间停歇（last-first 会把跨天会话算成"436 小时"）。
type SessionDuration struct {
	SessionID    string `json:"session_id"`
	DurationMs   int64  `json:"duration_ms"`
	MessageCount int    `json:"message_count"`
}

// Stats 是全部 on-disk 会话的使用统计。字段与 cc-haha 的活动统计 API 对齐，
// 方便热力图等前端逻辑直接移植。
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
}

// 相邻消息间隔不超过该值才计入会话活跃时长（与 cc-haha 一致）。
const activeSessionGapMs = 30 * 60 * 1000

type usagelike struct {
	InputTokens              int64  `json:"input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheCreation            *struct {
		Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// cacheCreationTokens 与 cc-haha 一致：API 给了 5m/1h 拆分时用拆分，
// 否则回退到旧的聚合字段。
func (u *usagelike) cacheCreationTokens() int64 {
	if u.CacheCreation != nil {
		return u.CacheCreation.Ephemeral5mInputTokens + u.CacheCreation.Ephemeral1hInputTokens
	}
	if u.CacheCreationInputTokens != nil {
		return *u.CacheCreationInputTokens
	}
	return 0
}

func (u *usagelike) total() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.cacheCreationTokens()
}

// fileAgg is the self-contained per-session contribution to the global stats.
// Keeping each session's aggregate separate lets the cache recompute totals
// from cached per-file results without re-parsing jsonl: only sessions whose
// mtime/size changed are rescanned.
type fileAgg struct {
	fileID    string                  // session id (filename without .jsonl); LongestSession display
	path      string                  // full jsonl path; per-day session-dedup key
	active    bool                    // counted > 0 → counts toward TotalSessions
	counted   int                     // user + deduped assistant messages
	durationMs int64
	hour      int                     // first counted message hour; -1 if none
	dayMsg    map[string]int          // date -> message count
	dayTool   map[string]int          // date -> tool call count
	dayTokens map[string]map[string]int64 // date -> model -> total tokens
	modelUsage map[string]ModelUsage  // model -> cumulative usage
	toolUsage  map[string]int64       // tool -> call count
}

// scanSession parses one session jsonl into a self-contained fileAgg. The
// dedup rules (assistant message.id, usage) are per-file, matching the
// original aggSession semantics.
func scanSession(path, fileID string) fileAgg {
	fa := fileAgg{
		fileID: fileID, path: path, hour: -1,
		dayMsg:      map[string]int{},
		dayTool:     map[string]int{},
		dayTokens:   map[string]map[string]int64{},
		modelUsage:  map[string]ModelUsage{},
		toolUsage:   map[string]int64{},
	}
	seenMsg := map[string]bool{} // 已计数的 assistant 消息 id（去重内容块行）
	usageCounted := map[string]bool{}
	hourRecorded := false
	var prevMs int64 = -1
	var durationMs int64

	for _, l := range scanFile(path) {
		if l.IsSidechain || l.IsMeta {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, l.Timestamp)
		if err != nil {
			continue
		}
		date := ts.Format("2006-01-02")
		countedThis := false

		switch l.Type {
		case "user":
			t := strings.TrimSpace(textOf(l.Message.Content))
			if t == "" || strings.HasPrefix(t, "<") || strings.HasPrefix(t, "[Request interrupted") {
				continue
			}
			fa.counted++
			fa.dayMsg[date]++
			countedThis = true

		case "assistant":
			// 单条 API 回复被拆成多个内容块行，共享 message.id；按 id 去重
			key := l.Message.ID
			if key == "" {
				key = "line:" + l.UUID
			}
			if !seenMsg[key] {
				seenMsg[key] = true
				fa.counted++
				fa.dayMsg[date]++
				countedThis = true
			}
			// tool_use 块计数（每行只出现本行块，不用再按 id 去重）
			var blocks []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if json.Unmarshal(l.Message.Content, &blocks) == nil {
				for _, b := range blocks {
					if b.Type == "tool_use" {
						fa.dayTool[date]++
						if b.Name != "" {
							fa.toolUsage[b.Name]++
						}
					}
				}
			}
			// usage 只在同 id 首次出现的行累计（后续行重复同一份 usage）
			if len(l.Message.Usage) > 0 && !usageCounted[key] {
				usageCounted[key] = true
				var u usagelike
				if json.Unmarshal(l.Message.Usage, &u) == nil {
					model := l.Message.Model
					if model == "" {
						model = "unknown"
					}
					// CLI 内部合成消息（<synthetic>）不算真实用量
					if !strings.HasPrefix(model, "<") {
						m := fa.modelUsage[model]
						m.InputTokens += u.InputTokens
						m.OutputTokens += u.OutputTokens
						m.CacheReadTokens += u.CacheReadInputTokens
						m.CacheCreationTokens += u.cacheCreationTokens()
						m.Messages++
						fa.modelUsage[model] = m
						tm := fa.dayTokens[date]
						if tm == nil {
							tm = map[string]int64{}
							fa.dayTokens[date] = tm
						}
						tm[model] += u.total()
					}
				}
			}
		}

		if countedThis {
			if !hourRecorded {
				// 峰值时段按会话首条消息的小时（与 cc-haha 同口径）
				fa.hour = ts.Hour()
				hourRecorded = true
			}
			if prevMs >= 0 {
				if gap := ts.UnixMilli() - prevMs; gap > 0 && gap <= activeSessionGapMs {
					durationMs += gap
				}
			}
			prevMs = ts.UnixMilli()
		}
	}

	fa.durationMs = durationMs
	fa.active = fa.counted > 0
	return fa
}

// recompute merges per-session aggregates into the final Stats. Cheap: only
// arithmetic over sessions, no JSON parsing.
func recompute(fas []fileAgg) *Stats {
	stats := &Stats{
		ModelUsage: map[string]ModelUsage{},
		ToolUsage:  map[string]int64{},
	}
	dayActivity := map[string]*DailyActivity{}
	daySessions := map[string]map[string]bool{} // date -> set(sessionKey)
	dayTokens := map[string]map[string]int64{}  // date -> model -> tokens
	hourCounts := map[int]int{}

	for _, fa := range fas {
		if !fa.active {
			continue
		}
		stats.TotalSessions++
		stats.TotalMessages += fa.counted
		for date, n := range fa.dayMsg {
			dayActivityOf(date, dayActivity).MessageCount += n
			daySessionAdd(date, daySessions, fa.path)
		}
		for date, n := range fa.dayTool {
			dayActivityOf(date, dayActivity).ToolCallCount += n
		}
		for date, tm := range fa.dayTokens {
			dst := dayTokens[date]
			if dst == nil {
				dst = map[string]int64{}
				dayTokens[date] = dst
			}
			for model, t := range tm {
				dst[model] += t
			}
		}
		for model, m := range fa.modelUsage {
			cur := stats.ModelUsage[model]
			cur.InputTokens += m.InputTokens
			cur.OutputTokens += m.OutputTokens
			cur.CacheReadTokens += m.CacheReadTokens
			cur.CacheCreationTokens += m.CacheCreationTokens
			cur.Messages += m.Messages
			stats.ModelUsage[model] = cur
		}
		for tool, n := range fa.toolUsage {
			stats.ToolUsage[tool] += n
		}
		if fa.hour >= 0 {
			hourCounts[fa.hour]++
		}
		if stats.LongestSession == nil || fa.durationMs > stats.LongestSession.DurationMs ||
			(fa.durationMs == stats.LongestSession.DurationMs && fa.fileID < stats.LongestSession.SessionID) {
			stats.LongestSession = &SessionDuration{
				SessionID:    fa.fileID,
				DurationMs:   fa.durationMs,
				MessageCount: fa.counted,
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
	stats.Streaks = calculateStreaks(activeDates)

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

	// 常用模型：token 总量最大
	maxTokens := int64(0)
	for model, m := range stats.ModelUsage {
		total := m.InputTokens + m.OutputTokens + m.CacheReadTokens + m.CacheCreationTokens
		if total > maxTokens {
			maxTokens = total
			stats.FavoriteModel = model
		}
	}
	return stats
}

// fileFp fingerprints a session file by mtime+size to detect changes without
// re-parsing.
type fileFp struct {
	modTime time.Time
	size    int64
}

func (fp fileFp) same(o fileFp) bool {
	return fp.size == o.size && fp.modTime.Equal(o.modTime)
}

// StatsCache incrementally aggregates session stats: only re-parses files whose
// mtime/size changed since the last scan, then recomputes totals from the
// cached per-file aggregates. Not safe for concurrent use; the server guards
// it with a mutex.
type StatsCache struct {
	files map[string]fileAgg // path -> aggregate
	fps   map[string]fileFp  // path -> fingerprint
	stats *Stats             // last fully recomputed result
}

// NewStatsCache returns an empty incremental stats cache.
func NewStatsCache() *StatsCache {
	return &StatsCache{files: map[string]fileAgg{}, fps: map[string]fileFp{}}
}

// Refresh returns current stats. fresh=true discards the per-file cache and
// rescans everything; otherwise only changed/new files are re-parsed, gone
// files are dropped, and totals are recomputed — or returned as-is when nothing
// changed since the last refresh.
func (c *StatsCache) Refresh(root string, fresh bool) *Stats {
	if fresh {
		c.files = map[string]fileAgg{}
		c.fps = map[string]fileFp{}
		c.stats = nil
	}

	projects, err := os.ReadDir(root)
	if err != nil {
		// root 不可读（尚无会话目录等）：返回空统计，不污染缓存。
		return recompute(nil)
	}
	changed := false
	seen := map[string]bool{}
	for _, proj := range projects {
		if !proj.IsDir() {
			continue
		}
		dir := filepath.Join(root, proj.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(dir, f.Name())
			seen[path] = true
			fi, err := f.Info()
			if err != nil {
				continue
			}
			fp := fileFp{modTime: fi.ModTime(), size: fi.Size()}
			if old, ok := c.fps[path]; ok && old.same(fp) {
				continue // 指纹未变：复用缓存的逐文件聚合
			}
			sid := strings.TrimSuffix(f.Name(), ".jsonl")
			c.files[path] = scanSession(path, sid)
			c.fps[path] = fp
			changed = true
		}
	}
	// 丢弃已不存在的文件
	for path := range c.fps {
		if !seen[path] {
			delete(c.files, path)
			delete(c.fps, path)
			changed = true
		}
	}
	if !changed && c.stats != nil {
		return c.stats
	}
	fas := make([]fileAgg, 0, len(c.files))
	for _, fa := range c.files {
		fas = append(fas, fa)
	}
	c.stats = recompute(fas)
	return c.stats
}

// AggregateStats scans root (usually ~/.claude/projects, each subdir a cwd slug)
// and aggregates activity/tokens across all session files. Top-level session
// files only (subagents/ not recursed), same source as List/import. Unreadable
// files are skipped silently; an unreadable root yields empty stats.
func AggregateStats(root string) *Stats {
	c := NewStatsCache()
	return c.Refresh(root, true)
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

// calculateStreaks 计算当前/最长连续活跃天数。当前连续从今天起往前数
// （今天无活动则从昨天起数），中断后归零。
func calculateStreaks(dates []string) StreakInfo {
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
		cursor = addDateKey(cursor, -1)
	}
	return StreakInfo{Current: current, Longest: longest}
}

// addDateKey 对 YYYY-MM-DD 日期做加减天。
func addDateKey(key string, delta int) string {
	t, err := time.Parse("2006-01-02", key)
	if err != nil {
		return key
	}
	return t.AddDate(0, 0, delta).Format("2006-01-02")
}
