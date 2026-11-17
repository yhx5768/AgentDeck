package opencodev2

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseServerPassword(t *testing.T) {
	cases := []struct {
		line string
		want string
		ok   bool
	}{
		{"server listening on http://127.0.0.1:4610", "", false},
		{"server password v38HS8bQxZ9-X1ao-f2EeQ6HtyC0Rj2KCyrbJ9yHLQY", "v38HS8bQxZ9-X1ao-f2EeQ6HtyC0Rj2KCyrbJ9yHLQY", true},
		{"… server password abc", "abc", true},
		{"server password ", "", false},
	}
	for _, c := range cases {
		got, ok := parseServerPassword(c.line)
		if ok != c.ok || got != c.want {
			t.Errorf("parseServerPassword(%q) = (%q,%v), want (%q,%v)", c.line, got, ok, c.want, c.ok)
		}
	}
}

// TestGetSessionLifecycle pins the decoding of the lifecycle timestamps the
// turn uses as its terminal-event fallback. If the shape drifts, a dropped
// /api/event subscription would silently stall turns for the full timeout.
func TestGetSessionLifecycle(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/session/ses_x" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if u, p, ok := r.BasicAuth(); !ok || u != "opencode" || p != "pw" {
			t.Errorf("basic auth = (%q,%q,%v)", u, p, ok)
		}
		_, _ = io.WriteString(w, `{"data":{"id":"ses_x","outcome":"succeeded",`+
			`"time":{"created":1,"updated":2,"busy":3,"idle":4}}}`)
	}))
	defer ts.Close()

	s := &Server{baseURL: ts.URL, password: "pw"}
	st, err := s.GetSession(context.Background(), "ses_x")
	if err != nil {
		t.Fatal(err)
	}
	if st.ID != "ses_x" || st.Outcome != "succeeded" {
		t.Fatalf("session = %+v", st)
	}
	if st.Time.Busy != 3 || st.Time.Idle != 4 {
		t.Fatalf("time = %+v", st.Time)
	}
}
