package store

import "testing"

func TestWithClaudeSID(t *testing.T) {
	// empty -> non-empty: nothing to archive (a brand-new session's first id).
	s := &Session{ID: "s1"}
	WithClaudeSID("aaa")(s)
	if s.ClaudeSessionID != "aaa" {
		t.Fatalf("got ClaudeSessionID=%q want aaa", s.ClaudeSessionID)
	}
	if len(s.PreviousClaudeSessionIDs) != 0 {
		t.Fatalf("first id should not be archived, got %v", s.PreviousClaudeSessionIDs)
	}

	// non-empty -> different non-empty (restart / continuation refreshes the id):
	// the old id is archived.
	WithClaudeSID("bbb")(s)
	if s.ClaudeSessionID != "bbb" {
		t.Fatalf("got ClaudeSessionID=%q want bbb", s.ClaudeSessionID)
	}
	if len(s.PreviousClaudeSessionIDs) != 1 || s.PreviousClaudeSessionIDs[0] != "aaa" {
		t.Fatalf("old id aaa should be archived once, got %v", s.PreviousClaudeSessionIDs)
	}

	// rewind-to-first clears the id: the current id is archived before clearing,
	// so a stale id left in the emptied transcript filename stays searchable.
	WithClaudeSID("")(s)
	if s.ClaudeSessionID != "" {
		t.Fatalf("got ClaudeSessionID=%q want empty", s.ClaudeSessionID)
	}
	if len(s.PreviousClaudeSessionIDs) != 2 || s.PreviousClaudeSessionIDs[1] != "bbb" {
		t.Fatalf("cleared id bbb should be archived, got %v", s.PreviousClaudeSessionIDs)
	}

	// re-assigning the same id again is a no-op for the archive (dedup).
	WithClaudeSID("aaa")(s)
	WithClaudeSID("bbb")(s)
	if len(s.PreviousClaudeSessionIDs) != 2 {
		t.Fatalf("archive must dedup known ids, got %v", s.PreviousClaudeSessionIDs)
	}
	if s.ClaudeSessionID != "bbb" {
		t.Fatalf("got ClaudeSessionID=%q want bbb", s.ClaudeSessionID)
	}
}
