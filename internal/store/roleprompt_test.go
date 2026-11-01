package store

import (
	"testing"
	"time"
)

// TestRolePromptRoundTrip pins persistence of the per-session role prompt: it
// must survive Upsert → Get and a full reopen of db.json.
func TestRolePromptRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now()
	sess := &Session{
		ID:         "s1",
		Name:       "worker",
		CLI:        "claude",
		Provider:   "gw",
		Cwd:        "/tmp",
		RolePrompt: "你是 reviewer，只评审不改代码。",
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := st.Upsert(sess, nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got := st.Get("s1"); got == nil || got.RolePrompt != sess.RolePrompt {
		t.Fatalf("Get after Upsert = %+v", got)
	}

	// The mutate path the PATCH handler uses must update it too.
	if err := st.Upsert(sess, func(cur *Session) { cur.RolePrompt = "第二轮规则" }); err != nil {
		t.Fatalf("upsert mutate: %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := reopened.Get("s1")
	if got == nil {
		t.Fatal("session missing after reopen")
	}
	if got.RolePrompt != "第二轮规则" {
		t.Fatalf("role prompt after reopen = %q", got.RolePrompt)
	}
}
