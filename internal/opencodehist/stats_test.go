package opencodehist

import (
	"errors"
	"testing"
	"time"

	"cc-webui/internal/statsagg"
)

// at parses a "2006-01-02 15:04:05" local timestamp, for fixture readability.
func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func mkPart(t string, extra map[string]any) map[string]any {
	p := map[string]any{"type": t}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func toolPart(callID, name string) map[string]any {
	return mkPart("tool", map[string]any{"callID": callID, "tool": name})
}

func textPart(t string) map[string]any {
	return mkPart("text", map[string]any{"text": t})
}

func userMsg(id string, ts time.Time, parts ...map[string]any) SessionMessage {
	return SessionMessage{Role: "user", ID: id, TS: ts, Parts: parts}
}

func assistantMsg(id string, ts time.Time, prov, model string, tk MessageTokens, parts ...map[string]any) SessionMessage {
	return SessionMessage{
		Role: "assistant", ID: id, TS: ts, Parts: parts,
		ModelProviderID: prov, ModelID: model, Tokens: tk,
	}
}

func TestScanSessionCountingRules(t *testing.T) {
	s := &Session{
		ID: "ses_1",
		Messages: []SessionMessage{
			userMsg("m1", at("2026-09-01 09:00:00"), textPart(`"hello world"`)),
			assistantMsg("m2", at("2026-09-01 09:01:00"), "anthropic", "claude-sonnet-5",
				MessageTokens{Input: 100, Output: 50, Reasoning: 10, CacheRead: 400, CacheWrite: 20},
				toolPart("c1", "read"), textPart("answer")),
			// running→completed 双发：同一 callID 只算一次工具调用
			assistantMsg("m3", at("2026-09-01 09:02:00"), "anthropic", "claude-sonnet-5",
				MessageTokens{Input: 1, Output: 1}, toolPart("c1", "read")),
			// 零 token 的 assistant 步仍计消息，但不进 ModelUsage
			assistantMsg("m4", at("2026-09-01 09:03:00"), "anthropic", "claude-sonnet-5", MessageTokens{}),
			// 注入内容与中断提示不算用户消息
			userMsg("m5", at("2026-09-01 09:04:00"), textPart("<system-reminder>x</system-reminder>")),
			userMsg("m6", at("2026-09-01 09:05:00"), textPart("[Request interrupted by user]")),
			// 附件产生的工具前导/文件展开文本不是 prompt
			userMsg("m7", at("2026-09-01 09:06:00"), textPart("Called the Read tool with the following input: {}")),
			// 跨天：次日一条
			userMsg("m8", at("2026-09-02 10:00:00"), textPart("next day")),
		},
	}

	fa := ScanSession(s)
	if !fa.Active {
		t.Fatal("expected active session")
	}
	if fa.Engine != "opencode" || fa.FileID != "ses_1" || fa.Path != "opencode:ses_1" {
		t.Fatalf("identity: %+v", fa)
	}
	// user(m1) + assistant(m2,m3,m4) + user(m8) = 5
	if fa.Counted != 5 {
		t.Errorf("Counted = %d, want 5", fa.Counted)
	}
	if fa.DayMsg["2026-09-01"] != 4 || fa.DayMsg["2026-09-02"] != 1 {
		t.Errorf("DayMsg = %v", fa.DayMsg)
	}
	if fa.DayTool["2026-09-01"] != 1 {
		t.Errorf("DayTool = %v, want one call deduped", fa.DayTool)
	}
	if fa.ToolUsage["read"] != 1 {
		t.Errorf("ToolUsage = %v", fa.ToolUsage)
	}
	if fa.Hour != 9 {
		t.Errorf("Hour = %d, want 9", fa.Hour)
	}
	// 9:00→9:03 累计 3 分钟（9:04/9:05/9:06 未计入），9:03→次日 10:00 间隔超 30 分钟不计
	if fa.DurationMs != 3*60*1000 {
		t.Errorf("DurationMs = %d, want %d", fa.DurationMs, 3*60*1000)
	}

	model := fa.ModelUsage["anthropic/claude-sonnet-5"]
	if len(fa.ModelUsage) != 1 {
		t.Fatalf("ModelUsage keys = %v, want the provider/model key only", fa.ModelUsage)
	}
	if model.InputTokens != 101 || model.OutputTokens != 51 || model.ReasoningTokens != 10 ||
		model.CacheReadTokens != 400 || model.CacheCreationTokens != 20 || model.Messages != 2 {
		t.Errorf("model usage = %+v", model)
	}
	if total := fa.DayTokens["2026-09-01"]["anthropic/claude-sonnet-5"]; total != 582 {
		t.Errorf("DayTokens = %v, want 582", fa.DayTokens["2026-09-01"])
	}
}

func TestScanSessionFallsBackToSessionModel(t *testing.T) {
	// 真实 export：assistant 步不带 model 字段，只能回退到会话级模型
	s := &Session{
		ID:              "ses_2",
		ModelProviderID: "openrouter",
		ModelID:         "cohere/north-mini-code:free",
		Messages: []SessionMessage{
			userMsg("m1", at("2026-09-01 09:00:00"), textPart("hi")),
			assistantMsg("a1", at("2026-09-01 09:01:00"), "", "", MessageTokens{Input: 5, Output: 3}),
		},
	}
	fa := ScanSession(s)
	if len(fa.ModelUsage) != 1 {
		t.Fatalf("ModelUsage = %v, want the session-level model", fa.ModelUsage)
	}
	mu := fa.ModelUsage["openrouter/cohere/north-mini-code:free"]
	if mu.TotalTokens() != 8 || mu.Messages != 1 {
		t.Errorf("model usage = %+v", mu)
	}
}

func TestScanSessionEmptySession(t *testing.T) {
	fa := ScanSession(&Session{ID: "ses_empty", Messages: []SessionMessage{
		userMsg("m1", time.Time{}, textPart("zero timestamp")),
		userMsg("m2", at("2026-09-01 09:00:00"), textPart("   ")),
	}})
	if fa.Active || fa.Counted != 0 || fa.Hour != -1 {
		t.Errorf("inactive session: %+v", fa)
	}
}

func TestModelKey(t *testing.T) {
	if got := modelKey("anthropic", "claude-sonnet-5"); got != "anthropic/claude-sonnet-5" {
		t.Errorf("modelKey = %q", got)
	}
	if got := modelKey("", "deepseek-v4"); got != "deepseek-v4" {
		t.Errorf("modelKey = %q", got)
	}
	if got := modelKey("p", ""); got != "p" {
		t.Errorf("modelKey = %q", got)
	}
	if got := modelKey("", ""); got != "unknown" {
		t.Errorf("modelKey = %q", got)
	}
}

// fakeList is a table of (cli, dir) -> sessions used to drive StatsCache.Refresh.
type fakeList struct {
	// cli|dir -> rows
	rows  map[string][]SessionInfo
	fail  map[string]bool // cli|dir -> list error
	calls []string
}

func (f *fakeList) key(cli, dir string) string { return cli + "|" + dir }

func (f *fakeList) list(cli, dir string) ([]SessionInfo, error) {
	f.calls = append(f.calls, f.key(cli, dir))
	if f.fail[f.key(cli, dir)] {
		return nil, errors.New("list failed")
	}
	return f.rows[f.key(cli, dir)], nil
}

func info(id string, updated string) SessionInfo {
	return SessionInfo{SessionID: id, Title: id, Cwd: "/proj", UpdatedAt: at(updated)}
}

// mkCache builds a cache whose `opencode db` enumeration fails, so these tests
// drive the per-bucket `session list` fallback — the default wiring would shell
// out to an installed opencode.
func mkCache(listFn func(string, string) ([]SessionInfo, error),
	fetchFn func(string, string) (*Session, error)) *StatsCache {
	return mkCacheAll(func(string) ([]SessionInfo, error) {
		return nil, errors.New("db unavailable")
	}, listFn, fetchFn)
}

func mkCacheAll(listAllFn func(string) ([]SessionInfo, error),
	listFn func(string, string) ([]SessionInfo, error),
	fetchFn func(string, string) (*Session, error)) *StatsCache {
	c := NewStatsCache()
	c.listAllFn = listAllFn
	c.listFn = listFn
	c.fetchFn = fetchFn
	return c
}

func idsOf(fas []statsagg.FileAgg) (out []string) {
	for _, fa := range fas {
		out = append(out, fa.FileID)
	}
	return out
}

func TestRefreshExportsOnlyChangedSessions(t *testing.T) {
	l := &fakeList{
		rows: map[string][]SessionInfo{
			"oc|/proj": {info("ses_a", "2026-09-01 10:00:00")},
		},
	}
	var fetched []string
	fetch := func(cli, id string) (*Session, error) {
		fetched = append(fetched, id)
		return &Session{ID: id, UpdatedAt: at("2026-09-01 10:00:00"), Messages: []SessionMessage{
			userMsg("m", at("2026-09-01 09:00:00"), textPart("hi")),
		}}, nil
	}
	c := mkCache(l.list, fetch)

	if got := idsOf(c.Refresh([]string{"oc"}, []string{"/proj"}, false)); len(got) != 1 || got[0] != "ses_a" {
		t.Fatalf("first refresh = %v", got)
	}
	// updated 未前进：不再导出
	c.Refresh([]string{"oc"}, []string{"/proj"}, false)
	if len(fetched) != 1 {
		t.Fatalf("fetched %v, want no re-export", fetched)
	}
	// updated 前进：重导出一次
	l.rows["oc|/proj"] = []SessionInfo{info("ses_a", "2026-09-01 11:00:00")}
	c.Refresh([]string{"oc"}, []string{"/proj"}, false)
	if len(fetched) != 2 {
		t.Fatalf("fetched %v, want one re-export", fetched)
	}
	// fresh 强制全量重导出
	c.Refresh([]string{"oc"}, []string{"/proj"}, true)
	if len(fetched) != 3 {
		t.Fatalf("fetched %v, want re-export on fresh", fetched)
	}
}

func TestRefreshDedupesAcrossBuckets(t *testing.T) {
	l := &fakeList{
		rows: map[string][]SessionInfo{
			"oc|/repo":   {info("ses_a", "2026-09-01 10:00:00")},
			"oc|/repo/x": {info("ses_a", "2026-09-01 10:00:00"), info("ses_b", "2026-09-01 10:05:00")},
		},
	}
	var fetched []string
	fetch := func(cli, id string) (*Session, error) {
		fetched = append(fetched, id)
		return &Session{ID: id, Messages: []SessionMessage{
			userMsg("m", at("2026-09-01 09:00:00"), textPart("hi")),
		}}, nil
	}
	c := mkCache(l.list, fetch)
	if got := idsOf(c.Refresh([]string{"oc"}, []string{"/repo", "/repo/x"}, false)); len(got) != 2 {
		t.Fatalf("ids = %v, want both sessions once", got)
	}
	if len(fetched) != 2 {
		t.Fatalf("fetched %v, want 2 exports (shared id once)", fetched)
	}
}

func TestRefreshPrunesVanishedSessions(t *testing.T) {
	l := &fakeList{rows: map[string][]SessionInfo{
		"oc|/proj": {info("ses_a", "2026-09-01 10:00:00"), info("ses_b", "2026-09-01 10:01:00")},
	}}
	fetch := func(cli, id string) (*Session, error) {
		return &Session{ID: id}, nil
	}
	c := mkCache(l.list, fetch)
	if got := idsOf(c.Refresh([]string{"oc"}, []string{"/proj"}, false)); len(got) != 2 {
		t.Fatalf("ids = %v", got)
	}
	// ses_b 从桶里消失
	l.rows["oc|/proj"] = []SessionInfo{info("ses_a", "2026-09-01 10:00:00")}
	if got := idsOf(c.Refresh([]string{"oc"}, []string{"/proj"}, false)); len(got) != 1 || got[0] != "ses_a" {
		t.Fatalf("ids = %v, want ses_a only", got)
	}
}

func TestRefreshKeepsCacheWhenExportFails(t *testing.T) {
	l := &fakeList{rows: map[string][]SessionInfo{
		"oc|/proj": {info("ses_a", "2026-09-01 10:00:00")},
	}}
	fetch := func(cli, id string) (*Session, error) {
		if id == "ses_a" {
			return &Session{ID: id, Messages: []SessionMessage{
				userMsg("m", at("2026-09-01 09:00:00"), textPart("hi")),
			}}, nil
		}
		return nil, errors.New("export failed")
	}
	c := mkCache(l.list, fetch)
	if got := idsOf(c.Refresh([]string{"oc"}, []string{"/proj"}, false)); len(got) != 1 || got[0] != "ses_a" {
		t.Fatalf("ids = %v", got)
	}
	// 导出失败（updated 前进）：保留旧聚合而不是清零
	l.rows["oc|/proj"] = []SessionInfo{info("ses_a", "2026-09-01 11:00:00")}
	got := c.Refresh([]string{"oc"}, []string{"/proj"}, false)
	if len(got) != 1 || got[0].FileID != "ses_a" || got[0].Counted != 1 {
		t.Fatalf("after failed export = %+v, want old aggregate retained", got)
	}
	// 下次刷新重试成功
	l.rows["oc|/proj"] = []SessionInfo{info("ses_a", "2026-09-01 11:00:00")}
	c.Refresh([]string{"oc"}, []string{"/proj"}, false)
	got = c.Refresh([]string{"oc"}, []string{"/proj"}, true)
	if len(got) != 1 || got[0].Counted != 1 {
		t.Fatalf("after retry = %+v", got)
	}
}

func TestRefreshKeepsCacheWhenAllListsFail(t *testing.T) {
	l := &fakeList{rows: map[string][]SessionInfo{
		"oc|/proj": {info("ses_a", "2026-09-01 10:00:00")},
	}}
	fetch := func(cli, id string) (*Session, error) {
		return &Session{ID: id, Messages: []SessionMessage{
			userMsg("m", at("2026-09-01 09:00:00"), textPart("hi")),
		}}, nil
	}
	c := mkCache(l.list, fetch)
	if got := idsOf(c.Refresh([]string{"oc"}, []string{"/proj"}, false)); len(got) != 1 {
		t.Fatalf("ids = %v", got)
	}
	// 全部桶列表失败（CLI 挂了）：保留上次缓存，不把统计整体清零
	l.fail = map[string]bool{"oc|/proj": true}
	if got := c.Refresh([]string{"oc"}, []string{"/proj"}, false); len(got) != 1 || got[0].Counted != 1 {
		t.Fatalf("after list failure = %+v, want cache preserved", got)
	}
	// 有其它桶成功时按真实结果重建（不冻结旧缓存）
	l.fail = map[string]bool{"oc|/repo": true}
	l.rows["oc|/proj2"] = []SessionInfo{info("ses_c", "2026-09-01 10:03:00")}
	if got := c.Refresh([]string{"oc"}, []string{"/proj", "/proj2", "/repo"}, false); len(got) != 2 {
		t.Fatalf("partial list failure = %+v, want the listed sessions only", got)
	}
}

func TestRefreshDBEnumerationIsPrimary(t *testing.T) {
	var dbCalls, fetched []string
	db := func(cli string) ([]SessionInfo, error) {
		dbCalls = append(dbCalls, cli)
		return []SessionInfo{
			info("ses_a", "2026-09-01 10:00:00"),
			info("ses_b", "2026-09-01 10:05:00"),
		}, nil
	}
	list := &fakeList{}
	fetch := func(cli, id string) (*Session, error) {
		fetched = append(fetched, cli+"/"+id)
		return &Session{ID: id, Messages: []SessionMessage{
			userMsg("m", at("2026-09-01 09:00:00"), textPart("hi")),
		}}, nil
	}
	c := mkCacheAll(db, list.list, fetch)
	if got := idsOf(c.Refresh([]string{"oc", "oc2"}, []string{"/proj", "/other"}, false)); len(got) != 2 {
		t.Fatalf("ids = %v, want both sessions", got)
	}
	if len(dbCalls) != 1 || dbCalls[0] != "oc" {
		t.Fatalf("dbCalls = %v, want one query on the first cli only", dbCalls)
	}
	if len(list.calls) != 0 {
		t.Fatalf("bucket lists consulted: %v, want none once the db enumeration succeeds", list.calls)
	}
	// db 枚举成功即全量，导出的会话挂在查库的那个 cli 上
	if len(fetched) != 2 || fetched[0] != "oc/ses_a" || fetched[1] != "oc/ses_b" {
		t.Fatalf("fetched %v, want both sessions via the first cli", fetched)
	}
}

func TestRefreshDBFailureFallsBackToBuckets(t *testing.T) {
	list := &fakeList{rows: map[string][]SessionInfo{
		"oc|/proj": {info("ses_a", "2026-09-01 10:00:00")},
	}}
	dbCalls := 0
	db := func(string) ([]SessionInfo, error) {
		dbCalls++
		return nil, errors.New("db unavailable")
	}
	var fetched []string
	fetch := func(cli, id string) (*Session, error) {
		fetched = append(fetched, id)
		return &Session{ID: id, Messages: []SessionMessage{
			userMsg("m", at("2026-09-01 09:00:00"), textPart("hi")),
		}}, nil
	}
	c := mkCacheAll(db, list.list, fetch)
	if got := idsOf(c.Refresh([]string{"oc"}, []string{"/proj"}, false)); len(got) != 1 || got[0] != "ses_a" {
		t.Fatalf("ids = %v, want the bucket session", got)
	}
	if dbCalls != 1 {
		t.Fatalf("dbCalls = %d, want one attempt", dbCalls)
	}
	if len(list.calls) != 1 || list.calls[0] != "oc|/proj" {
		t.Fatalf("list calls = %v, want the fallback bucket scan", list.calls)
	}
}

func TestRefreshKeepsCacheWhenDBFailsWithoutBuckets(t *testing.T) {
	// db 挂了、又没有桶候选（首次调用时尚无托管会话的 cwd）：
	// 保留上次缓存，不把统计清零。
	list := &fakeList{rows: map[string][]SessionInfo{
		"oc|/proj": {info("ses_a", "2026-09-01 10:00:00")},
	}}
	fetch := func(cli, id string) (*Session, error) {
		return &Session{ID: id, Messages: []SessionMessage{
			userMsg("m", at("2026-09-01 09:00:00"), textPart("hi")),
		}}, nil
	}
	c := mkCache(list.list, fetch)
	if got := idsOf(c.Refresh([]string{"oc"}, []string{"/proj"}, false)); len(got) != 1 {
		t.Fatalf("ids = %v", got)
	}
	if got := c.Refresh([]string{"oc"}, nil, false); len(got) != 1 || got[0].Counted != 1 {
		t.Fatalf("after db failure = %+v, want the cache preserved", got)
	}
	// 有桶可查时按真实结果重建，不冻结旧缓存
	if got := idsOf(c.Refresh([]string{"oc"}, []string{"/proj"}, false)); len(got) != 1 || got[0] != "ses_a" {
		t.Fatalf("rebuild = %v", got)
	}
}

func TestRefreshPrunesWhenDBReturnsEmpty(t *testing.T) {
	// db 查询成功但返回空是真实结果（会话已清空），应当清缓存，
	// 与「db 挂了」的保守路径区分。
	list := &fakeList{rows: map[string][]SessionInfo{
		"oc|/proj": {info("ses_a", "2026-09-01 10:00:00")},
	}}
	fetch := func(cli, id string) (*Session, error) {
		return &Session{ID: id, Messages: []SessionMessage{
			userMsg("m", at("2026-09-01 09:00:00"), textPart("hi")),
		}}, nil
	}
	c := mkCache(list.list, fetch)
	if got := c.Refresh([]string{"oc"}, []string{"/proj"}, false); len(got) != 1 {
		t.Fatalf("ids = %v", got)
	}
	c.listAllFn = func(string) ([]SessionInfo, error) { return []SessionInfo{}, nil }
	if got := idsOf(c.Refresh([]string{"oc"}, []string{"/proj"}, false)); len(got) != 0 {
		t.Fatalf("ids = %v, want the cache pruned", got)
	}
}

func TestRefreshInvalidatesOnCLIChange(t *testing.T) {
	l := &fakeList{rows: map[string][]SessionInfo{
		"oc|/proj":  {info("ses_a", "2026-09-01 10:00:00")},
		"oc2|/proj": {info("ses_a", "2026-09-01 10:00:00")},
	}}
	var fetched []string
	fetch := func(cli, id string) (*Session, error) {
		fetched = append(fetched, cli+"/"+id)
		return &Session{ID: id, Messages: []SessionMessage{
			userMsg("m", at("2026-09-01 09:00:00"), textPart("hi")),
		}}, nil
	}
	c := mkCache(l.list, fetch)
	c.Refresh([]string{"oc"}, []string{"/proj"}, false)
	// 换 cli（用户改了配置）：缓存作废，同一会话重新导出
	c.Refresh([]string{"oc2"}, []string{"/proj"}, false)
	c.Refresh([]string{"oc2"}, []string{"/proj"}, false)
	if len(fetched) != 2 {
		t.Fatalf("fetched %v, want one export per cli set", fetched)
	}
	if fetched[0] != "oc/ses_a" || fetched[1] != "oc2/ses_a" {
		t.Fatalf("fetched %v", fetched)
	}
}

func TestRefreshNoCLIs(t *testing.T) {
	c := mkCache((*fakeList)(nil).list, func(string, string) (*Session, error) { return nil, nil })
	if got := c.Refresh(nil, []string{"/proj"}, false); len(got) != 0 {
		t.Errorf("Refresh(nil) = %v", got)
	}
}

func TestAggregateAcrossEngines(t *testing.T) {
	fas := []statsagg.FileAgg{
		func() statsagg.FileAgg {
			fa := statsagg.NewFileAgg("s1", "a.jsonl", "claude")
			fa.Counted = 3
			fa.Active = true
			fa.DayMsg["2026-09-01"] = 3
			fa.Hour = 9
			return fa
		}(),
		ScanSession(&Session{ID: "ses_x", Messages: []SessionMessage{
			userMsg("m", at("2026-09-01 10:00:00"), textPart("hi")),
			assistantMsg("a", at("2026-09-01 10:01:00"), "anthropic", "claude-sonnet-5",
				MessageTokens{Input: 10}, textPart("yo")),
		}}),
		ScanSession(&Session{ID: "ses_empty"}),
	}
	st := statsagg.Recompute(fas)
	if st.TotalSessions != 2 {
		t.Errorf("TotalSessions = %d, want 2 (inactive skipped)", st.TotalSessions)
	}
	if st.Engines["claude"] != 1 || st.Engines["opencode"] != 1 {
		t.Errorf("Engines = %v", st.Engines)
	}
	if st.TotalMessages != 5 {
		t.Errorf("TotalMessages = %d, want 5", st.TotalMessages)
	}
	if st.DailyActivity[0].SessionCount != 2 {
		t.Errorf("session count per day = %d, want 2 (per-day dedup by path)", st.DailyActivity[0].SessionCount)
	}
}
