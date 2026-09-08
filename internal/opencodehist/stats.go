// Usage statistics for opencode sessions, aggregated from the same official
// surfaces the rest of the package uses (`session list`, `export`) plus one
// read-only `opencode db` SELECT for enumeration, which the bucket-scoped
// `session list` cannot do.
//
// `session list` only returns the project bucket the process cwd belongs to and
// opencode exposes no way to list all buckets, so enumeration goes through
// `ListAll` (a fixed SELECT over the session table, newest first, subagent
// sessions excluded to match `session list`). The caller still supplies the
// bucket directories: they are the fallback when the db surface fails, because
// the store schema is opencode-internal.
package opencodehist

import (
	"log"
	"strings"
	"sync"

	"cc-webui/internal/config"
	"cc-webui/internal/statsagg"
)

// engineKindOpenCode is the engine key opencodehist records in Stats.Engines,
// aligned with the engine kind axis (config.Kind*).
const engineKindOpenCode = config.KindOpenCode

// StatsCache incrementally aggregates opencode session stats. `session list`
// is re-run for every bucket on each refresh (about a second per bucket), but
// `export` is expensive (~1s per session, multi-MB for long ones), so only
// sessions whose `updated` timestamp moved are re-exported. Not safe for
// concurrent use; the server guards it with a mutex.
type StatsCache struct {
	listAllFn func(cliPath string) ([]SessionInfo, error)
	listFn    func(cliPath, dir string) ([]SessionInfo, error)
	fetchFn   func(cliPath, sessionID string) (*Session, error)

	aggs      map[string]statsagg.FileAgg // sessionID -> aggregate
	updatedMs map[string]int64            // sessionID -> newest `updated` ms (grows only)
	sig       string                      // configured cli set this cache was built for
}

// NewStatsCache returns an empty incremental stats cache.
func NewStatsCache() *StatsCache {
	return &StatsCache{
		listAllFn: ListAll,
		listFn:    List,
		fetchFn:   FetchSession,
		aggs:      map[string]statsagg.FileAgg{},
		updatedMs: map[string]int64{},
	}
}

// exportWorkers bounds concurrent `opencode export` calls during a cold scan.
const exportWorkers = 8

// Refresh returns one aggregate per opencode session, across every project.
// Session ids are globally unique, so bucket-level dedup is only needed on the
// per-bucket fallback path. fresh=true re-exports everything; otherwise only
// sessions whose `updated` moved are re-exported and sessions that vanished are
// dropped. bucketDirs is only consulted when the `opencode db` enumeration
// fails.
func (c *StatsCache) Refresh(cliPaths []string, bucketDirs []string, fresh bool) []statsagg.FileAgg {
	sig := strings.Join(cliPaths, "\x00")
	if fresh || sig != c.sig {
		// 配置的 opencode cli 变了，缓存作废重建。
		c.aggs = map[string]statsagg.FileAgg{}
		c.updatedMs = map[string]int64{}
		c.sig = sig
	}
	if len(cliPaths) == 0 {
		return nil
	}

	// 1) enumerate. Prefer `opencode db` — one read-only SELECT returns every
	// project at once, which `session list` cannot (it is bucket scoped). Fall
	// back to asking every candidate bucket separately when the db surface is
	// unavailable: the table/column names are opencode-internal and may change.
	type listed struct {
		cli  string
		info SessionInfo
	}
	var rows []listed
	seenID := map[string]bool{}
	addRows := func(items []SessionInfo, cli string) {
		for _, it := range items {
			if seenID[it.SessionID] {
				continue
			}
			seenID[it.SessionID] = true
			rows = append(rows, listed{cli: cli, info: it})
		}
	}
	listFail := 0
	dbOK := false
	if all, err := c.listAllFn(cliPaths[0]); err != nil {
		log.Printf("stats: opencode db enumeration: %v; falling back to per-bucket session list", err)
	} else {
		dbOK = true
		addRows(all, cliPaths[0])
	}
	if !dbOK {
		// ids are globally unique, so dedup across buckets
		for _, cli := range cliPaths {
			for _, dir := range bucketDirs {
				items, err := c.listFn(cli, dir)
				if err != nil {
					log.Printf("stats: opencode session list %s: %v", cli, err)
					listFail++
					continue
				}
				addRows(items, cli)
			}
		}
	}
	if len(rows) == 0 && !dbOK && listFail == len(cliPaths)*len(bucketDirs) && len(c.aggs) > 0 {
		// 没有任何一次枚举成功（db 与全部桶列表都失败，CLI 不可用/异常）：
		// 保守保留上次缓存，避免一次环境问题把 opencode 统计整体清零。
		return c.all()
	}

	// 2) export the sessions that changed since the last refresh.
	var changed []listed
	for _, r := range rows {
		cur := r.info.UpdatedAt.UnixMilli()
		if old, ok := c.updatedMs[r.info.SessionID]; ok && old >= cur {
			continue
		}
		changed = append(changed, r)
	}
	var mu sync.Mutex
	work := make(chan listed, len(changed))
	var wg sync.WaitGroup
	for i := 0; i < exportWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range work {
				s, err := c.fetchFn(r.cli, r.info.SessionID)
				if err != nil {
					// 失败的会话保留旧缓存（下次刷新重试），不当作已删除
					log.Printf("stats: opencode export %s: %v", r.info.SessionID, err)
					continue
				}
				fa := ScanSession(s)
				ms := s.UpdatedAt.UnixMilli()
				if ms < r.info.UpdatedAt.UnixMilli() {
					ms = r.info.UpdatedAt.UnixMilli()
				}
				mu.Lock()
				c.aggs[s.ID] = fa
				c.updatedMs[s.ID] = ms
				mu.Unlock()
			}
		}()
	}
	for _, r := range changed {
		work <- r
	}
	close(work)
	wg.Wait()

	// 3) 丢弃已从所有桶消失的会话
	for id := range c.aggs {
		if !seenID[id] {
			delete(c.aggs, id)
			delete(c.updatedMs, id)
		}
	}
	return c.all()
}

// all returns the cached aggregates ordered by session id (deterministic ties).
func (c *StatsCache) all() []statsagg.FileAgg {
	fas := make([]statsagg.FileAgg, 0, len(c.aggs))
	for _, fa := range c.aggs {
		fas = append(fas, fa)
	}
	return statsagg.Sorted(fas)
}

// ScanSession aggregates one exported session into a self-contained FileAgg,
// mirroring claudehist.scanSession's counting rules so both engines feed
// statsagg.Recompute through one shape: user prompts and assistant steps count
// as messages, tool parts count as tool calls, and per-step usage counts once.
func ScanSession(s *Session) statsagg.FileAgg {
	fa := statsagg.NewFileAgg(s.ID, "opencode:"+s.ID, engineKindOpenCode)
	seenTool := map[string]bool{} // callID 去重（running→completed 双发）
	hourRecorded := false
	var prevMs int64 = -1

	for _, m := range s.Messages {
		if m.TS.IsZero() {
			continue
		}
		date := m.TS.Format("2006-01-02")
		counted := false

		switch m.Role {
		case "user":
			// 只取真正的 prompt echo（过滤 -f 附件产生的工具前导/文件展开文本），
			// 过滤规则与回放路径一致；< 前缀为注入内容，中断提示不算用户消息。
			t := ""
			for _, p := range m.Parts {
				if pt, ok := userPromptText(p); ok {
					t = strings.TrimSpace(stripEchoQuotes(pt))
					break
				}
			}
			if t == "" || strings.HasPrefix(t, "<") || strings.HasPrefix(t, "[Request interrupted") {
				continue
			}
			fa.Counted++
			fa.DayMsg[date]++
			counted = true
		case "assistant":
			fa.Counted++
			fa.DayMsg[date]++
			counted = true
			for _, p := range m.Parts {
				if partType(p) != "tool" {
					continue
				}
				callID, _ := p["callID"].(string)
				if callID == "" || seenTool[callID] {
					continue
				}
				seenTool[callID] = true
				fa.DayTool[date]++
				if name, _ := p["tool"].(string); name != "" {
					fa.ToolUsage[name]++
				}
			}
			if tot := m.Tokens.Total(); tot > 0 {
				// export 的 assistant 步不带 model 字段，回退到会话级模型
				prov, mid := m.ModelProviderID, m.ModelID
				if mid == "" {
					prov, mid = s.ModelProviderID, s.ModelID
				}
				model := modelKey(prov, mid)
				mu := fa.ModelUsage[model]
				mu.InputTokens += m.Tokens.Input
				mu.OutputTokens += m.Tokens.Output
				mu.ReasoningTokens += m.Tokens.Reasoning
				mu.CacheReadTokens += m.Tokens.CacheRead
				mu.CacheCreationTokens += m.Tokens.CacheWrite
				mu.Messages++
				fa.ModelUsage[model] = mu
				tm := fa.DayTokens[date]
				if tm == nil {
					tm = map[string]int64{}
					fa.DayTokens[date] = tm
				}
				tm[model] += tot
			}
		}

		if counted {
			if !hourRecorded {
				// 峰值时段按会话首条消息的小时（与 claude 侧同口径）
				fa.Hour = m.TS.Hour()
				hourRecorded = true
			}
			if prevMs >= 0 {
				if gap := m.TS.UnixMilli() - prevMs; gap > 0 && gap <= statsagg.ActiveSessionGapMs {
					fa.DurationMs += gap
				}
			}
			prevMs = m.TS.UnixMilli()
		}
	}

	fa.Active = fa.Counted > 0
	return fa
}

// modelKey builds the "<provider>/<model>" key shown in the stats UI, matching
// the model_override naming used for `opencode run -m`. A step whose export
// carries only one half of the pair falls back to it rather than "unknown".
func modelKey(providerID, modelID string) string {
	if providerID != "" && modelID != "" {
		return providerID + "/" + modelID
	}
	if modelID != "" {
		return modelID
	}
	if providerID != "" {
		return providerID
	}
	return "unknown"
}
