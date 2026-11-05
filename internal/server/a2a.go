package server

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"cc-webui/internal/a2a"
	"cc-webui/internal/config"
	"cc-webui/internal/runner"
	"cc-webui/internal/store"
)

// sessionAddress returns the address other sessions use to reach sess: the
// explicit peer name, else the cleaned display name. It mirrors runner's
// effectivePeerName so /api/a2a/send and the UI resolve the same address.
func sessionAddress(sess *store.Session) string {
	if sess.PeerName != "" {
		return runner.CleanPeerName(sess.PeerName)
	}
	return runner.CleanPeerName(sess.Name)
}

// resolveAddress finds the session named by a `to` address. It accepts an exact
// session id, or an address matched case-insensitively against peer_name /
// cleaned display name. A trailing "name [ref]" disambiguator is stripped (the
// ref only matters on Claude's native same-name path).
func (s *Server) resolveAddress(to string) *store.Session {
	to = strings.TrimSpace(to)
	if to == "" {
		return nil
	}
	if sess := s.st.Get(to); sess != nil {
		return sess
	}
	key := strings.ToLower(strings.TrimSpace(strings.SplitN(to, "[", 2)[0]))
	if key == "" {
		return nil
	}
	for _, sess := range s.st.List() {
		if sess.GroupID != "" {
			continue // group shadow members are internal, not addressable peers
		}
		if strings.ToLower(sessionAddress(sess)) == key {
			return sess
		}
	}
	return nil
}

// handleA2ASend delivers an agent-to-agent message to a session by address. The
// bus is engine-neutral: the message is queued as a normal turn whose text is
// wrapped in the A2A envelope, and the persisted user event carries
// source=a2a + from + body so the UI renders it as a peer bubble on both the
// live stream and replay.
func (s *Server) handleA2ASend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From    string `json:"from"`
		To      string `json:"to"`
		Message string `json:"message"`
		// SendID is an optional idempotency key: a retry with the same id (from
		// the same sender) returns the first delivery's receipt instead of
		// delivering the message twice.
		SendID string `json:"send_id"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	body.To = strings.TrimSpace(body.To)
	body.Message = strings.TrimSpace(body.Message)
	if body.To == "" || body.Message == "" {
		writeErr(w, http.StatusBadRequest, "to and message are required")
		return
	}
	key := ""
	if sendID := strings.TrimSpace(body.SendID); sendID != "" {
		sender := runner.CleanPeerName(body.From)
		if sender == "" {
			sender = "agent"
		}
		key = sender + "\x00" + sendID
		if receipt, ok := s.sendDedup.lookup(key); ok {
			writeJSON(w, http.StatusOK, dedupFields(receipt))
			return
		}
	}
	target, from, err := s.deliverA2A(body.From, body.To, body.Message)
	if err != nil {
		status := http.StatusInternalServerError
		if target == nil {
			status = http.StatusNotFound
		}
		writeErr(w, status, err.Error())
		return
	}
	receipt := map[string]any{
		"ok":         true,
		"session_id": target.ID,
		"to":         sessionAddress(target),
		"from":       from,
	}
	if key != "" {
		s.sendDedup.remember(key, receipt)
	}
	writeJSON(w, http.StatusOK, receipt)
}

// deliverA2A resolves a `to` address and queues the enveloped message as a peer
// turn on the target session. It is the single delivery path shared by the HTTP
// endpoint, the agentdeck MCP send_message tool and the runtime's worker
// notifications. A nil target (returned alongside the error) means the address
// did not resolve.
func (s *Server) deliverA2A(from, to, message string) (*store.Session, string, error) {
	target := s.resolveAddress(to)
	if target == nil {
		return nil, "", fmt.Errorf("no session with address: %s", to)
	}
	from = runner.CleanPeerName(from)
	if from == "" {
		from = "agent"
	}
	text := a2a.Envelope(from, message)
	if err := s.mgr.Enqueue(target.ID, runner.Message{
		Text:   text,
		Source: a2a.Source,
		From:   from,
		Body:   message,
	}); err != nil {
		return target, from, err
	}
	return target, from, nil
}

// sendDedup is the in-memory idempotency store for send_message's send_id. It
// is deliberately process-local and short-lived: the window only needs to
// outlive a caller's retry, and a restart losing it is acceptable.
type sendDedup struct {
	mu    sync.Mutex
	items map[string]sendReceipt
}

type sendReceipt struct {
	at     time.Time
	fields map[string]any
}

const (
	sendDedupTTL = 10 * time.Minute
	sendDedupCap = 512
)

func (d *sendDedup) lookup(key string) (map[string]any, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rec, ok := d.items[key]
	if !ok || time.Since(rec.at) > sendDedupTTL {
		if ok {
			delete(d.items, key)
		}
		return nil, false
	}
	return rec.fields, true
}

func (d *sendDedup) remember(key string, fields map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.items == nil {
		d.items = map[string]sendReceipt{}
	}
	if len(d.items) >= sendDedupCap {
		cut := time.Now().Add(-sendDedupTTL)
		for k, rec := range d.items {
			if rec.at.Before(cut) {
				delete(d.items, k)
			}
		}
	}
	d.items[key] = sendReceipt{at: time.Now(), fields: fields}
}

// agentSendBlocked guards against the duplicate-notification path: a Claude
// worker that calls agentdeck.py send to a mainAgent session while also using
// the native SendMessage injects both a user turn and a peer bubble. Only
// Claude sessions have that native duplicate — for every other engine agentdeck
// send is the only way to reach a main agent, so it must be allowed.
func (s *Server) agentSendBlocked(sess *store.Session, source string) bool {
	if sess == nil || !sess.MainAgent || source != "agent-send" {
		return false
	}
	return s.engineOf(sess) == config.KindClaude
}
