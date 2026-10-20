// Usage statistics for codex sessions, aggregated from the same rollout files
// the replay path reads. Like mcode, the transcript is local, so a steady-state
// refresh only re-parses the files whose size+mtime fingerprint moved.
//
// Counting rules stay identical to the claude/opencode/mcode sides so all
// engines feed statsagg.Recompute through one set of semantics: a user prompt
// and an assistant step each count one message, tool calls are deduped by call
// id, and each response's usage is counted once.
package codexhist

import (
	"os"
	"strings"

	"cc-webui/internal/config"
	"cc-webui/internal/statsagg"
)

const engineKindCodex = config.KindCodex

// StatsCache incrementally aggregates codex sessions. Not safe for concurrent
// use; the server guards it with a mutex.
type StatsCache struct {
	home string

	listFn  func(home string) ([]SessionInfo, error)
	fetchFn func(home, sessionID string) (*Session, error)

	aggs      map[string]statsagg.FileAgg
	updatedMs map[string]int64
	sig       map[string]string
}

// NewStatsCache returns an empty cache for the given codex home.
func NewStatsCache(home string) *StatsCache {
	return &StatsCache{
		home:      home,
		listFn:    List,
		fetchFn:   FetchSession,
		aggs:      map[string]statsagg.FileAgg{},
		updatedMs: map[string]int64{},
		sig:       map[string]string{},
	}
}

// Refresh returns one aggregate per codex session. fresh=true re-reads
// everything. A refresh that enumerated nothing at all keeps the previous
// cache rather than reporting zero usage.
func (c *StatsCache) Refresh(fresh bool) []statsagg.FileAgg {
	list, err := c.listFn(c.home)
	if err != nil || len(list) == 0 {
		if len(c.aggs) > 0 {
			return c.all()
		}
		return nil
	}
	seen := map[string]bool{}
	for _, it := range list {
		seen[it.SessionID] = true
		if !fresh {
			if old, ok := c.updatedMs[it.SessionID]; ok && old >= it.UpdatedAt.UnixMilli() {
				if fp, ok := c.sig[it.SessionID]; ok && fp == rolloutFingerprint(c.home, it.SessionID) {
					continue
				}
			}
		}
		s, err := c.fetchFn(c.home, it.SessionID)
		if err != nil {
			continue
		}
		c.aggs[it.SessionID] = ScanSession(s)
		if s.Updated.After(it.UpdatedAt) {
			c.updatedMs[it.SessionID] = s.Updated.UnixMilli()
		} else {
			c.updatedMs[it.SessionID] = it.UpdatedAt.UnixMilli()
		}
		c.sig[it.SessionID] = rolloutFingerprint(c.home, it.SessionID)
	}
	for id := range c.aggs {
		if !seen[id] {
			delete(c.aggs, id)
			delete(c.updatedMs, id)
			delete(c.sig, id)
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

func rolloutFingerprint(home, sessionID string) string {
	path, ok := FindSessionPath(home, sessionID)
	if !ok {
		return "missing"
	}
	st, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return itoa(st.Size()) + ":" + itoa(st.ModTime().UnixNano())
}

// ScanSession aggregates one parsed session into a self-contained FileAgg.
func ScanSession(s *Session) statsagg.FileAgg {
	fa := statsagg.NewFileAgg(s.ID, "codex:"+s.ID, engineKindCodex)
	seenTool := map[string]bool{}
	hourRecorded := false
	var prevMs int64 = -1

	for _, m := range s.Messages {
		if m.TS.IsZero() {
			continue
		}
		date := m.TS.Format("2006-01-02")
		counted := false

		switch m.Kind {
		case "user":
			if strings.TrimSpace(m.Text) == "" {
				continue
			}
			fa.Counted++
			fa.DayMsg[date]++
			counted = true
		case "assistant":
			if strings.TrimSpace(m.Text) == "" {
				continue
			}
			fa.Counted++
			fa.DayMsg[date]++
			counted = true
		case "toolUse":
			if m.ToolID == "" || seenTool[m.ToolID] {
				continue
			}
			seenTool[m.ToolID] = true
			fa.DayTool[date]++
			if m.ToolName != "" {
				fa.ToolUsage[m.ToolName]++
			}
		case "usage":
			if len(m.Usage) == 0 {
				continue
			}
			model := codexModelKey(s.Provider, firstNonEmpty(m.Model, s.ModelID))
			in := numOf(m.Usage["input_tokens"])
			out := numOf(m.Usage["output_tokens"])
			reason := numOf(m.Usage["reasoning_output_tokens"])
			cr := numOf(m.Usage["cached_input_tokens"])
			cw := numOf(m.Usage["cache_write_input_tokens"])
			if in+out+reason+cr+cw == 0 {
				continue
			}
			mu := fa.ModelUsage[model]
			mu.InputTokens += in
			mu.OutputTokens += out
			mu.ReasoningTokens += reason
			mu.CacheReadTokens += cr
			mu.CacheCreationTokens += cw
			fa.ModelUsage[model] = mu
			tm := fa.DayTokens[date]
			if tm == nil {
				tm = map[string]int64{}
				fa.DayTokens[date] = tm
			}
			tm[model] += in + out + reason + cr + cw
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

func codexModelKey(provider, model string) string {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	switch {
	case provider != "" && model != "":
		return provider + "/" + model
	case model != "":
		return model
	case provider != "":
		return provider
	default:
		return "unknown"
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
