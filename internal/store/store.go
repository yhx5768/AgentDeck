package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Session is the metadata of one managed chat session.
type Session struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	CLI             string    `json:"cli"`
	Provider        string    `json:"provider"`
	ModelOverride   string    `json:"model_override,omitempty"`
	EffortOverride  string    `json:"effort_override,omitempty"`
	PermissionMode  string    `json:"permission_mode,omitempty"`
	Cwd             string    `json:"cwd"`
	ClaudeSessionID string    `json:"claude_session_id,omitempty"`
	Archived        bool      `json:"archived"`
	SortOrder       int       `json:"sort_order"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
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

// ReplaceEvents overwrites the event log (used by session import).
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
