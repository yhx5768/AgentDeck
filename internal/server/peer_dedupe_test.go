package server

import (
	"testing"

	"cc-webui/internal/store"
)

func TestAgentSendBlocked(t *testing.T) {
	mainAgent := &store.Session{ID: "main", MainAgent: true}
	worker := &store.Session{ID: "worker"}
	cases := []struct {
		name   string
		sess   *store.Session
		source string
		want   bool
	}{
		{"agent-send to mainAgent is blocked", mainAgent, "agent-send", true},
		{"normal user message to mainAgent is allowed", mainAgent, "user", false},
		{"empty source defaults to user and is allowed", mainAgent, "", false},
		{"agent-send to worker is allowed", worker, "agent-send", false},
		{"explicit source is allowed", mainAgent, "agent-send-explicit", false},
		{"nil session is not blocked here", nil, "agent-send", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentSendBlocked(tc.sess, tc.source); got != tc.want {
				t.Fatalf("agentSendBlocked(%v, %q) = %v, want %v", tc.sess, tc.source, got, tc.want)
			}
		})
	}
}
