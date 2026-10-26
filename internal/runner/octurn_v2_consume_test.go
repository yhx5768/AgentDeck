package runner

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"cc-webui/internal/config"
	"cc-webui/internal/opencodev2"
	"cc-webui/internal/store"
)

// TestV2ConsumeMapping feeds a real-shaped v2 event sequence through the turn
// adapter and asserts the canonical blocks the frontend receives: streamed
// text, the full text block, a tool card + result, and the content-less usage
// carry event. It also pins the turn terminal event.
func TestV2ConsumeMapping(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cm := config.NewManager(filepath.Join(dir, "config.yaml"), &config.Config{DataDir: dir})
	m := NewManager(cm, st)
	defer m.Close()

	const sid = "s1"
	ch, unsub := m.Subscribe(sid)
	defer unsub()

	turn := newOpenCodeTurnV2(nil, m, sid)
	turn.ocSessionID = "ses_x"

	events := make(chan opencodev2.Event, 16)
	done := make(chan struct{})
	go func() {
		turn.consume(events)
		close(done)
	}()

	mk := func(typ, data string) opencodev2.Event {
		return opencodev2.Event{Type: typ, Data: json.RawMessage(data)}
	}
	feed := []opencodev2.Event{
		mk("session.text.delta", `{"sessionID":"ses_x","assistantMessageID":"msg_a","ordinal":0,"delta":"Hi"}`),
		mk("session.text.ended", `{"sessionID":"ses_x","assistantMessageID":"msg_a","ordinal":0,"text":"Hi there"}`),
		mk("session.tool.called", `{"sessionID":"ses_x","assistantMessageID":"msg_a","id":"call_1","name":"shell","input":{"command":"echo hi"}}`),
		mk("session.tool.success", `{"sessionID":"ses_x","assistantMessageID":"msg_a","id":"call_1","content":[{"type":"text","text":"hi\n"}]}`),
		mk("session.step.ended", `{"sessionID":"ses_x","assistantMessageID":"msg_a","tokens":{"input":10,"output":2,"reasoning":1,"cache":{"read":5,"write":0}}}`),
		mk("session.execution.succeeded", `{"sessionID":"ses_x"}`),
	}
	for _, ev := range feed {
		events <- ev
	}
	close(events)
	<-done

	var got []map[string]any
	for {
		select {
		case ev := <-ch:
			if ev.Kind != "stream" {
				continue
			}
			var d map[string]any
			_ = json.Unmarshal(ev.Data, &d)
			got = append(got, d)
		default:
			goto collected
		}
	}
collected:
	if len(got) != 5 {
		t.Fatalf("stream events = %d, want 5: %+v", len(got), got)
	}
	if got[0]["type"] != "oc_text_delta" || got[0]["partID"] != "msg_a:text-0" {
		t.Fatalf("delta = %+v", got[0])
	}
	// Full text block.
	msg := got[1]["message"].(map[string]any)
	blk := msg["content"].([]any)[0].(map[string]any)
	if blk["type"] != "text" || blk["text"] != "Hi there" || msg["id"] != "msg_a" {
		t.Fatalf("text block = %+v", got[1])
	}
	// Tool card, promoted to the frontend's Bash view.
	msg = got[2]["message"].(map[string]any)
	blk = msg["content"].([]any)[0].(map[string]any)
	if blk["type"] != "tool_use" || blk["name"] != "Bash" || blk["id"] != "call_1" {
		t.Fatalf("tool_use = %+v", blk)
	}
	// Tool result.
	msg = got[3]["message"].(map[string]any)
	blk = msg["content"].([]any)[0].(map[string]any)
	if blk["type"] != "tool_result" || blk["tool_use_id"] != "call_1" {
		t.Fatalf("tool_result = %+v", blk)
	}
	// Usage-only carry event: no message id, empty content, snake_case usage.
	msg = got[4]["message"].(map[string]any)
	if len(msg["content"].([]any)) != 0 || msg["id"] != nil {
		t.Fatalf("usage carry must be id-less and content-less: %+v", msg)
	}
	usage := msg["usage"].(map[string]any)
	if usage["output_tokens"].(float64) != 2 || usage["reasoning_tokens"].(float64) != 1 {
		t.Fatalf("usage = %+v", usage)
	}
	// The execution terminal event closes the turn.
	select {
	case <-turn.turnCh:
	case <-time.After(time.Second):
		t.Fatal("turn was not marked done by session.execution.succeeded")
	}
}
