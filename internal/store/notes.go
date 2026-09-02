package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Note is one item of the global todo pad. Persisted in notes.json, independent
// of sessions, so it survives backend restarts. The frontend owns the list and
// sends the whole array on every change; the server normalizes and persists.
type Note struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Done      bool      `json:"done"`
	UpdatedAt time.Time `json:"updated_at"`
}

// LoadNotes reads the global todo pad. A missing file is not an error — it
// returns an empty (non-nil) slice so callers can render "no notes yet".
func (s *Store) LoadNotes() ([]*Note, error) {
	s.notesMu.Lock()
	defer s.notesMu.Unlock()
	raw, err := os.ReadFile(s.notesPath())
	if err != nil {
		if os.IsNotExist(err) {
			return []*Note{}, nil
		}
		return nil, err
	}
	var notes []*Note
	if err := json.Unmarshal(raw, &notes); err != nil {
		return nil, fmt.Errorf("parse notes.json: %w", err)
	}
	if notes == nil {
		notes = []*Note{}
	}
	return notes, nil
}

// SaveNotes atomically replaces the global todo pad (temp + rename, 0600).
func (s *Store) SaveNotes(notes []*Note) error {
	s.notesMu.Lock()
	defer s.notesMu.Unlock()
	raw, err := json.MarshalIndent(notes, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.notesPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.notesPath())
}

func (s *Store) notesPath() string { return filepath.Join(s.dir, "notes.json") }
