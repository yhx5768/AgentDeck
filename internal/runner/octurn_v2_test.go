package runner

import (
	"encoding/json"
	"testing"

	"cc-webui/internal/opencodev2"
)

// Payloads below are copied verbatim from a live OpenCode 2.1.4 event stream,
// so a field rename upstream fails this test instead of silently producing
// empty blocks at runtime.
func TestV2EventDecoding(t *testing.T) {
	mk := func(typ, data string) opencodev2.Event {
		return opencodev2.Event{Type: typ, Data: json.RawMessage(data)}
	}

	t.Run("text delta", func(t *testing.T) {
		ev := mk("session.text.delta", `{"sessionID":"ses_x","assistantMessageID":"msg_a","ordinal":0,"delta":"Hi! 👋"}`)
		var d v2TextDelta
		if err := ev.Decode(&d); err != nil {
			t.Fatal(err)
		}
		if d.AssistantMessageID != "msg_a" || d.Delta != "Hi! 👋" || textPartID(d.AssistantMessageID, d.Ordinal) != "msg_a:text-0" {
			t.Fatalf("delta = %+v", d)
		}
		if ev.SessionID() != "ses_x" {
			t.Fatalf("session = %q", ev.SessionID())
		}
	})

	t.Run("tool called", func(t *testing.T) {
		ev := mk("session.tool.called", `{"sessionID":"ses_x","assistantMessageID":"msg_a","id":"call_1","input":{"command":"echo hello-tool"},"executed":false}`)
		var d v2ToolCalled
		if err := ev.Decode(&d); err != nil {
			t.Fatal(err)
		}
		if d.ID != "call_1" || d.Input["command"] != "echo hello-tool" {
			t.Fatalf("tool called = %+v", d)
		}
	})

	t.Run("tool success", func(t *testing.T) {
		ev := mk("session.tool.success", `{"sessionID":"ses_x","assistantMessageID":"msg_a","id":"call_1","content":[{"type":"text","text":"hello-tool\n"}],"metadata":{"status":"completed","exit":0}}`)
		var d v2ToolDone
		if err := ev.Decode(&d); err != nil {
			t.Fatal(err)
		}
		if len(d.Content) != 1 || d.Content[0].Text != "hello-tool\n" {
			t.Fatalf("tool done = %+v", d)
		}
	})

	t.Run("step ended tokens", func(t *testing.T) {
		ev := mk("session.step.ended", `{"sessionID":"ses_x","assistantMessageID":"msg_a","finish":"tool-calls","cost":0,"tokens":{"input":70,"output":40,"reasoning":75,"cache":{"read":5760,"write":0}}}`)
		var d v2StepEnded
		if err := ev.Decode(&d); err != nil {
			t.Fatal(err)
		}
		if d.Tokens == nil || d.Tokens.Output != 40 || d.Tokens.Reasoning != 75 || d.Tokens.Cache.Read != 5760 {
			t.Fatalf("tokens = %+v", d.Tokens)
		}
	})

	t.Run("permission asked", func(t *testing.T) {
		raw := `{"id":"per_1","sessionID":"ses_x","action":"shell","resources":["echo perm-test"],"save":["echo *"],"source":{"type":"tool","messageID":"msg_a","id":"call_1"}}`
		ev := mk("permission.asked", raw)
		var pr opencodev2.PermissionRequest
		if err := ev.Decode(&pr); err != nil {
			t.Fatal(err)
		}
		if pr.Action != "shell" || pr.Resources[0] != "echo perm-test" || pr.Source.ID != "call_1" {
			t.Fatalf("permission = %+v", pr)
		}
		if ev.SessionID() != "ses_x" || !ev.Control() {
			t.Fatalf("permission routing: sid=%q control=%v", ev.SessionID(), ev.Control())
		}
		in := v2PermissionInput(pr)
		if in["command"] != "echo perm-test" {
			t.Fatalf("card input = %+v", in)
		}
	})

	t.Run("form created", func(t *testing.T) {
		raw := `{"form":{"id":"f1","sessionID":"ses_x","title":"Pick","fields":[
		 {"key":"color","title":"颜色","type":"string","options":[{"value":"red","label":"红"},{"value":"blue","label":"蓝"}]},
		 {"key":"notes","title":"备注","type":"string"}]}}`
		ev := mk("form.created", raw)
		var d struct {
			Form opencodev2.FormInfo `json:"form"`
		}
		if err := ev.Decode(&d); err != nil {
			t.Fatal(err)
		}
		if d.Form.ID != "f1" || len(d.Form.Fields) != 2 {
			t.Fatalf("form = %+v", d.Form)
		}
		if ev.SessionID() != "ses_x" || !ev.Control() {
			t.Fatalf("form routing: sid=%q control=%v", ev.SessionID(), ev.Control())
		}
		qs := formQuestions(d.Form)
		if len(qs) != 2 || qs[0]["question"] != "颜色" || len(qs[0]["options"].([]map[string]any)) != 2 {
			t.Fatalf("questions = %+v", qs)
		}
		ans := formAnswer(d.Form, map[string]string{"颜色": "红", "备注": "hi"})
		if ans["color"] != "red" || ans["notes"] != "hi" {
			t.Fatalf("answer = %+v", ans)
		}
	})
}

func TestV2ToolDisplayName(t *testing.T) {
	if toolDisplayName("shell") != "Bash" {
		t.Fatal("shell must render as Bash")
	}
	if toolDisplayName("read") != "read" {
		t.Fatal("known names pass through")
	}
	if toolDisplayName("") != "tool" {
		t.Fatal("empty name gets a placeholder")
	}
}

func TestV2ErrorText(t *testing.T) {
	if got := v2ErrorText("boom", ""); got != "boom" {
		t.Fatalf("string error = %q", got)
	}
	if got := v2ErrorText(map[string]any{"message": "nope"}, ""); got != "nope" {
		t.Fatalf("object error = %q", got)
	}
	if got := v2ErrorText(nil, ""); got == "" {
		t.Fatal("nil error must still produce a message")
	}
}
