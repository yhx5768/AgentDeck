package server

import (
	"testing"

	"cc-webui/internal/config"
	"cc-webui/internal/store"
)

func TestAgentSendBlocked(t *testing.T) {
	s, _ := createResidentServer(t)
	mainAgent := &store.Session{ID: "main", MainAgent: true}
	worker := &store.Session{ID: "worker"}
	openCodeMain := &store.Session{ID: "oc-main", MainAgent: true, CLI: "oc"}
	cases := []struct {
		name   string
		sess   *store.Session
		source string
		want   bool
	}{
		{"agent-send to claude mainAgent is blocked", mainAgent, "agent-send", true},
		{"normal user message to mainAgent is allowed", mainAgent, "user", false},
		{"empty source defaults to user and is allowed", mainAgent, "", false},
		{"agent-send to worker is allowed", worker, "agent-send", false},
		{"explicit source is allowed", mainAgent, "agent-send-explicit", false},
		{"a2a source is never blocked", mainAgent, "a2a", false},
		{"nil session is not blocked here", nil, "agent-send", false},
		// A non-claude main agent has no native SendMessage to duplicate, so
		// agentdeck send is its only inbound path and must be allowed.
		{"agent-send to opencode mainAgent is allowed", openCodeMain, "agent-send", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.agentSendBlocked(tc.sess, tc.source); got != tc.want {
				t.Fatalf("agentSendBlocked(%v, %q) = %v, want %v", tc.sess, tc.source, got, tc.want)
			}
		})
	}
}

// TestAgentSendBlockedNonClaudeEngine pins the engine check itself: the opencode
// cli must actually resolve to a non-claude kind, otherwise the case above would
// pass for the wrong reason.
func TestAgentSendBlockedNonClaudeEngine(t *testing.T) {
	s, _ := createResidentServer(t)
	if k := s.engineOf(&store.Session{CLI: "oc"}); k != config.KindOpenCode {
		t.Fatalf("engineOf(oc) = %q, want %q", k, config.KindOpenCode)
	}
}
