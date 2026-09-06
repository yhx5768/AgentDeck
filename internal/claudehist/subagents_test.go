package claudehist

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSubagentFixture writes a session with one subagent transcript (and
// optional meta.json) under a fake HOME, returning (cwd, sessionID, subDir).
func writeSubagentFixture(t *testing.T, meta string, subLines []string) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	cwd := filepath.Join(root, "proj")
	sid := "s1"
	dir := HistoryDir(cwd)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	main := `{"type":"user","uuid":"u1","message":{"role":"user","content":"hello"}}` + "\n" +
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"tool_use","id":"tool_parent1","name":"Agent","input":{"description":"外围探查","subagent_type":"Explore","run_in_background":true}}]}}` + "\n"
	if err := os.WriteFile(HistoryPath(cwd, sid), []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	subDir := filepath.Join(dir, sid, "subagents")
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := ""
	for _, l := range subLines {
		raw += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(subDir, "agent-abc123.jsonl"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if meta != "" {
		if err := os.WriteFile(filepath.Join(subDir, "agent-abc123.meta.json"), []byte(meta), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cwd, sid, subDir
}

func subAssistantLine(uuid, ts string, blocks string) string {
	return `{"type":"assistant","uuid":"` + uuid + `","attributionAgent":"Explore","isSidechain":true,"agentId":"abc123",` +
		`"timestamp":"` + ts + `","message":{"role":"assistant","id":"msg_` + uuid + `","content":` + blocks + `}}`
}

type subagentEvent struct {
	Type            string `json:"type"`
	ParentToolUseID string `json:"parent_tool_use_id"`
	Subagent        *struct {
		Title string `json:"title"`
		Type  string `json:"type"`
	} `json:"subagent"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
			ID   string `json:"id"`
		} `json:"content"`
	} `json:"message"`
}

func TestConvertSubagentsRelayShape(t *testing.T) {
	cwd, sid, _ := writeSubagentFixture(t,
		`{"agentType":"Explore","description":"外围探查","toolUseId":"tool_parent1","spawnDepth":1}`,
		[]string{
			// 任务 prompt（string content）：不产出事件，仅作 fallback title 源
			`{"type":"user","uuid":"su1","isSidechain":true,"agentId":"abc123","timestamp":"2026-09-03T11:27:43.950Z","message":{"role":"user","content":"探查这个仓库"}}`,
			// thinking-only 行：跳过（CLI 实时透传也 strip thinking）
			subAssistantLine("sa1", "2026-09-03T11:28:00.000Z", `[{"type":"thinking","thinking":"内部思考"}]`),
			// tool_use 行：产出 assistant 事件
			subAssistantLine("sa2", "2026-09-03T11:28:01.000Z", `[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/x"}}]`),
			// text 行：产出 assistant 事件
			subAssistantLine("sa3", "2026-09-03T11:37:27.000Z", `[{"type":"text","text":"调查报告全文"}]`),
			// 子代理内部的 tool_result 回显：产出 user 事件
			`{"type":"user","uuid":"sr1","isSidechain":true,"agentId":"abc123","timestamp":"2026-09-03T11:28:02.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"file body"}]}}`,
		})
	events, err := ConvertSubagents(cwd, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3 (thinking-only line skipped)", len(events))
	}
	// ts 升序：tool_use(sa2) → tool_result(sr1) → text(sa3)
	if !events[0].Ts.Before(events[1].Ts) || !events[1].Ts.Before(events[2].Ts) {
		t.Fatalf("events not ts-ordered")
	}
	var tu, tr, tx subagentEvent
	for i, dst := range []*subagentEvent{&tu, &tr, &tx} {
		if err := json.Unmarshal(events[i].Data, dst); err != nil {
			t.Fatal(err)
		}
	}
	if tu.Type != "assistant" || tu.ParentToolUseID != "tool_parent1" || len(tu.Message.Content) != 1 ||
		tu.Message.Content[0].Type != "tool_use" || tu.Message.Content[0].Name != "Read" {
		t.Fatalf("tool_use event = %+v", tu.Message.Content)
	}
	if tr.Type != "user" || tr.ParentToolUseID != "tool_parent1" || len(tr.Message.Content) != 1 ||
		tr.Message.Content[0].Type != "tool_result" {
		t.Fatalf("tool_result event = %+v", tr)
	}
	if tx.Subagent == nil || tx.Subagent.Title != "外围探查" || tx.Subagent.Type != "Explore" {
		t.Fatalf("subagent meta = %+v, want 外围探查/Explore", tx.Subagent)
	}
	if len(tx.Message.Content) != 1 || tx.Message.Content[0].Text != "调查报告全文" {
		t.Fatalf("text content = %+v, want single text block", tx.Message.Content)
	}
	for i, ev := range events {
		if ev.Kind != "stream" {
			t.Fatalf("event %d kind = %s, want stream", i, ev.Kind)
		}
	}
}

func TestConvertSubagentsFallbacksAndOrder(t *testing.T) {
	// 无 meta.json：parent key 回退 agent:<id>，title 取任务 prompt，type 取 attributionAgent。
	// 两个 agent 文件乱序写盘，产出需按 ts 升序。
	cwd, sid, subDir := writeSubagentFixture(t, "", []string{
		`{"type":"user","uuid":"su1","isSidechain":true,"agentId":"abc123","timestamp":"2026-09-03T11:28:00.000Z","message":{"role":"user","content":"探查这个仓库"}}`,
		subAssistantLine("sb2", "2026-09-03T11:40:00.000Z", `[{"type":"text","text":"后到的报告"}]`),
	})
	// 同 fixture 目录里再放一个更早的 agent 文件
	raw := subAssistantLine("sb1", "2026-09-03T11:30:00.000Z", `[{"type":"text","text":"先到的报告"}]`)
	if err := os.WriteFile(filepath.Join(subDir, "agent-def456.jsonl"), []byte(raw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := ConvertSubagents(cwd, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if !events[0].Ts.Before(events[1].Ts) {
		t.Fatalf("events not ts-ordered: %v before %v", events[0].Ts, events[1].Ts)
	}
	var first subagentEvent
	if err := json.Unmarshal(events[0].Data, &first); err != nil {
		t.Fatal(err)
	}
	if first.ParentToolUseID != "agent:def456" {
		t.Fatalf("fallback parent key = %q, want agent:def456", first.ParentToolUseID)
	}
	if first.Subagent == nil {
		t.Fatal("subagent meta missing")
	}
	// def456 文件没有 user 行 → title 回退为空串由前端兜底；agentType 取 attributionAgent
	if first.Subagent.Type != "Explore" {
		t.Fatalf("fallback type = %q, want Explore", first.Subagent.Type)
	}
	// abc123 文件第一条 user 行是任务 prompt → title 回退取它
	var second subagentEvent
	if err := json.Unmarshal(events[1].Data, &second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Subagent.Title, "探查") || second.ParentToolUseID != "agent:abc123" {
		t.Fatalf("second event = ptid=%s title=%q, want agent:abc123 + prompt title", second.ParentToolUseID, second.Subagent.Title)
	}
}

func TestConvertSubagentsMissingDir(t *testing.T) {
	cwd, sid := writeHist(t, []string{realUserLine("u1", "hello")})
	events, err := ConvertSubagents(cwd, sid)
	if events != nil {
		t.Fatalf("events = %v, want nil", events)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want Is(os.ErrNotExist)", err)
	}
}
