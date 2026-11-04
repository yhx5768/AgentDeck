package server

import (
	"net/http"
	"strings"

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
	target := s.resolveAddress(body.To)
	if target == nil {
		writeErr(w, http.StatusNotFound, "no session with address: "+body.To)
		return
	}
	from := runner.CleanPeerName(body.From)
	if from == "" {
		from = "agent"
	}
	text := a2a.Envelope(from, body.Message)
	if err := s.mgr.Enqueue(target.ID, runner.Message{
		Text:   text,
		Source: a2a.Source,
		From:   from,
		Body:   body.Message,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"session_id": target.ID,
		"to":         sessionAddress(target),
		"from":       from,
	})
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
