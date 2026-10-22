package opencodeapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Event is one SSE message from /event. opencode names the event type in the
// SSE `event:` field and puts the payload in `data:` as
// {"type":"…","properties":{…}}.
type Event struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// eventPayload is the v1 nested shape. Event.Properties is the event payload
// object itself (verified live: session.status → {"sessionID":…,"status":…},
// message.part.updated → {"sessionID":…,"part":{…}}), so SessionIDOf reads
// the TOP level first. Only the v1 stream wraps its fields in one more
// "properties" object, which is what eventProps models.
type eventPayload struct {
	Type       string     `json:"type"`
	Properties eventProps `json:"properties"`
}

// flatProps reads fields from the event payload's top level.
type flatProps struct {
	Info       json.RawMessage `json:"info"`
	Part       json.RawMessage `json:"part"`
	Error      json.RawMessage `json:"error"`
	Data       json.RawMessage `json:"data"`
	SessionID  string          `json:"sessionID"`
	SessionIDs []string        `json:"sessionIDs"`
	Directory  string          `json:"directory"`
	Timestamp  string          `json:"timestamp"`
	// message.part.delta fields (flat, no `part` object).
	MessageID string `json:"messageID"`
	PartID    string `json:"partID"`
	Field     string `json:"field"`
	Delta     string `json:"delta"`
	// question.asked nests one level deeper than the rest.
	Nested json.RawMessage `json:"properties"`
}

// view returns the payload as the top-level field set, falling back to the
// v1 nested form when the top level carries no sessionID.
func (e Event) view() (flatProps, eventProps) {
	var top flatProps
	_ = json.Unmarshal(e.Properties, &top)
	var nest eventPayload
	_ = json.Unmarshal(e.Properties, &nest)
	return top, nest.Properties
}

type eventProps struct {
	Info  json.RawMessage `json:"info"`
	Part  json.RawMessage `json:"part"`
	Error json.RawMessage `json:"error"`
	// permission.asked and question.asked both carry their object here.
	Data json.RawMessage `json:"data"`
	// question.asked nests one level deeper.
	Nested     json.RawMessage `json:"properties"`
	SessionID  string          `json:"sessionID"`
	SessionIDs []string        `json:"sessionIDs"`
	Directory  string          `json:"directory"`
	Timestamp  string          `json:"timestamp"`
}

// Stream subscribes to /event and calls onEvent for every event whose
// sessionID is in sessions (an empty sessions map means "all sessions").
// onEvent runs on a single goroutine, so handlers need no locking.
//
// The connection is long-lived: opencode keeps it open and only closes it on
// server shutdown. A read error returns; callers decide whether to resubscribe.
func (s *Server) Stream(ctx context.Context, onEvent func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/event", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	// No client timeout: this is a stream that stays open for the whole session.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opencode /event: %s", resp.Status)
	}

	sc := bufio.NewScanner(resp.Body)
	// Part payloads (a big tool result, a long file) can be far larger than
	// bufio's default line cap.
	sc.Buffer(make([]byte, 256*1024), 64*1024*1024)
	var evType string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			// Blank line terminates one SSE message.
			evType = ""
			continue
		case strings.HasPrefix(line, ":"):
			// Comment / keepalive.
			continue
		case strings.HasPrefix(line, "event:"):
			evType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue
			}
			if ev.Type == "" {
				ev.Type = evType
			}
			onEvent(ev)
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// PartEvent is the subset of a `message.part.updated` payload cc-webui needs.
type PartEvent struct {
	MessageID string         `json:"messageID"`
	SessionID string         `json:"sessionID"`
	Part      map[string]any `json:"part"`
}

// Part decodes a message.part.updated event. opencode delivers part updates
// on two different buses: the v1 stream uses `message.part.updated`, while
// the current /event bus uses `session.next.*` (step/text/tool lifecycle).
// Both are handled so the turn renderer does not have to care which one the
// running server speaks.
func (e Event) Part() (PartEvent, bool) {
	switch e.Type {
	case "message.part.updated", "message.part.delta":
	default:
		return PartEvent{}, false
	}
	top, nest := e.view()
	var pe PartEvent
	for _, raw := range []json.RawMessage{top.Part, nest.Part} {
		if len(raw) == 0 {
			continue
		}
		_ = json.Unmarshal(raw, &pe.Part)
		if pe.Part != nil {
			break
		}
	}
	if pe.Part != nil {
		if id, _ := pe.Part["messageID"].(string); id != "" {
			pe.MessageID = id
		}
		if sid, _ := pe.Part["sessionID"].(string); sid != "" {
			pe.SessionID = sid
		}
	}
	return pe, pe.Part != nil
}

// SessionIDOf returns the session an event belongs to. opencode puts it in
// different places depending on the dialect: flat on the session.next bus,
// nested under info on the v1 part stream, and in a sessionIDs list on
// bus-wide notifications.
//
// Note the two levels: `properties` is the whole event payload object, and
// every event on this bus carries sessionID at ITS top level (verified live:
// session.status → {"sessionID":"ses_x","status":{…}}, message.part.updated
// → {"sessionID":…,"part":{…}}). The v1 stream instead nests the same fields
// under properties.info / properties.part, so both are checked — getting this
// wrong silently drops the event, and a dropped session.status idle event
// means a turn that never closes.
func (e Event) SessionIDOf() string {
	top, nest := e.view()
	if top.SessionID != "" {
		return top.SessionID
	}
	if len(top.SessionIDs) > 0 {
		return top.SessionIDs[0]
	}
	if nest.SessionID != "" {
		return nest.SessionID
	}
	if len(nest.SessionIDs) > 0 {
		return nest.SessionIDs[0]
	}
	var info struct {
		SessionID string `json:"sessionID"`
	}
	for _, raw := range []json.RawMessage{top.Info, nest.Info} {
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &info)
			if info.SessionID != "" {
				return info.SessionID
			}
		}
	}
	// The v1 part stream repeats the session on the part itself.
	var part struct {
		SessionID string `json:"sessionID"`
	}
	for _, raw := range []json.RawMessage{top.Part, nest.Part} {
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &part)
			if part.SessionID != "" {
				return part.SessionID
			}
		}
	}
	return ""
}

// MessageIDOf returns the assistant message id an event belongs to.
func (e Event) MessageIDOf() string {
	top, nest := e.view()
	var info struct {
		ID string `json:"id"`
	}
	for _, raw := range []json.RawMessage{top.Info, nest.Info} {
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &info)
			if info.ID != "" {
				return info.ID
			}
		}
	}
	var part struct {
		MessageID string `json:"messageID"`
	}
	for _, raw := range []json.RawMessage{top.Part, nest.Part} {
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &part)
			if part.MessageID != "" {
				return part.MessageID
			}
		}
	}
	return part.MessageID
}

// PermissionAsked decodes a `permission.asked` event.
//
// Live 1.18.35 puts the request fields directly on `properties`
// ({"id":…,"sessionID":…,"permission":…,"patterns":…,"tool":{…}}). Older /
// REST-era shapes wrap the same object under `properties.data`, so both
// spellings are accepted. Getting this wrong is silent: the card never
// opens and opencode's tool waits forever for a reply that never comes.
func (e Event) PermissionAsked() (PermissionRequest, bool) {
	if e.Type != "permission.asked" && e.Type != "permission.updated" {
		return PermissionRequest{}, false
	}
	top, nest := e.view()
	for _, raw := range []json.RawMessage{top.Data, nest.Data, e.Properties} {
		if len(raw) == 0 {
			continue
		}
		var pr PermissionRequest
		if err := json.Unmarshal(raw, &pr); err == nil && pr.ID != "" {
			return pr, true
		}
	}
	return PermissionRequest{}, false
}

// QuestionAsked decodes a `question.asked` event. Like permission, the live
// 1.18.35 event carries the object directly on `properties`; the decoder also
// accepts the `data`-wrapped and one-level-deeper `properties.properties`
// spellings.
func (e Event) QuestionAsked() (QuestionRequest, bool) {
	if e.Type != "question.asked" && e.Type != "question.updated" {
		return QuestionRequest{}, false
	}
	top, nest := e.view()
	for _, raw := range []json.RawMessage{top.Data, nest.Data, top.Nested, nest.Nested, e.Properties} {
		if len(raw) == 0 {
			continue
		}
		var qr QuestionRequest
		if err := json.Unmarshal(raw, &qr); err == nil && qr.ID != "" {
			return qr, true
		}
	}
	return QuestionRequest{}, false
}

// SessionError decodes a `session.error` event.
func (e Event) SessionError() (string, bool) {
	if e.Type != "session.error" && e.Type != "session.next.error" {
		return "", false
	}
	top, nest := e.view()
	errRaw := top.Error
	if len(errRaw) == 0 {
		errRaw = nest.Error
	}
	if len(errRaw) == 0 {
		return "", false
	}
	var errObj struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
			Ref     string `json:"ref"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(errRaw, &errObj); err != nil {
		return "", false
	}
	msg := errObj.Data.Message
	if msg == "" {
		msg = errObj.Message
	}
	if msg == "" {
		msg = errObj.Name
	}
	if msg == "" {
		return "", false
	}
	if errObj.Data.Ref != "" {
		msg += "（ref " + errObj.Data.Ref + "）"
	}
	return msg, true
}

// Hub fans one /event subscription out to per-session listeners. Each session
// gets its own goroutine, so a slow handler on one session cannot stall the
// rest; a session that has no listener simply drops its events (they are not
// durable — the transcript is re-read from opencode's store on reconnect).
type Hub struct {
	srv *Server

	mu   sync.Mutex
	subs map[string]chan Event
}

// NewHub creates a Hub bound to a server.
func NewHub(s *Server) *Hub {
	return &Hub{srv: s, subs: map[string]chan Event{}}
}

// Subscribe registers a listener for one session and returns its channel plus
// an unsubscribe func.
func (h *Hub) Subscribe(sessionID string) (<-chan Event, func()) {
	ch := make(chan Event, 256)
	h.mu.Lock()
	h.subs[sessionID] = ch
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			if h.subs[sessionID] == ch {
				delete(h.subs, sessionID)
			}
			h.mu.Unlock()
			close(ch)
		})
	}
}

// Run keeps one /event subscription alive, reconnecting on read errors until
// ctx is done. Events are dispatched to the matching session's subscriber.
func (h *Hub) Run(ctx context.Context) {
	for ctx.Err() == nil {
		err := h.srv.Stream(ctx, func(ev Event) {
			sid := ev.SessionIDOf()
			if sid == "" {
				return
			}
			h.mu.Lock()
			ch, ok := h.subs[sid]
			h.mu.Unlock()
			if !ok {
				return
			}
			if isControlEvent(ev.Type) {
				// Control events cannot be re-derived from the transcript and
				// opencode waits forever on the ones it raised: a dropped
				// question.asked leaves the turn busy with no card and no 55s
				// timeout (the timeout lives in the card handler). Give them a
				// bounded blocking send instead of the lossy fast path. The
				// cap keeps one stuck session from stalling the whole bus.
				select {
				case ch <- ev:
				case <-ctx.Done():
				case <-time.After(2 * time.Second):
					log.Printf("opencode event hub: dropped %s for %s (subscriber full)", ev.Type, sid)
				}
				return
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
			default:
				// Subscriber is not keeping up: drop rather than stall the
				// whole bus. The turn's authoritative state is still on
				// disk, and a resync can rebuild the view.
			}
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// Back off before resubscribing so a dead server does not spin.
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
}

// isControlEvent reports whether an event carries a state transition or a
// prompt that opencode will wait on. Those must not be silently dropped by
// the lossy per-session fan-out.
func isControlEvent(t string) bool {
	switch t {
	case "permission.asked", "permission.updated",
		"question.asked", "question.updated",
		"session.idle", "session.next.idle", "session.status",
		"session.error", "session.next.error",
		"session.next.step.failed":
		return true
	}
	return false
}

// SubscribeAll is a helper for callers that want every event, not per session.
func (h *Hub) SubscribeAll() (<-chan Event, func()) {
	const all = "__all__"
	ch := make(chan Event, 512)
	h.mu.Lock()
	h.subs[all] = ch
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			if h.subs[all] == ch {
				delete(h.subs, all)
			}
			h.mu.Unlock()
			close(ch)
		})
	}
}

// readAll is a small helper for draining an HTTP body in tests.
func readAll(r io.Reader) []byte {
	b, _ := io.ReadAll(r)
	return b
}

// ---- the session.next.* bus ----
//
// The current /event bus (verified live against 1.18.35) reports turn
// lifecycle as `session.next.*` rather than the older `message.part.updated`
// stream. The events cc-webui actually needs:
//
//	session.next.prompted         a turn was admitted
//	session.next.step.started     a model step began (carries assistantMessageID)
//	session.next.text.started/.ended / .delta   assistant text
//	session.next.step.failed      the step died (carries `error`)
//	session.idle                  the session went idle — the turn boundary
//
// The session.idle / step.failed pair is what replaces the old
// "process exited" signal: with a shared serve process there is no process to
// exit, so the turn must be closed from the event stream.

// NextPart is one normalized lifecycle event of the session.next bus.
type NextPart struct {
	// Kind is the cc-webui-facing name: "text" | "step_failed" | "idle" |
	// "busy" | "prompted" | "" (nothing chat-visible).
	Kind string
	// MessageID is the assistant message this event belongs to.
	MessageID string
	// SessionID is the opencode session.
	SessionID string
	// Text carries text-delta payloads.
	Text string
	// Timestamp is opencode's own ISO-8601 stamp, when present.
	Timestamp string
	// ErrMsg is set for step.failed.
	ErrMsg string
}

// Next decodes a session.next.* / session.idle event. ok is false for
// anything cc-webui does not act on.
func (e Event) Next() (NextPart, bool) {
	top, _ := e.view()
	var n NextPart
	n.SessionID = top.SessionID
	n.Timestamp = top.Timestamp

	// Flat decode of the properties object: the next-bus events keep every
	// field at the top level (sessionID, assistantMessageID, error, …), unlike
	// the v1 events which nest under info/part/data.
	var flat map[string]any
	_ = json.Unmarshal(e.Properties, &flat)
	n.MessageID, _ = flat["assistantMessageID"].(string)
	if n.MessageID == "" {
		n.MessageID, _ = flat["messageID"].(string)
	}

	switch e.Type {
	case "session.next.prompted", "session.next.prompt.admitted":
		n.Kind = "prompted"
		return n, true
	case "session.next.text.delta":
		n.Kind = "text"
		n.Text, _ = flat["text"].(string)
		return n, true
	case "session.next.step.failed":
		n.Kind = "step_failed"
		if errObj, ok := flat["error"]; ok {
			switch e2 := errObj.(type) {
			case map[string]any:
				m, _ := e2["message"].(string)
				if m == "" {
					m, _ = e2["name"].(string)
				}
				n.ErrMsg = m
			case string:
				n.ErrMsg = e2
			}
		}
		if n.ErrMsg == "" {
			n.ErrMsg = "opencode step 失败"
		}
		return n, true
	case "session.idle", "session.next.idle":
		n.Kind = "idle"
		return n, true
	case "session.status":
		// session.status is how the v1 prompt path reports the turn boundary:
		// {"sessionID":…,"status":{"type":"busy"}} then {"type":"idle"}.
		// This is the ONLY completion signal on that path (prompt_async
		// returns 204 immediately), so it must be wired or the turn would
		// never be closed.
		var st struct {
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		}
		if err := json.Unmarshal(e.Properties, &st); err != nil || st.Status.Type == "" {
			return n, false
		}
		switch st.Status.Type {
		case "idle":
			n.Kind = "idle"
		case "busy", "retry":
			n.Kind = "busy"
		default:
			return n, false
		}
		return n, true
	default:
		return NextPart{}, false
	}
}

// MessageRole reports the role of the message an event belongs to
// (message.updated only).
func (e Event) MessageRole() (id string, role string, ok bool) {
	if e.Type != "message.updated" && e.Type != "message.part.updated" {
		return "", "", false
	}
	top, nest := e.view()
	for _, raw := range []json.RawMessage{top.Info, nest.Info} {
		if len(raw) == 0 {
			continue
		}
		var info struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		}
		if err := json.Unmarshal(raw, &info); err != nil {
			continue
		}
		if info.ID != "" && info.Role != "" {
			return info.ID, info.Role, true
		}
	}
	return "", "", false
}

// PartDelta is one incremental update to an in-flight part.
//
// opencode streams text as message.part.delta events carrying only
// {partID, field, delta} — no `part` object at all — and only publishes the
// assembled part in a later message.part.updated. A client that waits for
// updated therefore sees whole paragraphs appear at once, with no token-level
// streaming; a client that only understands `part` sees nothing.
type PartDelta struct {
	SessionID string
	MessageID string
	PartID    string
	Field     string // "text" today; opencode reserves the others
	Delta     string
}

// PartDelta decodes a message.part.delta event.
func (e Event) PartDelta() (PartDelta, bool) {
	if e.Type != "message.part.delta" {
		return PartDelta{}, false
	}
	top, _ := e.view()
	d := PartDelta{
		SessionID: top.SessionID,
		MessageID: top.MessageID,
		PartID:    top.PartID,
		Field:     top.Field,
		Delta:     top.Delta,
	}
	if d.Delta == "" {
		return PartDelta{}, false
	}
	return d, true
}
