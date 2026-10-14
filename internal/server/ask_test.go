package server

import (
	"encoding/json"
	"testing"
	"time"

	"cc-webui/internal/runner"
)

// The sidebar/toast layer learns about a pending question from sessionView /
// groupView, so the summarizer has to survive both input shapes a prompt can
// arrive in: claude/qoder decode AskUserQuestion input from JSON ([]any), while
// opencode builds the same shape in process ([]map[string]any).
func TestPendingAsksOfBothInputShapes(t *testing.T) {
	var decoded any = map[string]any{}
	if err := json.Unmarshal([]byte(`{"questions":[{"header":"选个方案","question":"用哪种实现？"}]}`), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	decodedInput, ok := decoded.(map[string]any)
	if !ok {
		t.Fatalf("decode gave %T", decoded)
	}
	built := map[string]any{
		"questions": []map[string]any{{"header": "选个方案", "question": "用哪种实现？"}},
	}
	older := time.Now().Add(-30 * time.Second)

	for name, input := range map[string]map[string]any{"decoded": decodedInput, "built": built} {
		got := pendingAsksOf([]*runner.PermissionRequest{
			{ID: "p1", Tool: askToolName, Input: input, CreatedAt: older},
			// Tool prompts must not be reported as questions — the sidebar only
			// promises "待回答" for AskUserQuestion.
			{ID: "p2", Tool: "Bash", Input: input, CreatedAt: older},
			{ID: "p3", Tool: askToolName, Input: nil, CreatedAt: time.Now()},
		})
		if len(got) != 2 {
			t.Fatalf("%s: want 2 asks, got %d (%+v)", name, len(got), got)
		}
		if got[0].ID != "p1" { // oldest first: closest to the 55s timeout
			t.Fatalf("%s: want oldest first, got %+v", name, got)
		}
		if got[0].Question != "用哪种实现？" || got[0].Header != "选个方案" {
			t.Errorf("%s: header/question not carried over: %+v", name, got[0])
		}
		if got[1].Question != "" {
			t.Errorf("%s: malformed input must not fabricate a question: %+v", name, got[1])
		}
	}
}

// An empty pending list still has to marshal as [] — the UI indexes into it
// unguarded, and a nil slice would come out as JSON null.
func TestPendingAsksOfEmptyIsNonNil(t *testing.T) {
	got := pendingAsksOf(nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("want empty non-nil slice, got %#v", got)
	}
}
