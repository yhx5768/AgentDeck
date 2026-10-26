// Usage statistics for OpenCode 2 sessions. Same pipeline and counting rules as
// the v1 opencodehist cache (so the merged stats page is consistent), but the
// transcript shape and the enumeration surface are v2's:
//
//   - enumeration: `api session.list --standalone` returns every project in one
//     call (v1 needed a raw sqlite SELECT because `session list` was
//     bucket-scoped);
//   - per-session read: `session export <id> --standalone`;
//   - v2 assistant messages carry `tokens` plus an ordered `content` array, so
//     tool calls are counted from parts of type "tool" (deduped by call id).
package opencodev2

import (
	"log"
	"strings"
	"sync"

	"cc-webui/internal/config"
	"cc-webui/internal/statsagg"
)

const engineKindOpenCode = config.KindOpenCode

// StatsCache incrementally aggregates v2 sessions. Enumeration is cheap (one
// CLI call); export is ~1s per session, so only sessions whose updated time
// moved are re-read.
type StatsCache struct {
	listAllFn func(cliPath string) ([]SessionInfo, error)
	listFn    func(cliPath, dir string) ([]SessionInfo, error)
	fetchFn   func(cliPath, sessionID string) (*Session, error)

	aggs      map[string]statsagg.FileAgg
	updatedMs map[string]int64
	sig       string
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

const exportWorkers = 8

// Refresh returns one aggregate per v2 session, across every project.
func (c *StatsCache) Refresh(cliPaths []string, bucketDirs []string, fresh bool) []statsagg.FileAgg {
	sig := strings.Join(cliPaths, "\x00")
	if fresh || sig != c.sig {
		c.aggs = map[string]statsagg.FileAgg{}
		c.updatedMs = map[string]int64{}
		c.sig = sig
	}
	if len(cliPaths) == 0 {
		return nil
	}

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

	listOK := false
	if all, err := c.listAllFn(cliPaths[0]); err != nil {
		log.Printf("stats: opencode v2 enumeration: %v; falling back to per-bucket session list", err)
	} else {
		listOK = true
		addRows(all, cliPaths[0])
	}
	if !listOK {
		fail := 0
		for _, cli := range cliPaths {
			for _, dir := range bucketDirs {
				items, err := c.listFn(cli, dir)
				if err != nil {
					log.Printf("stats: opencode v2 session list %s: %v", cli, err)
					fail++
					continue
				}
				addRows(items, cli)
			}
		}
		if len(rows) == 0 && fail > 0 && len(c.aggs) > 0 {
			// Every enumeration failed: keep the previous cache rather than
			// reporting zero because of a transient CLI problem.
			return c.all()
		}
	}

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
					log.Printf("stats: opencode v2 export %s: %v", r.info.SessionID, err)
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

	for id := range c.aggs {
		if !seenID[id] {
			delete(c.aggs, id)
			delete(c.updatedMs, id)
		}
	}
	return c.all()
}

func (c *StatsCache) all() []statsagg.FileAgg {
	fas := make([]statsagg.FileAgg, 0, len(c.aggs))
	for _, fa := range c.aggs {
		fas = append(fas, fa)
	}
	return statsagg.Sorted(fas)
}

// ScanSession aggregates one exported v2 session with the same counting rules
// the other engines use: a user prompt and an assistant step each count as a
// message, tool calls are deduped by call id, and per-step usage counts once.
func ScanSession(s *Session) statsagg.FileAgg {
	fa := statsagg.NewFileAgg(s.ID, "opencode:"+s.ID, engineKindOpenCode)
	seenTool := map[string]bool{}
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
			t := strings.TrimSpace(m.Text)
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
			for _, p := range m.Content {
				if p.Type != "tool" || p.CallID == "" || seenTool[p.CallID] {
					continue
				}
				seenTool[p.CallID] = true
				fa.DayTool[date]++
				name := p.ToolName
				if name == "" {
					name = "tool"
				}
				fa.ToolUsage[name]++
			}
			if tot := m.Tokens.Total(); tot > 0 {
				prov, mid := m.ModelProviderID, m.ModelID
				if mid == "" {
					prov, mid = s.ModelProviderID, s.ModelID
				}
				model := ocModelKey(prov, mid)
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

func ocModelKey(providerID, modelID string) string {
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
