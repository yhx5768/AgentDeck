package opencodever

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Version
	}{
		{"1.18.35", V1},
		{"opencode v2.1.4", V2},
		{"2.1.4\n", V2},
		{"opencode 1.2.3", V1},
		{"", Unknown},
		{"garbage", Unknown},
	}
	for _, c := range cases {
		if got := parse(c.in); got != c.want {
			t.Errorf("parse(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestResolveOverride(t *testing.T) {
	// An explicit override must win without touching the binary (path is a
	// nonexistent file here).
	if Resolve("/nope/opencode", "v2") != V2 {
		t.Error("override v2 not honored")
	}
	if Resolve("/nope/opencode", "1") != V1 {
		t.Error("override 1 not honored")
	}
}
