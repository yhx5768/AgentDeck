package opencodev2

import "testing"

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
