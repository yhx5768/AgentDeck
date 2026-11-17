package runner

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestV2WaitTurnFallsBackToStatus pins the safety net: when the SSE terminal
// event is lost (v2 drops /api/event subscriptions on a location reload), the
// turn must close from the session's lifecycle timestamps instead of hanging
// until the 12h timeout.
func TestV2WaitTurnFallsBackToStatus(t *testing.T) {
	turn := newOpenCodeTurnV2(nil, nil, "s1")
	turn.ocSessionID = "ses_x"
	turn.promptAt = 1_000
	turn.pollEvery = 5 * time.Millisecond
	turn.probe = func(context.Context) (int64, int64, error) {
		return 1_100, 1_200, nil // this turn started and finished
	}

	done := make(chan struct{})
	go func() {
		turn.waitTurn(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitTurn ignored the finished session status")
	}
}

// TestV2WaitTurnIgnoresStaleIdle guards the other side: an idle timestamp that
// predates this turn's prompt belongs to a previous turn and must not close
// the new one.
func TestV2WaitTurnIgnoresStaleIdle(t *testing.T) {
	turn := newOpenCodeTurnV2(nil, nil, "s1")
	turn.ocSessionID = "ses_x"
	turn.promptAt = 1_000
	turn.pollEvery = 5 * time.Millisecond
	turn.probe = func(context.Context) (int64, int64, error) {
		return 900, 950, nil // previous turn's completion
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		turn.waitTurn(ctx)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("stale idle timestamp ended the turn")
	case <-time.After(120 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitTurn did not return after cancellation")
	}
}

// TestV2WaitTurnSurvivesProbeErrors makes sure a flaky status probe cannot end
// a running turn; only a confirmed idle does.
func TestV2WaitTurnSurvivesProbeErrors(t *testing.T) {
	turn := newOpenCodeTurnV2(nil, nil, "s1")
	turn.ocSessionID = "ses_x"
	turn.promptAt = 1_000
	turn.pollEvery = 5 * time.Millisecond
	turn.probe = func(context.Context) (int64, int64, error) {
		return 0, 0, errors.New("boom")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		turn.waitTurn(ctx)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("probe error ended the turn")
	case <-time.After(120 * time.Millisecond):
	}
	cancel()
	<-done
}
