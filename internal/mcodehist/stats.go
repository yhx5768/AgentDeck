// Usage statistics for mcode sessions, aggregated from the same on-disk
// transcript the replay path reads (messages.jsonl).
//
// mcode is cheaper to aggregate than opencode: the transcript is a local file
// rather than a CLI export, so a full rescan is milliseconds rather than ~1s
// per session. The incremental mtime+size fingerprint (the same approach
// claudehist uses) still applies, so a steady-state refresh only re-reads the
// sessions that moved.
//
// Counting rules are kept identical to the claude and opencode sides so all
// three feed statsagg.Recompute through one set of semantics: a user prompt
// and an assistant step each count one message, tool calls are deduped by
// call id (a running→completed pair is one call), and each step's usage is
// counted once.
package mcodehist

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"cc-webui/internal/config"
	"cc-webui/internal/statsagg"
)

// engineKindMcode is the engine key mcodehist records in Stats.Engines,
// aligned with the engine kind axis (config.Kind*).
const engineKindMcode = config.KindMcode

// StatsCache incrementally aggregates mcode session stats. Not safe for
// concurrent use; the server guards it with a mutex.
type StatsCache struct {
	listFn  func() ([]SessionInfo, error)
	fetchFn func(sessionID string) (*Session, error)

	aggs      map[string]statsagg.FileAgg // sessionID -> aggregate
	updatedMs map[string]int64            // sessionID -> newest updated ms
	sig       map[string]string           // sessionID -> "size:mtimeNs" fingerprint
}

// NewStatsCache returns an empty incremental stats cache.
func NewStatsCache() *StatsCache {
	return &StatsCache{
		listFn:    List,
		fetchFn:   FetchSession,
		aggs:      map[string]statsagg.FileAgg{},
		updatedMs: map[string]int64{},
		sig:       map[string]string{},
	}
}

// Refresh returns one aggregate per mcode session, newest first enumeration
// reused for the change detection. fresh=true re-reads everything; otherwise
// only sessions whose transcript fingerprint moved are re-parsed, and
// sessions that vanished are dropped.
//
// A refresh that enumerated nothing at all (the store is missing or
// unreadable) keeps the previous cache rather than reporting zero usage —
// a transient environment problem should not look like "you used nothing".
func (c *StatsCache) Refresh(fresh bool) []statsagg.FileAgg {
	list, err := c.listFn()
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
				if fp, ok := c.sig[it.SessionID]; ok && fp == transcriptFingerprint(it.SessionID) {
					continue
				}
			}
		}
		s, err := c.fetchFn(it.SessionID)
		if err != nil {
			// A session that fails to parse keeps its old aggregate; it is
			// not treated as deleted and is retried on the next refresh.
			continue
		}
		c.aggs[it.SessionID] = ScanSession(s)
		c.updatedMs[it.SessionID] = maxMs(s.UpdatedAt, it.UpdatedAt).UnixMilli()
		c.sig[it.SessionID] = transcriptFingerprint(it.SessionID)
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

func maxMs(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// transcriptFingerprint is the transcript's size + mtime, the cheap change
// detector for the incremental scan.
func transcriptFingerprint(sessionID string) string {
	dir, ok := FindSessionDir(sessionID)
	if !ok {
		return "missing"
	}
	st, err := os.Stat(filepath.Join(dir, "messages.jsonl"))
	if err != nil {
		return "missing"
	}
	return itoa(st.Size()) + ":" + itoa(st.ModTime().UnixNano())
}

// ScanSession aggregates one parsed session into a self-contained FileAgg,
// mirroring claudehist/opencodehist counting rules so all three engines feed
// statsagg.Recompute through one shape.
func ScanSession(s *Session) statsagg.FileAgg {
	fa := statsagg.NewFileAgg(s.ID, "mcode:"+s.ID, engineKindMcode)
	seenTool := map[string]bool{} // tool call id dedup
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
			// Strip the injected <system-reminder> block; a message that is
			// nothing but injection is not a user prompt (same rule as the
			// replay path, so counts and bubbles agree).
			t := firstUserText(m)
			if t == "" {
				continue
			}
			fa.Counted++
			fa.DayMsg[date]++
			counted = true
		case "assistant":
			fa.Counted++
			fa.DayMsg[date]++
			counted = true
			for _, b := range m.Blocks {
				if t, _ := b["type"].(string); t != "toolCall" {
					continue
				}
				callID, _ := b["id"].(string)
				if callID == "" || seenTool[callID] {
					continue
				}
				seenTool[callID] = true
				fa.DayTool[date]++
				if name, _ := b["name"].(string); name != "" {
					fa.ToolUsage[name]++
				}
			}
			if in, out := mcpUsage(m.Usage); in+out > 0 {
				model := mcodeModelKey(m.Provider, m.Model)
				mu := fa.ModelUsage[model]
				mu.InputTokens += in
				mu.OutputTokens += out
				mu.ReasoningTokens += 0
				mu.CacheReadTokens += mcpNum(m.Usage, "cacheRead")
				mu.CacheCreationTokens += mcpNum(m.Usage, "cacheWrite")
				mu.Messages++
				fa.ModelUsage[model] = mu
				tm := fa.DayTokens[date]
				if tm == nil {
					tm = map[string]int64{}
					fa.DayTokens[date] = tm
				}
				tm[model] += in + out + mcpNum(m.Usage, "cacheRead") + mcpNum(m.Usage, "cacheWrite")
			}
		}

		if counted {
			if !hourRecorded {
				// Peak hour uses the session's first message, same as the
				// claude side.
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

// mcpUsage reads mcode's per-step token counts. The transcript uses
// {"input":…,"output":…,"cacheRead":…,"cacheWrite":…,"totalTokens":…,
//
//	"cost":{…}} — different key names from both claude and opencode, so it is
//
// normalized here rather than in statsagg (which stays engine-agnostic).
func mcpUsage(u map[string]any) (input, output int64) {
	return mcpNum(u, "input"), mcpNum(u, "output")
}

func mcpNum(u map[string]any, key string) int64 {
	if u == nil {
		return 0
	}
	return numOf(u[key])
}

// mcodeModelKey builds the "<provider>/<model>" key shown in the stats UI.
func mcodeModelKey(provider, model string) string {
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
