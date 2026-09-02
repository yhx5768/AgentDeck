package server

import (
	"net/http"
	"strings"
	"time"

	"cc-webui/internal/store"
)

// handleGetNotes returns the global todo pad.
func (s *Server) handleGetNotes(w http.ResponseWriter, r *http.Request) {
	notes, err := s.st.LoadNotes()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, notes)
}

// handlePutNotes replaces the whole todo pad. The frontend is authoritative
// and sends the full list on every change; the server normalizes (trim text,
// fill missing id/updated_at, drop empty items) and persists. Returning the
// kept list keeps the on-disk shape honest for the next LoadNotes.
func (s *Server) handlePutNotes(w http.ResponseWriter, r *http.Request) {
	var notes []*store.Note
	if err := decode(r, &notes); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	now := time.Now()
	kept := make([]*store.Note, 0, len(notes))
	for _, n := range notes {
		if n == nil {
			continue
		}
		n.Text = strings.TrimSpace(n.Text)
		if n.Text == "" {
			continue
		}
		if n.ID == "" {
			n.ID = newID()
		}
		if n.UpdatedAt.IsZero() {
			n.UpdatedAt = now
		}
		kept = append(kept, n)
	}
	if err := s.st.SaveNotes(kept); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, kept)
}
