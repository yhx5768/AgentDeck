package claudehist

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cc-webui/internal/config"
	"cc-webui/internal/statsagg"
)

// engineKindClaude is the engine key claudehist records in Stats.Engines,
// aligned with the engine kind axis (config.Kind*).
const engineKindClaude = config.KindClaude

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

// scanSession parses one session jsonl into a self-contained FileAgg. The
// dedup rules (assistant message.id, usage) are per-file, matching the
// original aggSession semantics.
//
// engine is the engine kind recorded in Stats.Engines: the same transcript
// format is read by more than one CLI (see Profile), and each reports under
// its own kind.
//
// modelHint, when non-empty, replaces the model name recorded in the
// transcript. Qoder CLI CN writes an internal key there ("qfmodel") rather
// than the model name, so a per-model breakdown would otherwise collapse
// every qoder session into one meaningless bucket; the caller supplies the
// model each session was actually launched with. Claude's transcripts carry
// the real name (and may change it mid-session), so its cache passes "".
func scanSession(path, fileID, engine, modelHint string) statsagg.FileAgg {
	fa := statsagg.NewFileAgg(fileID, path, engine)
	seenMsg := map[string]bool{} // 已计数的 assistant 消息 id（去重内容块行）
	usageCounted := map[string]bool{}
	hourRecorded := false
	var prevMs int64 = -1

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
			fa.Counted++
			fa.DayMsg[date]++
			countedThis = true

		case "assistant":
			// 单条 API 回复被拆成多个内容块行，共享 message.id；按 id 去重
			key := l.Message.ID
			if key == "" {
				key = "line:" + l.UUID
			}
			if !seenMsg[key] {
				seenMsg[key] = true
				fa.Counted++
				fa.DayMsg[date]++
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
						fa.DayTool[date]++
						if b.Name != "" {
							fa.ToolUsage[b.Name]++
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
					if modelHint != "" {
						model = modelHint
					}
					if model == "" {
						model = "unknown"
					}
					// CLI 内部合成消息（<synthetic>）不算真实用量
					if !strings.HasPrefix(model, "<") {
						m := fa.ModelUsage[model]
						m.InputTokens += u.InputTokens
						m.OutputTokens += u.OutputTokens
						m.CacheReadTokens += u.CacheReadInputTokens
						m.CacheCreationTokens += u.cacheCreationTokens()
						m.Messages++
						fa.ModelUsage[model] = m
						tm := fa.DayTokens[date]
						if tm == nil {
							tm = map[string]int64{}
							fa.DayTokens[date] = tm
						}
						tm[model] += u.total()
					}
				}
			}
		}

		if countedThis {
			if !hourRecorded {
				// 峰值时段按会话首条消息的小时（与 cc-haha 同口径）
				fa.Hour = ts.Hour()
				hourRecorded = true
			}
			if prevMs >= 0 {
				if gap := ts.UnixMilli() - prevMs; gap > 0 && gap <= statsagg.ActiveSessionGapMs {
					fa.DurationMs += gap
				}
			}
			prevMs = ts.UnixMilli()
		}
	}

	fa.Active = fa.Counted > 0
	return fa
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

// StatsCache incrementally aggregates Claude session stats: only re-parses
// files whose mtime/size changed since the last scan; totals are recomputed
// from the cached per-file aggregates by the caller. Not safe for concurrent
// use; the server guards it with a mutex.
type StatsCache struct {
	files  map[string]statsagg.FileAgg // path -> aggregate
	fps    map[string]fileFp           // path -> fingerprint
	engine string                      // engine kind recorded in every aggregate
}

// NewStatsCache returns an empty incremental stats cache keyed to the claude
// engine kind.
func NewStatsCache() *StatsCache {
	return NewStatsCacheFor(engineKindClaude)
}

// NewStatsCacheFor returns an empty incremental stats cache that records its
// aggregates under the given engine kind — one cache per CLI sharing this
// transcript format (Claude Code, Qoder CLI CN).
func NewStatsCacheFor(engine string) *StatsCache {
	return &StatsCache{
		files:  map[string]statsagg.FileAgg{},
		fps:    map[string]fileFp{},
		engine: engine,
	}
}

// Refresh returns the cached per-session aggregates. fresh=true discards the
// per-file cache and rescans everything; otherwise only changed/new files are
// re-parsed and gone files are dropped.
func (c *StatsCache) Refresh(root string, fresh bool) []statsagg.FileAgg {
	return c.RefreshWithModels(root, nil, fresh)
}

// RefreshWithModels is Refresh plus a session-id -> model-name map that
// overrides the model recorded in the transcript (see scanSession). Used by
// the engines whose transcripts do not name the model.
func (c *StatsCache) RefreshWithModels(root string, models map[string]string, fresh bool) []statsagg.FileAgg {
	if fresh {
		c.files = map[string]statsagg.FileAgg{}
		c.fps = map[string]fileFp{}
	}

	projects, err := os.ReadDir(root)
	if err != nil {
		// root 不可读（尚无会话目录等）：返回空统计，不污染缓存。
		return nil
	}
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
			sid := strings.TrimSuffix(f.Name(), ".jsonl")
			if old, ok := c.fps[path]; ok && old.same(fp) {
				continue // 指纹未变：复用缓存的逐文件聚合
			}
			c.files[path] = scanSession(path, sid, c.engine, models[sid])
			c.fps[path] = fp
		}
	}
	// 丢弃已不存在的文件
	for path := range c.fps {
		if !seen[path] {
			delete(c.files, path)
			delete(c.fps, path)
		}
	}
	fas := make([]statsagg.FileAgg, 0, len(c.files))
	for _, fa := range c.files {
		fas = append(fas, fa)
	}
	return fas
}

// AggregateStats scans root (usually ~/.claude/projects, each subdir a cwd
// slug) and aggregates activity/tokens across all session files. Top-level
// session files only (subagents/ not recursed), same source as List/import.
// Unreadable files are skipped silently; an unreadable root yields empty stats.
func AggregateStats(root string) *statsagg.Stats {
	return statsagg.Recompute(NewStatsCache().Refresh(root, true))
}
