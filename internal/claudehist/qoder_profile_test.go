package claudehist

// Qoder CLI CN 是 Claude Code 的 fork：transcript 结构同构，只是 home 目录不同
// （~/.qoder-cn vs ~/.claude）。这里用真实抓下来的 qoder transcript 钉住
// Profile 参数化后的读取行为——尤其是 qoder 独有的记账行（runtime-config、
// active-leaf、last-prompt、workspace-directories）必须被安静跳过，以及子代理
// 目录的命名差异（agent-<type>-<hash>.jsonl）不影响解析。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realQoderTranscript 摘自 qoderclicn 1.1.65 的一次真实会话
// （"用 Bash 工具执行 echo hello-qoder"），逐行 verbatim：qoder 独有的记账行
// 与 claude 的 message 行混在一起，assistant 的 model 记的是归一化 key。
var realQoderTranscript = []string{
	`{"type":"workspace-directories","sessionId":"sid","directories":["/w"]}`,
	`{"type":"runtime-config","sessionId":"sid","model":"qfmodel","reasoningEffort":null,"contextWindow":null,"generation":null,"timestamp":1790768333470}`,
	`{"type":"worktree-state","sessionId":"sid","worktreeSession":null}`,
	`{"type":"user","uuid":"u1","timestamp":"2026-09-30T11:38:53.951Z","cwd":"/w","sessionId":"sid","message":{"role":"user","content":"用 Bash 工具执行 echo hello-qoder，然后只回复命令的输出"},"permissionMode":"default","origin":{"kind":"human"},"isSidechain":false,"parentUuid":null}`,
	`{"type":"attachment","uuid":"at1","sessionId":"sid","attachment":{"type":"skill_listing","content":"- mcp-config: ..."}}`,
	`{"type":"active-leaf","sessionId":"sid","leafUuid":"a1","explicit":true,"timestamp":1790768333470}`,
	`{"type":"assistant","uuid":"a1","timestamp":"2026-09-30T11:38:57.338Z","cwd":"/w","sessionId":"sid","isSidechain":false,"message":{"id":"chatcmpl-1","type":"message","role":"assistant","model":"qfmodel","stop_reason":"tool_use","content":[{"type":"tool_use","id":"call_1","name":"Bash","input":{"command":"echo hello-qoder"}}],"usage":{"input_tokens":0,"output_tokens":0}}}`,
	`{"type":"user","uuid":"u2","timestamp":"2026-09-30T11:38:57.446Z","cwd":"/w","sessionId":"sid","isSidechain":false,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"hello-qoder"}]}}`,
	`{"type":"assistant","uuid":"a2","timestamp":"2026-09-30T11:38:59.100Z","cwd":"/w","sessionId":"sid","isSidechain":false,"message":{"id":"chatcmpl-2","type":"message","role":"assistant","model":"qfmodel","stop_reason":"end_turn","content":[{"type":"text","text":"hello-qoder"}],"usage":{"input_tokens":0,"output_tokens":0}}}`,
	`{"type":"last-prompt","sessionId":"sid","lastPrompt":"用 Bash 工具执行 echo hello-qoder"}`,
}

// writeProfileHist 把 jsonl 行写进 <HOME>/<profile.Home>/projects/<slug>/ 下，
// HOME 指向临时目录，不碰真实的 ~/.claude 或 ~/.qoder-cn。
func writeProfileHist(t *testing.T, p Profile, cwd, sid string, lines []string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	dir := HistoryDirIn(p, cwd)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := HistoryPathIn(p, cwd, sid)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestQoderProfileConvertRealTranscript: 真实 qoder transcript 走同一套转换器
// 产出的事件序列——user 事件（落盘那一类）+ assistant/tool_result 的 stream
// 事件，qoder 的记账行一个都不许冒泡成消息。
func TestQoderProfileConvertRealTranscript(t *testing.T) {
	cwd, sid := "/w", "sid"
	writeProfileHist(t, QoderProfile, cwd, sid, realQoderTranscript)

	events, err := ConvertIn(QoderProfile, cwd, sid)
	if err != nil {
		t.Fatalf("ConvertIn: %v", err)
	}
	var kinds, streamTypes []string
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == "stream" {
			var obj struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(ev.Data, &obj); err != nil {
				t.Fatalf("stream event not JSON: %v", err)
			}
			streamTypes = append(streamTypes, obj.Type)
		}
	}
	// 1 个 user（真实提示词）+ 2 个 assistant + 1 个 tool_result user 行。
	wantStream := []string{"assistant", "user", "assistant"}
	if len(events) != 4 {
		t.Fatalf("got %d events (%v), want 4", len(events), kinds)
	}
	if events[0].Kind != "user" {
		t.Errorf("first event kind = %q, want user", events[0].Kind)
	}
	for i, want := range wantStream {
		if streamTypes[i] != want {
			t.Errorf("stream event %d type = %q, want %q", i, streamTypes[i], want)
		}
	}
	// user 事件的文本必须与 transcript 里的提示词逐字一致——mergeStoredAndClaude
	// 靠这个去重 cc-webui 自己落的 echo，qoder 不像 opencode 那样加回显引号。
	var u struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(events[0].Data, &u); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u.Text, "用 Bash 工具执行 echo hello-qoder") {
		t.Errorf("user echo = %q", u.Text)
	}
	// 记账行（runtime-config/active-leaf/last-prompt/workspace-directories/
	// attachment/worktree-state）都不带 message，必须被跳过而不是渲染出来。
	for _, s := range streamTypes {
		if s == "system" {
			t.Error("a bookkeeping line leaked into the event stream as system")
		}
	}
}

// 两个 Profile 指向不同的 home，互不串味：同一 cwd+sid 在 qoder 根下存在时，
// claude 根必须读不到（否则会把 qoder 会话混进 claude 的回放/统计）。
func TestProfilesAreIndependentRoots(t *testing.T) {
	cwd, sid := "/w", "sid"
	root := writeProfileHist(t, QoderProfile, cwd, sid, realQoderTranscript)

	if !strings.HasPrefix(HistoryDirIn(QoderProfile, cwd), filepath.Join(root, ".qoder-cn", "projects")) {
		t.Errorf("qoder history dir = %s", HistoryDirIn(QoderProfile, cwd))
	}
	if _, err := ConvertIn(ClaudeProfile, cwd, sid); err == nil {
		t.Error("claude profile read a qoder transcript")
	}
	if _, err := ConvertIn(QoderProfile, cwd, sid); err != nil {
		t.Errorf("qoder profile failed to read its own transcript: %v", err)
	}
	// 同理，另一个 profile 的会话也不该出现在 qoder 的列表/统计里。
	if list, err := ListIn(ClaudeProfile, ""); err == nil && len(list) != 0 {
		t.Errorf("claude list saw %d qoder sessions", len(list))
	}
}

// 子代理 transcript：qoder 的文件名把 agent 类型也编进去了
// （agent-general-purpose-<hash>.jsonl，claude 是 agent-<id>.jsonl），meta.json
// 的键则完全一致。解析必须两种都能吃。
func TestQoderProfileSubagents(t *testing.T) {
	cwd, sid := "/w", "sid"
	writeProfileHist(t, QoderProfile, cwd, sid, realQoderTranscript)

	subDir := filepath.Join(HistoryDirIn(QoderProfile, cwd), sid, "subagents")
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "agent-general-purpose-95e8db21abed4584"
	meta := `{"agentType":"general-purpose","toolUseId":"call_1","description":"读取 a.txt 内容","invocationName":"general-purpose"}`
	if err := os.WriteFile(filepath.Join(subDir, name+".meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"user","uuid":"s0","timestamp":"2026-09-30T11:40:00.000Z","isSidechain":true,"parent_tool_use_id":"call_1","agentId":"general-purpose","message":{"role":"user","content":"读取 a.txt 内容"}}`,
		`{"type":"assistant","uuid":"s1","timestamp":"2026-09-30T11:40:01.000Z","isSidechain":true,"agentId":"general-purpose","message":{"id":"chatcmpl-s1","role":"assistant","content":[{"type":"text","text":"文件内容是 x"}]}}`,
		`{"type":"last-prompt","sessionId":"sid","lastPrompt":"读取 a.txt 内容"}`,
	}
	if err := os.WriteFile(filepath.Join(subDir, name+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	events, err := ConvertSubagentsIn(QoderProfile, cwd, sid)
	if err != nil {
		t.Fatalf("ConvertSubagentsIn: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d subagent events, want 1 (assistant text only)", len(events))
	}
	var ev struct {
		ParentToolUseID string `json:"parent_tool_use_id"`
		Subagent        struct {
			Title string `json:"title"`
			Type  string `json:"type"`
		} `json:"subagent"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(events[0].Data, &ev); err != nil {
		t.Fatal(err)
	}
	// meta.json 提供的 toolUseId 让卡片挂到父会话的 Agent 工具调用上。
	if ev.ParentToolUseID != "call_1" {
		t.Errorf("parent_tool_use_id = %q, want call_1", ev.ParentToolUseID)
	}
	if ev.Subagent.Title != "读取 a.txt 内容" || ev.Subagent.Type != "general-purpose" {
		t.Errorf("subagent label = %+v", ev.Subagent)
	}
	if len(ev.Message.Content) != 1 || ev.Message.Content[0].Text != "文件内容是 x" {
		t.Errorf("content = %+v", ev.Message.Content)
	}
}

// 统计：engine 记 qoder，且模型名用调用方给的提示（transcript 里只有 qfmodel）。
func TestQoderProfileStats(t *testing.T) {
	cwd, sid := "/w", "sid"
	root := writeProfileHist(t, QoderProfile, cwd, sid, realQoderTranscript)

	fas := NewStatsCacheFor(QoderProfile.Engine).
		RefreshWithModels(HistoryDirIn(QoderProfile, ""), map[string]string{sid: "Qwen3.8-Flash"}, true)
	if len(fas) != 1 {
		t.Fatalf("got %d aggregates, want 1", len(fas))
	}
	fa := fas[0]
	if fa.Engine != QoderProfile.Engine {
		t.Errorf("engine = %q, want %q", fa.Engine, QoderProfile.Engine)
	}
	// 1 条 user + 2 条 assistant（按 message.id 去重后仍是 2 条不同 id）。
	if fa.Counted != 3 {
		t.Errorf("counted = %d, want 3", fa.Counted)
	}
	// 模型分桶必须是真名，不是 transcript 里的 qfmodel。
	if _, ok := fa.ModelUsage["Qwen3.8-Flash"]; !ok {
		t.Errorf("model usage = %+v, want a Qwen3.8-Flash bucket", fa.ModelUsage)
	}
	if _, ok := fa.ModelUsage["qfmodel"]; ok {
		t.Error("the opaque transcript key leaked into the model breakdown")
	}

	// 不给提示时退回 transcript 自己的值——总比丢数据强。
	fas = NewStatsCacheFor(QoderProfile.Engine).
		RefreshWithModels(HistoryDirIn(QoderProfile, ""), nil, true)
	if _, ok := fas[0].ModelUsage["qfmodel"]; !ok {
		t.Errorf("without a hint the transcript key should be used: %+v", fas[0].ModelUsage)
	}

	// claude 侧的缓存不读 qoder 的目录，两边统计互不污染。
	if got := NewStatsCache().Refresh(HistoryDirIn(ClaudeProfile, ""), true); len(got) != 0 {
		t.Errorf("claude cache picked up %d qoder sessions", len(got))
	}
	_ = root
}

// MoveSessionIn/RenameSessionIn 对 qoder 同样生效：改工作区要把 transcript
// 挪到新 slug，否则 --resume 找不到会话。
func TestQoderProfileMoveAndRename(t *testing.T) {
	cwd, sid := "/old-proj", "sid"
	writeProfileHist(t, QoderProfile, cwd, sid, realQoderTranscript)

	if err := RenameSessionIn(QoderProfile, cwd, sid, "worker-1"); err != nil {
		t.Fatalf("RenameSessionIn: %v", err)
	}
	raw, err := os.ReadFile(HistoryPathIn(QoderProfile, cwd, sid))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"customTitle":"worker-1"`) {
		t.Error("qoder transcript did not get the custom-title line")
	}

	moved, err := MoveSessionIn(QoderProfile, cwd, "/new-proj", sid)
	if err != nil || !moved {
		t.Fatalf("MoveSessionIn = %v, %v", moved, err)
	}
	if _, err := os.Stat(HistoryPathIn(QoderProfile, "/new-proj", sid)); err != nil {
		t.Errorf("transcript not at the new slug: %v", err)
	}
	if _, err := os.Stat(HistoryPathIn(QoderProfile, cwd, sid)); !os.IsNotExist(err) {
		t.Error("old transcript still present after move")
	}
	// 挪完之后仍能回放。
	if _, err := ConvertIn(QoderProfile, "/new-proj", sid); err != nil {
		t.Errorf("ConvertIn after move: %v", err)
	}
}

// ListIn 用 qoder 根列会话：标题取首条真实提示词，cwd 取 transcript 里记录的。
func TestQoderProfileList(t *testing.T) {
	cwd, sid := "/w", "sid-abc"
	writeProfileHist(t, QoderProfile, cwd, sid, realQoderTranscript)

	list, err := ListIn(QoderProfile, cwd)
	if err != nil {
		t.Fatalf("ListIn: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d sessions, want 1", len(list))
	}
	if list[0].SessionID != "sid-abc" {
		t.Errorf("session id = %q", list[0].SessionID)
	}
	if list[0].Cwd != "/w" {
		t.Errorf("cwd = %q, want /w", list[0].Cwd)
	}
	if !strings.HasPrefix(list[0].Title, "用 Bash 工具执行 echo hello-qoder") {
		t.Errorf("title = %q", list[0].Title)
	}
	// 按另一个 cwd 过滤应为空。
	if l, _ := ListIn(QoderProfile, "/elsewhere"); len(l) != 0 {
		t.Errorf("cwd filter leaked %d sessions", len(l))
	}
}

// PreviewIn 走 qoder 根。口径与 claude 一致：只展示有文本的消息，所以那条纯
// tool_use 的 assistant 步不进预览（2 条而不是 3 条）。
func TestQoderProfilePreview(t *testing.T) {
	cwd, sid := "/w", "sid"
	writeProfileHist(t, QoderProfile, cwd, sid, realQoderTranscript)

	msgs := PreviewIn(QoderProfile, cwd, sid, 8)
	if len(msgs) != 2 {
		t.Fatalf("got %d preview messages, want 2 (user + final text)", len(msgs))
	}
	if msgs[0].Role != "user" || !strings.HasPrefix(msgs[0].Text, "用 Bash") {
		t.Errorf("first = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Text != "hello-qoder" {
		t.Errorf("last = %+v", msgs[1])
	}
}
