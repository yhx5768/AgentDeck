package runner

import "sync"

// ocTurnRegistry tracks the live opencode turn of each session so Stop() can
// reach it (an opencode turn runs inside a shared `opencode serve` process,
// not a per-turn child, so there is no process to kill — the turn has to be
// interrupted over HTTP).
type ocTurnRegistry struct {
	mu    sync.Mutex
	turns map[string]*openCodeTurn
}

func newOcTurnRegistry() *ocTurnRegistry {
	return &ocTurnRegistry{turns: map[string]*openCodeTurn{}}
}

func (r *ocTurnRegistry) set(sessionID string, t *openCodeTurn) {
	r.mu.Lock()
	r.turns[sessionID] = t
	r.mu.Unlock()
}

func (r *ocTurnRegistry) get(sessionID string) *openCodeTurn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turns[sessionID]
}

func (r *ocTurnRegistry) clear(sessionID string, t *openCodeTurn) {
	r.mu.Lock()
	// Only clear our own entry: a queued next turn may already have replaced it.
	if r.turns[sessionID] == t {
		delete(r.turns, sessionID)
	}
	r.mu.Unlock()
}
