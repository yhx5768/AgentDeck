package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Session is the metadata of one managed chat session.
type Session struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	CLI             string `json:"cli"`
	Provider        string `json:"provider"`
	ModelOverride   string `json:"model_override,omitempty"`
	EffortOverride  string `json:"effort_override,omitempty"`
	PermissionMode  string `json:"permission_mode,omitempty"`
	Cwd             string `json:"cwd"`
	ClaudeSessionID string `json:"claude_session_id,omitempty"`
	// PreviousClaudeSessionIDs holds engine session ids this conversation used
	// before they were replaced. rewind-to-first clears ClaudeSessionID and the
	// next turn opens a fresh claude session with a new id; restarts or
	// continuations can also refresh it. The old id is still on disk (a claude
	// transcript filename, a data/backups copy, a peer registry entry), so
	// archiving it keeps the sidebar search able to resolve that stale id back
	// to this session. See WithClaudeSID.
	PreviousClaudeSessionIDs []string `json:"previous_claude_session_ids,omitempty"`
	SlashCommands            []string `json:"slash_commands,omitempty"`
	// Resident keeps a long-lived streaming-input claude process alive for this
	// session so other Claude sessions can reach it via SendMessage (peer
	// messaging). PeerName is the addressable name written into the claude
	// transcript as a custom-title; empty means "derive from Name".
	Resident bool   `json:"resident,omitempty"`
	PeerName string `json:"peer_name,omitempty"`
	// GroupID marks a shadow member session owned by a group chat: it runs
	// the group's turns for one participant and is hidden from the sidebar
	// (openable directly by id from the group view). Empty = ordinary session.
	GroupID  string `json:"group_id,omitempty"`
	Archived bool   `json:"archived"`
	// Unread marks a session whose latest turn just finished (active→idle) but
	// no client has opened it since. Set server-side by the runner on
	// turn_end/error (not on interrupt) and cleared by a PATCH when a client
	// opens it. Server-side so every client agrees: the old localStorage blue
	// dot only marked the tab that saw the turn end, leaving the dot blue on
	// every other client even after one had read it.
	Unread bool `json:"unread"`
	// Todo is a manual "come back later" marker the user sets from the session
	// menu (orange dot) for sessions that run background work they want to
	// revisit. Cleared automatically when the user sends a follow-up message,
	// signalling the revisit happened.
	Todo bool `json:"todo"`
	// Goal is a session-scoped "keep working until this is true" contract. See
	// the Goal type. Nil when the session has none. Copied by Get/List so a
	// caller holding a *Session can never write through to the stored goal.
	Goal      *Goal     `json:"goal,omitempty"`
	SortOrder int       `json:"sort_order"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Goal is a "keep turning until X holds" contract on one session.
//
// After every clean turn the runner asks the model to judge the condition; a
// "not met" verdict queues another turn. MaxTurns and MaxHours are the circuit
// breaker — without them a goal that can never be satisfied would spin forever,
// so an exhausted goal always stops and says why.
type Goal struct {
	// Text is the termination condition, written by the user and judged by the
	// model. Kept verbatim so the runner can quote it back every round.
	Text string `json:"text"`
	// MaxTurns caps continuation rounds; MaxHours caps wall-clock span. Zero
	// means "no limit of that kind".
	MaxTurns int     `json:"max_turns"`
	MaxHours float64 `json:"max_hours"`
	// Rounds counts continuation rounds already spent.
	Rounds int `json:"rounds"`
	// Status: active | met | stopped | exhausted. See the Goal* constants.
	Status string `json:"status"`
	// LastReason carries the unmet reason or the stop cause, for the UI.
	LastReason string    `json:"last_reason,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Goal statuses.
const (
	GoalActive    = "active"    // running; the runner will keep feeding it turns
	GoalMet       = "met"       // the model reported the condition satisfied
	GoalStopped   = "stopped"   // the user stopped it
	GoalExhausted = "exhausted" // circuit breaker fired (rounds or wall clock)
)

// Circuit-breaker defaults, used when a caller sets none.
const (
	DefaultGoalMaxTurns = 20
	DefaultGoalMaxHours = 4.0
)

// Validate fills in the default limits and rejects an empty condition.
func (g *Goal) Validate() error {
	g.Text = strings.TrimSpace(g.Text)
	if g.Text == "" {
		return fmt.Errorf("goal text required")
	}
	if g.MaxTurns <= 0 {
		g.MaxTurns = DefaultGoalMaxTurns
	}
	if g.MaxHours <= 0 {
		g.MaxHours = DefaultGoalMaxHours
	}
	if g.Status == "" {
		g.Status = GoalActive
	}
	return nil
}

// Active reports whether the runner should keep feeding this goal turns.
func (g *Goal) Active() bool { return g != nil && g.Status == GoalActive }

// Exhausted reports whether the circuit breaker has fired for this goal.
func (g *Goal) Exhausted(now time.Time) bool {
	if g == nil {
		return false
	}
	if g.MaxTurns > 0 && g.Rounds >= g.MaxTurns {
		return true
	}
	if g.MaxHours > 0 && !g.CreatedAt.IsZero() &&
		now.After(g.CreatedAt.Add(time.Duration(g.MaxHours*float64(time.Hour)))) {
		return true
	}
	return false
}

// StopReason names the limit that fired, for the UI to show.
func (g *Goal) StopReason(now time.Time) string {
	if g == nil {
		return ""
	}
	if g.MaxTurns > 0 && g.Rounds >= g.MaxTurns {
		return fmt.Sprintf("达到最大轮数 %d", g.MaxTurns)
	}
	if g.MaxHours > 0 && !g.CreatedAt.IsZero() &&
		now.After(g.CreatedAt.Add(time.Duration(g.MaxHours*float64(time.Hour)))) {
		return fmt.Sprintf("超过最长时限 %g 小时", g.MaxHours)
	}
	return ""
}

// cloneGoal deep-copies a goal so stored state can't be aliased out by Get/List.
func cloneGoal(g *Goal) *Goal {
	if g == nil {
		return nil
	}
	cp := *g
	return &cp
}

// WithClaudeSID returns a Store mutator (for Upsert) that swaps the engine
// session id to new, archiving the previous id (if any and distinct) into
// PreviousClaudeSessionIDs so it stays searchable. Use this at every site that
// overwrites ClaudeSessionID instead of a bare `s.ClaudeSessionID = new`:
// rewind-to-first clears the id and the next turn opens a fresh claude session
// with a new id, so without archiving the stale id left in a transcript
// filename or file lock can no longer resolve to this session. An empty new
// (the rewind clear) archives the current id; empty->non-empty is a no-op.
func WithClaudeSID(new string) func(*Session) {
	return func(s *Session) {
		old := s.ClaudeSessionID
		if old != "" && old != new && !slices.Contains(s.PreviousClaudeSessionIDs, old) {
			s.PreviousClaudeSessionIDs = append(s.PreviousClaudeSessionIDs, old)
		}
		s.ClaudeSessionID = new
	}
}

// Event is one line in a session's event log.
// Kind: "user" (data={text}), "stream" (data=raw claude stream-json object),
// "status" (data={state: turn_start|turn_end|interrupted|error, ...}).
type Event struct {
	Seq  int64           `json:"seq"`
	Ts   time.Time       `json:"ts"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type db struct {
	Sessions []*Session `json:"sessions"`
}

// Store persists session metadata (db.json) and per-session event logs.
type Store struct {
	dir string

	mu       sync.Mutex
	sessions map[string]*Session

	eventsMu sync.Mutex
	events   map[string][]*Event // lazily loaded, append-only

	notesMu sync.Mutex // protects notes.json (temp path collision + lost updates)
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "events"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, sessions: map[string]*Session{}, events: map[string][]*Event{}}
	raw, err := os.ReadFile(s.dbPath())
	if err == nil {
		var d db
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, fmt.Errorf("parse db.json: %w", err)
		}
		for _, sess := range d.Sessions {
			s.sessions[sess.ID] = sess
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *Store) dbPath() string { return filepath.Join(s.dir, "db.json") }

func (s *Store) saveLocked() error {
	d := db{}
	for _, sess := range s.sessions {
		d.Sessions = append(d.Sessions, sess)
	}
	sort.Slice(d.Sessions, func(i, j int) bool {
		return d.Sessions[i].UpdatedAt.After(d.Sessions[j].UpdatedAt)
	})
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.dbPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.dbPath())
}

func (s *Store) List() []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		cp := *sess
		cp.Goal = cloneGoal(sess.Goal)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		// Manual sort: stable order by sort_order; fallback to updated_at.
		if out[i].SortOrder != 0 || out[j].SortOrder != 0 {
			if out[i].SortOrder != out[j].SortOrder {
				return out[i].SortOrder > out[j].SortOrder
			}
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

func (s *Store) Get(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		cp := *sess
		cp.Goal = cloneGoal(sess.Goal)
		return &cp
	}
	return nil
}

// Upsert inserts or updates a session. The mutate callback runs under lock.
func (s *Store) Upsert(sess *Session, mutate func(*Session)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.sessions[sess.ID]
	if !ok {
		cur = sess
		s.sessions[sess.ID] = cur
	}
	if mutate != nil {
		mutate(cur)
	}
	return s.saveLocked()
}

// setFlag mutates a single existing session under lock and rewrites db.json.
// It is a no-op when the session no longer exists: unread/todo are UI state
// toggled by the sidebar or set at turn end, never something that should
// resurrect a just-deleted session. Unlike Upsert it does not touch
// UpdatedAt — a flag toggle must not reorder the session list (opening a
// session or marking a todo shouldn't bump it to the top).
func (s *Store) setFlag(id string, mutate func(*Session)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.sessions[id]
	if !ok {
		return nil
	}
	mutate(cur)
	return s.saveLocked()
}

// TouchUpdatedAt bumps the session's UpdatedAt for an existing session and
// rewrites db.json. Unlike Upsert it never resurrects a session that was
// deleted meanwhile (the runner passes a snapshot taken before the delete
// could land). The runner calls it when a prompt is enqueued: the sidebar
// sorts by this field, so without the bump a session keeps its old position
// (sorted by its last reply) for the whole turn right after the user hits
// send.
func (s *Store) TouchUpdatedAt(id string) error {
	return s.setFlag(id, func(cur *Session) { cur.UpdatedAt = time.Now() })
}

// SetUnread sets the unread flag on an existing session. Used by the runner
// when a turn ends so the session shows a blue dot on every client until one
// of them opens it.
func (s *Store) SetUnread(id string, v bool) error {
	return s.setFlag(id, func(cur *Session) { cur.Unread = v })
}

// SetFlags writes the unread and/or todo flags together in one pass. Used by
// the PATCH fast path so a flags-only update is one db.json write. Either
// pointer may be nil to leave that flag untouched.
func (s *Store) SetFlags(id string, unread, todo *bool) error {
	return s.setFlag(id, func(cur *Session) {
		if unread != nil {
			cur.Unread = *unread
		}
		if todo != nil {
			cur.Todo = *todo
		}
	})
}

// SetGoal replaces (or clears, when g is nil) a session's goal under lock and
// rewrites db.json. Like setFlag it leaves UpdatedAt alone: a goal ticking from
// round to round is internal bookkeeping and must not reorder the session list.
// A nil / non-active goal is never resurrected, matching setFlag's reasoning.
func (s *Store) SetGoal(id string, g *Goal) error {
	return s.setFlag(id, func(cur *Session) {
		if g == nil {
			cur.Goal = nil
			return
		}
		cp := *g
		cur.Goal = &cp
	})
}

// MutateGoal applies mutate to the session's current goal under lock and rewrites
// db.json, returning the updated copy. A no-op when the session is gone or has no
// goal — the runner uses it to bump Rounds and flip status, and neither should
// create a goal that was never set.
func (s *Store) MutateGoal(id string, mutate func(*Goal)) (*Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.sessions[id]
	if !ok || cur.Goal == nil {
		return nil, nil
	}
	cp := *cur.Goal
	mutate(&cp)
	cp.UpdatedAt = time.Now()
	cur.Goal = &cp
	return &cp, s.saveLocked()
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	if err := s.saveLocked(); err != nil {
		return err
	}
	s.eventsMu.Lock()
	delete(s.events, id)
	s.eventsMu.Unlock()
	return os.RemoveAll(s.eventPath(id))
}

func (s *Store) eventPath(sessionID string) string {
	return filepath.Join(s.dir, "events", sessionID+".jsonl")
}

// AppendEvent appends an event to the session log, assigning Seq. Thread-safe.
func (s *Store) AppendEvent(sessionID, kind string, data any) (*Event, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	events, err := s.loadEventsLocked(sessionID)
	if err != nil {
		return nil, err
	}
	ev := &Event{Seq: int64(len(events)) + 1, Ts: time.Now(), Kind: kind, Data: raw}
	line, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.eventPath(sessionID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	s.events[sessionID] = append(events, ev)
	return ev, nil
}

// MaxSeq returns the highest seq assigned to any event in the session log.
// Returns 0 if the log is empty/missing.
func (s *Store) MaxSeq(sessionID string) (int64, error) {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	events, err := s.loadEventsLocked(sessionID)
	if err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}
	return events[len(events)-1].Seq, nil
}

// Events returns all events of a session (copy).
func (s *Store) Events(sessionID string) ([]*Event, error) {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	events, err := s.loadEventsLocked(sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]*Event, len(events))
	copy(out, events)
	return out, nil
}

// UpdateEvent applies mutate to the event with the given seq, rewriting the
// whole event log (events are few, so a full rewrite is simple and safe).
// Returns the updated event, or an error if no event has that seq. Used to
// attach a file-checkpoint uuid to a user event after the turn that produced
// it completes.
func (s *Store) UpdateEvent(sessionID string, seq int64, mutate func(*Event)) (*Event, error) {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	events, err := s.loadEventsLocked(sessionID)
	if err != nil {
		return nil, err
	}
	var found *Event
	for _, ev := range events {
		if ev.Seq == seq {
			found = ev
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("event seq %d not found", seq)
	}
	if mutate != nil {
		mutate(found)
	}
	f, err := os.OpenFile(s.eventPath(sessionID), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return nil, err
		}
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	return found, nil
}

// ReplaceEvents overwrites the event log (used by session import / rewind).
func (s *Store) ReplaceEvents(sessionID string, events []*Event) error {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	f, err := os.OpenFile(s.eventPath(sessionID), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	out := make([]*Event, 0, len(events))
	for i, ev := range events {
		ev.Seq = int64(i) + 1
		line, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
		out = append(out, ev)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	s.events[sessionID] = out
	return nil
}

func (s *Store) loadEventsLocked(sessionID string) ([]*Event, error) {
	if events, ok := s.events[sessionID]; ok {
		return events, nil
	}
	f, err := os.Open(s.eventPath(sessionID))
	if os.IsNotExist(err) {
		s.events[sessionID] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var events []*Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err == nil {
			events = append(events, &ev)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	s.events[sessionID] = events
	return events, nil
}
