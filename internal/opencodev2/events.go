package opencodev2

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Event is one `/api/event` frame. `data` is the event-specific payload.
type Event struct {
	ID       string          `json:"id"`
	Created  int64           `json:"created"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
	Durable *struct {
		AggregateID string `json:"aggregateID"`
		Seq         int64  `json:"seq"`
	} `json:"durable"`
}

// SessionID returns the session an event belongs to, or "". v2 puts it at
// data.sessionID for session.* events and at data.form.sessionID for
// form.created.
func (e Event) SessionID() string {
	var d struct {
		SessionID string `json:"sessionID"`
		Form      *struct {
			SessionID string `json:"sessionID"`
		} `json:"form"`
	}
	if err := json.Unmarshal(e.Data, &d); err != nil {
		return ""
	}
	if d.SessionID != "" {
		return d.SessionID
	}
	if d.Form != nil {
		return d.Form.SessionID
	}
	return ""
}

// Decode unmarshals the event payload.
func (e Event) Decode(v any) error { return json.Unmarshal(e.Data, v) }

// Control reports events the turn must not lose to a full buffer: losing a
// permission/form event means no card and a turn stuck forever, and losing the
// execution terminal event means the turn never closes.
func (e Event) Control() bool {
	switch e.Type {
	case "permission.asked", "form.created",
		"session.execution.succeeded", "session.execution.failed",
		"session.execution.interrupted", "session.error":
		return true
	}
	return false
}

// Stream subscribes to /api/event and calls onEvent for every frame until the
// context is cancelled or the connection ends.
func (s *Server) Stream(ctx context.Context, onEvent func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/api/event", nil)
	if err != nil {
		return err
	}
	if s.password != "" {
		req.SetBasicAuth("opencode", s.password)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opencode /api/event: %s", resp.Status)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 256*1024), 16*1024*1024)
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(data) > 0 {
				if ev, ok := parseEvent(strings.Join(data, "\n")); ok {
					onEvent(ev)
				}
				data = data[:0]
			}
		case strings.HasPrefix(line, ":"):
			// comment / heartbeat
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(data) > 0 {
		if ev, ok := parseEvent(strings.Join(data, "\n")); ok {
			onEvent(ev)
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func parseEvent(raw string) (Event, bool) {
	var ev Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil || ev.Type == "" {
		return Event{}, false
	}
	return ev, true
}
