package server

import (
	"encoding/json"
	"testing"
	"time"

	"cc-webui/internal/store"
)

func statusEv(state string) *store.Event {
	return &store.Event{Kind: "status", Ts: time.Now(), Data: json.RawMessage(`{"state":"` + state + `"}`)}
}

func userEv() *store.Event {
	return &store.Event{Kind: "user", Ts: time.Now(), Data: json.RawMessage(`{"text":"hi"}`)}
}

// lastStates extracts the states of the status events a slice ends with
// (after any repair), for compact assertions.
func lastState(evs []*store.Event) string {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind != "status" {
			continue
		}
		var d struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(evs[i].Data, &d)
		return d.State
	}
	return ""
}

func TestCloseStaleTurnStart(t *testing.T) {
	cases := []struct {
		name   string
		events []*store.Event
		// wantClosed: the dangling turn_start gets a closing interrupted event
		// appended; wantLast is the state of the final status event after repair.
		wantAppended bool
		wantLast     string
	}{
		{
			name:         "dangling turn_start at the end gets closed",
			events:       []*store.Event{userEv(), statusEv("turn_start")},
			wantAppended: true,
			wantLast:     "interrupted",
		},
		{
			name:         "dangling turn_start after older completed turns",
			events:       []*store.Event{userEv(), statusEv("turn_start"), statusEv("turn_end"), userEv(), statusEv("turn_start")},
			wantAppended: true,
			wantLast:     "interrupted",
		},
		{
			name:     "log closed by turn_end stays untouched",
			events:   []*store.Event{userEv(), statusEv("turn_start"), statusEv("turn_end")},
			wantLast: "turn_end",
		},
		{
			name:     "log closed by error stays untouched",
			events:   []*store.Event{userEv(), statusEv("turn_start"), statusEv("error")},
			wantLast: "error",
		},
		{
			name:     "log closed by interrupted stays untouched",
			events:   []*store.Event{userEv(), statusEv("turn_start"), statusEv("interrupted")},
			wantLast: "interrupted",
		},
		{
			name:     "user echo without any status stays untouched",
			events:   []*store.Event{userEv()},
			wantLast: "",
		},
		{
			name:   "empty log stays empty",
			events: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := closeStaleTurnStart(c.events)
			if c.events == nil && got == nil {
				return
			}
			if c.wantAppended && len(got) != len(c.events)+1 {
				t.Fatalf("len = %d, want %d (one interrupted appended)", len(got), len(c.events)+1)
			}
			if !c.wantAppended && len(got) != len(c.events) {
				t.Fatalf("len = %d, want %d (unchanged)", len(got), len(c.events))
			}
			if c.wantLast != "" && lastState(got) != c.wantLast {
				t.Fatalf("last status state = %q, want %q", lastState(got), c.wantLast)
			}
			if c.wantLast == "" && lastState(got) != "" {
				t.Fatalf("expected no status events, got trailing %q", lastState(got))
			}
		})
	}
}
