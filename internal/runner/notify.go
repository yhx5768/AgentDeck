package runner

import (
	"time"
)

// Notifier is the runtime's outbound channel to the workspace main agent.
//
// The runner supervises every worker process, so it sees the three failures a
// worker cannot report itself: its turn ended with an error, its process went
// silent (hung), or it parked on a human-only gate (a permission card or an
// AskUserQuestion). Each is handed to the server, which decides whether the
// session is a dispatched worker and routes an A2A message to the main agent
// (see internal/server/incidents.go).
//
// The callbacks are invoked from the runner's goroutines (a turn's emit path,
// the permission registry, or the stall watchdog) and must not block or fail
// the turn they describe: an implementation is expected to do its work on its
// own goroutine. A nil field disables that event.
type Notifier struct {
	// TurnError fires when a turn emits a status error (any engine: CLI
	// missing, model rejected, process crashed).
	TurnError func(sessionID, lastErr string)
	// Stalled fires once per turn when a running turn has produced no event at
	// all for longer than the configured stall timeout.
	Stalled func(sessionID string, silentFor time.Duration)
	// Ask fires when a session registers an outstanding approval/question
	// prompt (permission card or AskUserQuestion, every engine).
	Ask func(sessionID string, pr *PermissionRequest)
}

// SetNotifier installs the notifier. Safe to call while turns are in flight;
// the server calls it once after construction.
func (m *Manager) SetNotifier(n Notifier) {
	m.notifyMu.Lock()
	m.notifier = &n
	m.notifyMu.Unlock()
}

func (m *Manager) notifierFor() *Notifier {
	m.notifyMu.Lock()
	defer m.notifyMu.Unlock()
	return m.notifier
}

// noteActivity records that a session just produced an event, feeding the stall
// watchdog. Called from emit and BroadcastTransient on every path.
func (m *Manager) noteActivity(sessionID string) {
	r := m.rt(sessionID)
	r.mu.Lock()
	r.lastActivity = time.Now()
	r.mu.Unlock()
}

// notifyTurnError forwards a turn error to the notifier. Interrupts are not
// errors (they emit state "interrupted", not "error"), so they never reach
// here; the guard is belt and braces.
func (m *Manager) notifyTurnError(sessionID, lastErr string) {
	n := m.notifierFor()
	if n == nil || n.TurnError == nil || lastErr == "" || lastErr == "interrupted" {
		return
	}
	n.TurnError(sessionID, lastErr)
}

// notifyAsk forwards a newly registered prompt to the notifier.
func (m *Manager) notifyAsk(sessionID string, pr *PermissionRequest) {
	n := m.notifierFor()
	if n == nil || n.Ask == nil || pr == nil {
		return
	}
	n.Ask(sessionID, pr)
}

// StartStallWatch starts the watchdog that reports a running turn which has gone
// silent. Idempotent; safe to call from New even in tests.
func (m *Manager) StartStallWatch() {
	m.watchOnce.Do(func() { go m.stallLoop() })
}

// stallInterval is how often the watchdog scans; the configured timeout is the
// precision that matters, so this only bounds detection latency.
const stallInterval = 30 * time.Second

func (m *Manager) stallLoop() {
	t := time.NewTicker(stallInterval)
	defer t.Stop()
	for range t.C {
		m.scanStalls(time.Now())
	}
}

// scanStalls reports every running turn that has gone silent past the
// configured timeout, latching each once. Split out so tests can drive it
// without waiting on the ticker.
func (m *Manager) scanStalls(now time.Time) []stallHit {
	settings := m.cfg().Notify()
	if !settings.Enabled {
		return nil
	}
	n := m.notifierFor()
	if n == nil || n.Stalled == nil {
		return nil
	}
	var hits []stallHit
	m.mu.Lock()
	for id, r := range m.procs {
		r.mu.Lock()
		// Running = a live turn holds a cancel func (matches Status.Running).
		running := r.working && r.cancel != nil
		silent := now.Sub(r.lastActivity)
		if running && !r.stallNotified && !r.lastActivity.IsZero() && silent >= settings.StallTimeout {
			r.stallNotified = true
			hits = append(hits, stallHit{id: id, silent: silent})
		}
		r.mu.Unlock()
	}
	m.mu.Unlock()
	for _, h := range hits {
		n.Stalled(h.id, h.silent)
	}
	return hits
}

// stallHit is one stalled session found by scanStalls.
type stallHit struct {
	id     string
	silent time.Duration
}
