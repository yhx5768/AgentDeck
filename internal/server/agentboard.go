package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// boardChange is the lightweight delta broadcast on the board SSE stream. The
// frontend uses task_id to re-fetch only the affected card (and falls back to a
// full snapshot when it cannot apply a change). Keeping the payload tiny means
// a large board is never copied through every subscriber.
type boardChange struct {
	TaskID   string `json:"task_id,omitempty"`
	Kind     string `json:"kind,omitempty"`
	At       string `json:"at,omitempty"`
	Status   string `json:"status,omitempty"`
	Archived bool   `json:"archived,omitempty"`
	// Seq is a monotonically increasing broadcast sequence shared by every
	// subscriber. The board tab and the Personal Agent pane each open their own
	// SSE connection; both receive the same event, so the browser drops the
	// duplicate by (task_id, seq).
	Seq uint64 `json:"seq,omitempty"`
}

// boardSub coalesces changes per SSE connection. A busy turn can write many
// timeline events per second; the subscriber only needs the latest change per
// task, so pending deltas are replaced by task id (and a full resync supersedes
// all of them).
type boardSub struct {
	notify  chan struct{}
	mu      sync.Mutex
	pending map[string]boardChange
	full    bool
}

func newBoardSub() *boardSub {
	return &boardSub{notify: make(chan struct{}, 1)}
}

func (sub *boardSub) add(ev boardChange) {
	sub.mu.Lock()
	if ev.TaskID == "" || ev.Kind == "resync" {
		sub.full = true
		sub.pending = nil
	} else if !sub.full {
		if sub.pending == nil {
			sub.pending = map[string]boardChange{}
		}
		sub.pending[ev.TaskID] = ev
	}
	sub.mu.Unlock()
	select {
	case sub.notify <- struct{}{}:
	default:
	}
}

// take drains the pending change set. (true, nil) asks the caller to send a
// full-resync marker.
func (sub *boardSub) take() (bool, []boardChange) {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	full, pending := sub.full, sub.pending
	sub.full = false
	sub.pending = nil
	if full || len(pending) == 0 {
		return true, nil
	}
	changes := make([]boardChange, 0, len(pending))
	for _, ev := range pending {
		changes = append(changes, ev)
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].At != changes[j].At {
			return changes[i].At < changes[j].At
		}
		return changes[i].TaskID < changes[j].TaskID
	})
	return false, changes
}

// boardHub fans out "the board changed" notifications to SSE clients.
type boardHub struct {
	mu   sync.Mutex
	subs map[*boardSub]struct{}
	seq  uint64
}

func newBoardHub() *boardHub {
	return &boardHub{subs: map[*boardSub]struct{}{}}
}

func (h *boardHub) subscribe() *boardSub {
	sub := newBoardSub()
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub
}

func (h *boardHub) unsubscribe(sub *boardSub) {
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
}

func (h *boardHub) broadcast(ev boardChange) {
	if ev.At == "" {
		ev.At = time.Now().Format("2006-01-02 15:04:05")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	ev.Seq = h.seq
	for sub := range h.subs {
		sub.add(ev)
	}
}

// handleAgentBoardEvents streams board deltas whenever the SQLite board is
// written. Each event carries the affected task id plus a cheap kind/status
// hint; a full-resync marker is sent when a subscriber missed events or the
// change cannot be represented per task (e.g. a new status column). The
// browser patches only the affected card and falls back to a full snapshot for
// resync markers.
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

	sub := s.boardHub.subscribe()
	defer s.boardHub.unsubscribe(sub)
	fmt.Fprint(w, "retry: 2000\nevent: ready\ndata: {}\n\n")
	flusher.Flush()

	// Keep proxies from closing an idle connection; EventSource reconnects on
	// its own if the stream still dies.
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-sub.notify:
			full, changes := sub.take()
			payload := map[string]any{"changed": true}
			if full {
				payload["full"] = true
			} else {
				payload["changes"] = changes
			}
			data, err := json.Marshal(payload)
			if err != nil {
				payload = map[string]any{"changed": true, "full": true}
				data, _ = json.Marshal(payload)
			}
			fmt.Fprintf(w, "event: board\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}
