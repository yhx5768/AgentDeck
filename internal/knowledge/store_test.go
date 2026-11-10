package knowledge

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeAndMatchQuery(t *testing.T) {
	norm := NormalizeFTS("知识沉淀")
	for _, want := range []string{"知", "识", "沉", "淀", "知识", "识沉", "沉淀"} {
		if !strings.Contains(" "+norm+" ", " "+want+" ") {
			t.Fatalf("NormalizeFTS(%q) = %q, missing token %q", "知识沉淀", norm, want)
		}
	}
	if got := MatchQuery("看板"); got != `"看板"` {
		t.Fatalf("MatchQuery(看板) = %q", got)
	}
	if got := MatchQuery("重编号"); got != `"重编 编号"` {
		t.Fatalf("MatchQuery(重编号) = %q", got)
	}
	if got := MatchQuery("SSE 重编号"); got != `"sse" AND "重编 编号"` {
		t.Fatalf("MatchQuery(SSE 重编号) = %q", got)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "knowledge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestStoreCreateSearchApprove(t *testing.T) {
	st := newTestStore(t)
	entry, duplicate, err := st.Create(CreateInput{
		Title:      "AgentDeck SSE 重编号",
		Summary:    "每个连接有自己的 sendSeq，rewind 用 checkpoint uuid。",
		Body:       "## 结论\nSSE 的 sendSeq 与存储 seq 无关。知识沉淀时要保留证据。",
		Project:    "AgentDeck",
		Tags:       []string{"SSE", "rewind"},
		SourceKind: "manual",
		Owner:      "mainAgent",
		Actor:      "mainAgent",
		Evidence:   []Evidence{{Kind: "task", Ref: "T-1", Locator: "timeline:seq=1", Quote: "sendSeq"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if duplicate || entry.Status != "draft" {
		t.Fatalf("unexpected create result: duplicate=%v status=%s", duplicate, entry.Status)
	}
	if len(entry.Tags) != 2 || len(entry.Evidence) != 1 {
		t.Fatalf("tags/evidence not loaded: %#v", entry)
	}

	for _, query := range []string{"重编号", "编号", "知识", "沉淀", " SSE "} {
		res, err := st.Search(SearchOptions{Query: query, IncludeDrafts: true})
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		if res.Total != 1 || len(res.Entries) != 1 {
			t.Fatalf("search %q: total=%d entries=%d", query, res.Total, len(res.Entries))
		}
	}
	if res, err := st.Search(SearchOptions{Query: "重编号"}); err != nil || res.Total != 0 {
		t.Fatalf("draft should be hidden from default search: total=%d err=%v", res.Total, err)
	}

	approved := "approved"
	updated, err := st.Update(entry.ID, UpdateInput{Status: &approved, Reviewer: &entry.Owner, Actor: "mainAgent", Action: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "approved" || updated.LastVerifiedAt == "" {
		t.Fatalf("approve did not set status/verified: %#v", updated)
	}
	if res, err := st.Search(SearchOptions{Query: "重编号"}); err != nil || res.Total != 1 {
		t.Fatalf("approved entry not searchable: total=%d err=%v", res.Total, err)
	}
}

func TestStoreDedupe(t *testing.T) {
	st := newTestStore(t)
	first, dup, err := st.Create(CreateInput{Title: "同一结论", Body: "SSE seq 无关", Project: "AgentDeck", Actor: "user"})
	if err != nil || dup {
		t.Fatalf("first create: dup=%v err=%v", dup, err)
	}
	second, dup, err := st.Create(CreateInput{
		Title: "同一结论", Body: "SSE seq 无关", Project: "AgentDeck", Actor: "worker",
		Evidence: []Evidence{{Kind: "artifact", Ref: "A-1", Locator: "report.md"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !dup || second.ID != first.ID {
		t.Fatalf("expected duplicate of %s, got %s dup=%v", first.ID, second.ID, dup)
	}
	if len(second.Evidence) != 1 {
		t.Fatalf("duplicate evidence not appended: %#v", second.Evidence)
	}
}

func TestStoreSensitiveQuarantine(t *testing.T) {
	st := newTestStore(t)
	entry, _, err := st.Create(CreateInput{
		Title: "连接配置", Body: "token=abcdefgh12345678\n正常说明", Project: "AgentDeck", Actor: "worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Status != "quarantined" || entry.Sensitivity != "sensitive" {
		t.Fatalf("sensitive content not quarantined: %#v", entry)
	}
	if strings.Contains(entry.Body, "abcdefgh12345678") || !strings.Contains(entry.Body, "[REDACTED:") {
		t.Fatalf("body not redacted: %q", entry.Body)
	}
	if res, err := st.Search(SearchOptions{Query: "连接配置", IncludeDrafts: true, IncludeSensitive: true}); err != nil || res.Total != 0 {
		t.Fatalf("quarantined entry should not be indexed: total=%d err=%v", res.Total, err)
	}
}

func TestStoreSummariesAndTaskGuard(t *testing.T) {
	st := newTestStore(t)
	if _, _, err := st.Create(CreateInput{Title: "任务结论", Body: "结论", Project: "P", SourceTaskID: "T-1", Actor: "mainAgent"}); err != nil {
		t.Fatal(err)
	}
	has, err := st.HasTaskEntry("T-1")
	if err != nil || !has {
		t.Fatalf("HasTaskEntry = %v, %v", has, err)
	}
	summaries, counts, err := st.SummariesForTasks([]string{"T-1", "T-2"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if counts["T-1"] != 1 || len(summaries["T-1"]) != 1 || counts["T-2"] != 0 {
		t.Fatalf("unexpected summaries: %#v counts=%#v", summaries, counts)
	}
}

func TestStoreTypeVerificationScope(t *testing.T) {
	st := newTestStore(t)
	entry, _, err := st.Create(CreateInput{
		Title: "编译集群连接方式", Body: "通过 relay 非交互执行脚本。",
		Project: "编译集群", Type: "env_map", Verification: "tested",
		Scope: "三台 Linux 编译机", LastVerifiedAt: "2026-11-10 10:00:00", Actor: "mainAgent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Type != "env_map" || entry.Verification != "tested" || entry.Scope != "三台 Linux 编译机" {
		t.Fatalf("type fields not persisted: %#v", entry)
	}
	res, err := st.Search(SearchOptions{Query: "relay", Type: "env_map", Verification: "tested", IncludeDrafts: true})
	if err != nil || res.Total != 1 {
		t.Fatalf("type/verification filter: total=%d err=%v", res.Total, err)
	}
	res, err = st.Search(SearchOptions{Query: "relay", Verification: "observed", IncludeDrafts: true})
	if err != nil || res.Total != 0 {
		t.Fatalf("verification mismatch should be empty: total=%d err=%v", res.Total, err)
	}
	if _, _, err := st.Create(CreateInput{Title: "bad", Body: "x", Type: "nonsense"}); err == nil {
		t.Fatal("invalid type should fail")
	}
}

func TestMigrationAddsTypeColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knowledge-old.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{
		"DROP INDEX IF EXISTS idx_knowledge_type",
		"DROP INDEX IF EXISTS idx_knowledge_verification",
		"ALTER TABLE knowledge_entries DROP COLUMN type",
		"ALTER TABLE knowledge_entries DROP COLUMN verification",
		"ALTER TABLE knowledge_entries DROP COLUMN scope",
	} {
		if _, err := st.db.Exec(ddl); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	entry, _, err := migrated.Create(CreateInput{Title: "迁移后的条目", Body: "默认字段", Actor: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Type != "playbook" || entry.Verification != "unverified" || entry.Scope != "" {
		t.Fatalf("migration defaults: %#v", entry)
	}
	if _, err := migrated.db.Exec("CREATE INDEX IF NOT EXISTS idx_knowledge_type ON knowledge_entries(type, status, updated_at DESC)"); err != nil {
		t.Fatalf("type index missing after migration: %v", err)
	}
}
