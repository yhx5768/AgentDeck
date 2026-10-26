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

// ocTurnRegistryV2 is the OpenCode 2 counterpart (separate type because the
// turn driver and its interrupt path differ).
type ocTurnRegistryV2 struct {
	mu    sync.Mutex
	turns map[string]*openCodeTurnV2
}

func newOcTurnRegistryV2() *ocTurnRegistryV2 {
	return &ocTurnRegistryV2{turns: map[string]*openCodeTurnV2{}}
}

func (r *ocTurnRegistryV2) set(sessionID string, t *openCodeTurnV2) {
	r.mu.Lock()
	r.turns[sessionID] = t
	r.mu.Unlock()
}

func (r *ocTurnRegistryV2) get(sessionID string) *openCodeTurnV2 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turns[sessionID]
}

func (r *ocTurnRegistryV2) clear(sessionID string, t *openCodeTurnV2) {
	r.mu.Lock()
	if r.turns[sessionID] == t {
		delete(r.turns, sessionID)
	}
	r.mu.Unlock()
}
