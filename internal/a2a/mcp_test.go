package a2a

import "testing"

func TestToolsAdvertiseReadSearchAndSendID(t *testing.T) {
	names := map[string]bool{}
	sendID := false
	for _, tool := range Tools {
		names[tool.Name] = true
		if tool.Name == "send_message" {
			props, _ := tool.InputSchema["properties"].(map[string]any)
			_, sendID = props["send_id"]
		}
	}
	for _, want := range []string{"read_session_messages", "search_messages"} {
		if !names[want] {
			t.Fatalf("tool %q is not advertised", want)
		}
		if IsBoardTool(want) {
			t.Fatalf("%q must not be a main-agent-only board tool", want)
		}
	}
	if !sendID {
		t.Fatal("send_message must advertise the send_id idempotency key")
	}
}
