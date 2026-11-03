package server

import (
	"fmt"
	"net/http"
	"sync"
)

// boardHub fans out "the board changed" notifications to SSE clients. The
// payload is intentionally tiny: the frontend re-fetches /api/agent-board, so
// a large board is not copied through every connection.
type boardHub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func newBoardHub() *boardHub {
	return &boardHub{subs: map[chan struct{}]struct{}{}}
}

func (h *boardHub) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *boardHub) unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *boardHub) broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// handleAgentBoardEvents streams a refresh trigger whenever the SQLite board is
// written. The browser re-fetches /api/agent-board on each event; this keeps
// the SSE payload constant-sized and avoids stale full-board copies.
func (s *Server) handleAgentBoardEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "SSE not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := s.boardHub.subscribe()
	defer s.boardHub.unsubscribe(ch)
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			fmt.Fprint(w, "event: board\ndata: {\"changed\":true}\n\n")
			flusher.Flush()
		}
	}
}
