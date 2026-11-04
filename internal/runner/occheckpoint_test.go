package runner

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"cc-webui/internal/config"
	"cc-webui/internal/store"
)

// Regression: capturing a checkpoint must merge `checkpoint` into the user
// event, not replace the event with it. The event's `images` is a []string,
// which fails a map[string]string unmarshal — the old fallback then rewrote
// the whole payload as `{checkpoint}` and silently dropped text/source/images.
// The user saw their own message come back as a blank bubble plus a duplicate
// (the opencode echo no longer matched the stored text, so dedup failed).
func TestWriteCheckpointKeepsImageMessage(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cm := config.NewManager(filepath.Join(dir, "config.yaml"), &config.Config{DataDir: dir})
	m := NewManager(cm, st)
	defer m.Close()

	const sid = "s1"
	if _, err := st.AppendEvent(sid, "user", map[string]any{
		"text": "这张图什么颜色？", "source": "user", "images": []string{"image.png"},
	}); err != nil {
		t.Fatal(err)
	}
	m.writeCheckpoint(sid, "msg_1")

	events, err := st.Events(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	var d map[string]any
	if err := json.Unmarshal(events[0].Data, &d); err != nil {
		t.Fatal(err)
	}
	if d["checkpoint"] != "msg_1" {
		t.Fatalf("checkpoint = %v", d["checkpoint"])
	}
	if d["text"] != "这张图什么颜色？" || d["source"] != "user" {
		t.Fatalf("text/source dropped: %+v", d)
	}
	imgs, ok := d["images"].([]any)
	if !ok || len(imgs) != 1 || imgs[0] != "image.png" {
		t.Fatalf("images dropped: %+v", d["images"])
	}
}
