package codexhist

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseModels(t *testing.T) {
	raw := []byte(`{"models":[
	  {"slug":"gpt-6-sol","display_name":"GPT-6-Sol","visibility":"list","supported_in_api":true,
	   "default_reasoning_level":"high",
	   "supported_reasoning_levels":[{"effort":"low"},{"effort":"high"},{"effort":"ultra"}],
	   "input_modalities":["text","image"],"context_window":272000},
	  {"slug":"hidden","display_name":"Hidden","visibility":"hide",
	   "supported_reasoning_levels":[{"effort":"low"}]},
	  {"slug":"api-off","display_name":"API Off","visibility":"list","supported_in_api":false}
	]}`)
	cat, err := ParseModels(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Models) != 1 {
		t.Fatalf("models = %d, want 1 (%+v)", len(cat.Models), cat.Models)
	}
	m := cat.Models[0]
	if m.ID != "gpt-6-sol" || m.Name != "GPT-6-Sol" || m.DefaultEffort != "high" {
		t.Fatalf("unexpected model: %+v", m)
	}
	if len(m.Efforts) != 3 || m.Efforts[2] != "ultra" {
		t.Fatalf("efforts: %v", m.Efforts)
	}
	if !cat.EffortOK("gpt-6-sol", "ultra") || cat.EffortOK("gpt-6-sol", "bogus") {
		t.Fatalf("EffortOK mismatch")
	}
}

func TestLiveAdaptToolAndTurnScopedIDs(t *testing.T) {
	tx := NewToolTx()
	lines := []string{
		`{"type":"thread.started","thread_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"one"}}`,
		`{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"echo hi","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"echo hi","aggregated_output":"hi\n","exit_code":0,"status":"completed"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":3,"output_tokens":2,"reasoning_output_tokens":1}}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"two"}}`,
	}
	var got []any
	for _, l := range lines {
		ev, ok := ParseLive([]byte(l))
		if !ok {
			t.Fatalf("parse failed: %s", l)
		}
		got = append(got, ev.Adapt(tx)...)
		if ev.Type == "turn.completed" {
			got = append(got, UsagePayload(ev.Usage, "gpt-6-sol", ev.TS))
		}
	}
	if len(got) != 5 {
		t.Fatalf("payloads = %d, want 5: %#v", len(got), got)
	}
	// Tool card must appear before its result, and the result must carry the
	// aggregated output.
	var blocks []map[string]any
	for _, p := range got {
		b, _ := json.Marshal(p)
		var obj map[string]any
		_ = json.Unmarshal(b, &obj)
		blocks = append(blocks, obj)
	}
	text1 := blocks[0]["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	text2 := blocks[4]["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if text1["id"] == text2["id"] {
		t.Fatalf("assistant ids collide across turns: %v", text1["id"])
	}
	if text2["text"] != "two" {
		t.Fatalf("second turn text = %v", text2["text"])
	}
}

func TestParseRolloutReplayAndStats(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "10", "20")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "11111111-2222-3333-4444-555555555555"
	lines := []string{
		`{"timestamp":"2026-10-20T08:00:00.000Z","type":"session_meta","payload":{"session_id":"` + id + `","id":"` + id + `","timestamp":"2026-10-20T08:00:00.000Z","cwd":"/tmp/x","model_provider":"oneapi"}}`,
		`{"timestamp":"2026-10-20T08:00:01.000Z","type":"turn_context","payload":{"model":"gpt-6-sol","cwd":"/tmp/x"}}`,
		`{"timestamp":"2026-10-20T08:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>noise</environment_context>"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["environments.environment_context"]}}}`,
		`{"timestamp":"2026-10-20T08:00:03.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello codex"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}}}`,
		`{"timestamp":"2026-10-20T08:00:04.000Z","type":"response_item","payload":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"planning"}]}}`,
		`{"timestamp":"2026-10-20T08:00:05.000Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_1","content":[{"type":"output_text","text":"I will run it"}]}}`,
		`{"timestamp":"2026-10-20T08:00:06.000Z","type":"response_item","payload":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\":\"echo hi\"}"}}`,
		`{"timestamp":"2026-10-20T08:00:07.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"Process exited with code 0\nOutput:\nhi\n"}}`,
		`{"timestamp":"2026-10-20T08:00:08.000Z","type":"token_usage_record","payload":{"usage":{"input_tokens":10,"cached_input_tokens":3,"cache_write_input_tokens":0,"output_tokens":2,"reasoning_output_tokens":1}}}`,
		`{"timestamp":"2026-10-20T08:00:09.000Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_2","content":[{"type":"output_text","text":"done"}]}}`,
	}
	path := filepath.Join(dir, "rollout-2026-10-20T08-00-00-"+id+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	list, err := List(home)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	if list[0].Title != "hello codex" || list[0].Cwd != "/tmp/x" {
		t.Fatalf("info = %+v", list[0])
	}

	sess, err := FetchSession(home, id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ModelID != "gpt-6-sol" || sess.Provider != "oneapi" {
		t.Fatalf("model/provider = %q/%q", sess.ModelID, sess.Provider)
	}
	evs := sess.ToEvents()
	var userText, assistant, thinking, toolUse, toolResult string
	usageSeen := false
	for _, ev := range evs {
		var obj map[string]any
		_ = json.Unmarshal(ev.Data, &obj)
		switch ev.Kind {
		case "user":
			userText, _ = obj["text"].(string)
		case "stream":
			msg, _ := obj["message"].(map[string]any)
			blocks, _ := msg["content"].([]any)
			if len(blocks) == 0 {
				if _, ok := msg["usage"]; ok {
					usageSeen = true
				}
				continue
			}
			b, _ := blocks[0].(map[string]any)
			switch b["type"] {
			case "text":
				assistant += b["text"].(string)
			case "thinking":
				thinking = b["thinking"].(string)
			case "tool_use":
				toolUse = b["id"].(string)
			case "tool_result":
				toolResult = b["tool_use_id"].(string)
			}
		}
	}
	if userText != "hello codex" {
		t.Fatalf("user text = %q", userText)
	}
	if assistant != "I will run itdone" {
		t.Fatalf("assistant = %q", assistant)
	}
	if thinking != "planning" || toolUse != "call_1" || toolResult != "call_1" || !usageSeen {
		t.Fatalf("thinking=%q toolUse=%q result=%q usage=%v", thinking, toolUse, toolResult, usageSeen)
	}

	fa := ScanSession(sess)
	if !fa.Active || fa.Counted != 3 {
		t.Fatalf("agg counted = %d active=%v", fa.Counted, fa.Active)
	}
	if fa.ToolUsage["exec_command"] != 1 {
		t.Fatalf("tool usage = %v", fa.ToolUsage)
	}
	mu := fa.ModelUsage["oneapi/gpt-6-sol"]
	if mu.InputTokens != 10 || mu.OutputTokens != 2 || mu.ReasoningTokens != 1 || mu.CacheReadTokens != 3 {
		t.Fatalf("model usage = %+v", mu)
	}
}

func TestSessionIDFromPathAndFind(t *testing.T) {
	id := "abcdef01-2345-6789-abcd-ef0123456789"
	p := "/x/sessions/2026/01/02/rollout-2026-01-02T03-04-05-" + id + ".jsonl"
	if got := sessionIDFromPath(p); got != id {
		t.Fatalf("id = %q, want %q", got, id)
	}
}

// codex stores an attached image as <image …> + input_image + </image> blocks
// plus the real prompt. The wrapper tags must not become user text: cc-webui's
// stored echo has only the prompt, and a mismatch makes the same message show
// as two bubbles after switching away and back.
func TestUserTextSkipsImageWrappers(t *testing.T) {
	blocks := []contentBlock{
		{Type: "input_text", Text: `<image name=[Image #1] path="/tmp/image.png">`},
		{Type: "input_image"},
		{Type: "input_text", Text: "</image>"},
		{Type: "input_text", Text: "看看这张图\n\n（用户上传了 1 个文件，已保存到磁盘：）\n- /tmp/image.png"},
	}
	got := userText(blocks)
	want := "看看这张图\n\n（用户上传了 1 个文件，已保存到磁盘：）\n- /tmp/image.png"
	if got != want {
		t.Fatalf("userText = %q, want %q", got, want)
	}
}

func TestLoadCatalogPrefersWrapperCatalog(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "model-catalogs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"models":[
	  {"slug":"GLM-5.3","display_name":"GLM-5.3","visibility":"list","supported_in_api":true,
	   "supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}],
	   "input_modalities":["text"],"context_window":1000000},
	  {"slug":"DeepSeek-V4-Pro","display_name":"DeepSeek-V4-Pro","visibility":"list","supported_in_api":true,
	   "supported_reasoning_levels":[{"effort":"medium"}],"input_modalities":["text"]}
	]}`
	if err := os.WriteFile(filepath.Join(dir, "oneapi-models.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	cat, err := LoadCatalogFor("ducx", home, "oneapi", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Models) != 2 || cat.Models[0].ID != "GLM-5.3" {
		t.Fatalf("catalog = %+v", cat.Models)
	}
	if !cat.EffortOK("GLM-5.3", "high") {
		t.Fatalf("wrapper efforts missing: %+v", cat.Models[0])
	}
}

func TestParseModelsCommand(t *testing.T) {
	cat, err := parseModelsCommand([]byte("Available Models:\nGLM-5.3-Flash, DeepSeek-V4-Pro, gpt-6.1-sol\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Models) != 3 || cat.Models[1].ID != "DeepSeek-V4-Pro" {
		t.Fatalf("models = %+v", cat.Models)
	}
}
