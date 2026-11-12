package runner

import (
	"encoding/json"
	"testing"
	"time"
)

func boardAssistantEvent(id string, blocks ...map[string]any) map[string]any {
	content := make([]any, 0, len(blocks))
	for _, block := range blocks {
		content = append(content, block)
	}
	return map[string]any{
		"type":    "assistant",
		"message": map[string]any{"id": id, "role": "assistant", "content": content},
	}
}

func textBlock(id, text string) map[string]any {
	block := map[string]any{"type": "text", "text": text}
	if id != "" {
		block["id"] = id
	}
	return block
}

func newCaptureManager(t *testing.T, fn func(TurnFinal)) *Manager {
	t.Helper()
	m := &Manager{procs: map[string]*sessionRuntime{}}
	m.SetTurnObserver(fn)
	return m
}

func waitFinal(t *testing.T, ch <-chan TurnFinal) TurnFinal {
	t.Helper()
	select {
	case final := <-ch:
		return final
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TurnFinal")
	}
	return TurnFinal{}
}

func TestBoardTurnCaptureFinalAssistantMessage(t *testing.T) {
	ch := make(chan TurnFinal, 1)
	m := newCaptureManager(t, func(final TurnFinal) { ch <- final })
	m.beginBoardTurn("s1", Message{BoardTaskID: "T-1", userSeq: 7})

	m.noteBoardOutput("s1", boardAssistantEvent("m1", textBlock("", "先跑测试")))
	m.noteBoardOutput("s1", boardAssistantEvent("m2",
		textBlock("", "## 结论\n"),
		map[string]any{"type": "tool_use", "id": "t1", "name": "Bash"},
		textBlock("", "**完成**"),
	))
	// A trailing tool-use-only message must not hide the final text.
	m.noteBoardOutput("s1", boardAssistantEvent("m3", map[string]any{"type": "tool_use", "id": "t2"}))
	m.finishBoardTurn("s1", "")

	final := waitFinal(t, ch)
	if final.BoardTaskID != "T-1" || final.SessionID != "s1" || final.UserSeq != 7 {
		t.Fatalf("final binding = %+v", final)
	}
	if final.TurnID != "s1#7" {
		t.Fatalf("turn id = %q", final.TurnID)
	}
	if final.Text != "## 结论\n**完成**" {
		t.Fatalf("final text = %q", final.Text)
	}
}

func TestBoardTurnCaptureReplacesGrowingOpencodePart(t *testing.T) {
	ch := make(chan TurnFinal, 1)
	m := newCaptureManager(t, func(final TurnFinal) { ch <- final })
	m.beginBoardTurn("s1", Message{BoardTaskID: "T-1", userSeq: 1})

	// opencode re-announces the same text part as it grows; the part id is the
	// dedupe key, so the reply is the last snapshot, not the sum of snapshots.
	m.noteBoardOutput("s1", boardAssistantEvent("msg_1", textBlock("part_1", "你")))
	m.noteBoardOutput("s1", boardAssistantEvent("msg_1", textBlock("part_1", "你好")))
	m.noteBoardOutput("s1", boardAssistantEvent("msg_1", textBlock("part_2", "，世界")))
	m.finishBoardTurn("s1", "")

	if final := waitFinal(t, ch); final.Text != "你好，世界" {
		t.Fatalf("final text = %q", final.Text)
	}
}

func TestBoardTurnCaptureResultEventFallback(t *testing.T) {
	ch := make(chan TurnFinal, 1)
	m := newCaptureManager(t, func(final TurnFinal) { ch <- final })
	m.beginBoardTurn("s1", Message{BoardTaskID: "T-1", userSeq: 6})
	m.noteBoardOutput("s1", map[string]any{"type": "result", "subtype": "success", "result": "结果文本"})
	m.finishBoardTurn("s1", "")
	if final := waitFinal(t, ch); final.Text != "结果文本" {
		t.Fatalf("result fallback = %q", final.Text)
	}
}

func TestBoardTurnCaptureIgnoresDeltasAndRawStream(t *testing.T) {
	ch := make(chan TurnFinal, 1)
	m := newCaptureManager(t, func(final TurnFinal) { ch <- final })
	m.beginBoardTurn("s1", Message{BoardTaskID: "T-1", userSeq: 2})

	m.noteBoardOutput("s1", map[string]any{"type": "oc_text_delta", "delta": "半截"})
	raw, _ := json.Marshal(boardAssistantEvent("m1", textBlock("", "完整回复")))
	m.noteBoardOutput("s1", json.RawMessage(raw))
	m.finishBoardTurn("s1", "")

	if final := waitFinal(t, ch); final.Text != "完整回复" {
		t.Fatalf("final text = %q", final.Text)
	}
}

func TestBoardTurnCaptureNonBoardTurnDoesNotEmit(t *testing.T) {
	ch := make(chan TurnFinal, 1)
	m := newCaptureManager(t, func(final TurnFinal) { ch <- final })
	m.beginBoardTurn("s1", Message{userSeq: 3})
	m.noteBoardOutput("s1", boardAssistantEvent("m1", textBlock("", "普通回复")))
	m.finishBoardTurn("s1", "")

	select {
	case final := <-ch:
		t.Fatalf("non-board turn emitted %+v", final)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBoardTurnCaptureErrorTurnDropsPartialText(t *testing.T) {
	ch := make(chan TurnFinal, 1)
	m := newCaptureManager(t, func(final TurnFinal) { ch <- final })
	m.beginBoardTurn("s1", Message{BoardTaskID: "T-1", userSeq: 4})
	m.noteBoardOutput("s1", boardAssistantEvent("m1", textBlock("", "只写了一半")))
	m.finishBoardTurn("s1", "process crashed")

	select {
	case final := <-ch:
		t.Fatalf("error turn emitted %+v", final)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBoardTurnCaptureEmptyReplyDoesNotEmit(t *testing.T) {
	ch := make(chan TurnFinal, 1)
	m := newCaptureManager(t, func(final TurnFinal) { ch <- final })
	m.beginBoardTurn("s1", Message{BoardTaskID: "T-1", userSeq: 5})
	m.noteBoardOutput("s1", boardAssistantEvent("m1", textBlock("", "   ")))
	m.finishBoardTurn("s1", "")

	select {
	case final := <-ch:
		t.Fatalf("empty reply emitted %+v", final)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBoardTurnCaptureClearedForNextTurn(t *testing.T) {
	ch := make(chan TurnFinal, 1)
	m := newCaptureManager(t, func(final TurnFinal) { ch <- final })
	m.beginBoardTurn("s1", Message{BoardTaskID: "T-1", userSeq: 1})
	m.noteBoardOutput("s1", boardAssistantEvent("m1", textBlock("", "第一轮")))
	m.finishBoardTurn("s1", "")
	waitFinal(t, ch)

	// The next turn is ordinary: it must not inherit the previous capture.
	m.beginBoardTurn("s1", Message{userSeq: 2})
	m.noteBoardOutput("s1", boardAssistantEvent("m2", textBlock("", "第二轮")))
	m.finishBoardTurn("s1", "")
	select {
	case final := <-ch:
		t.Fatalf("ordinary turn reused board capture: %+v", final)
	case <-time.After(100 * time.Millisecond):
	}
}
