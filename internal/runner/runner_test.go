package runner

import (
	"strings"
	"testing"
)

func TestParseClaudeCodeError(t *testing.T) {
	cases := []struct {
		name       string
		stderr     string
		wantKind   string
		wantMsgHas string
	}{
		{
			name:       "unrecognized_model with model",
			stderr:     `[claude-code:unrecognized_model] {"model":"glm-5.3-flash[1m]","query_source":"sdk"}`,
			wantKind:   "unrecognized_model",
			wantMsgHas: "glm-5.3-flash[1m]",
		},
		{
			name:       "unrecognized_model with extra lines ignored",
			stderr:     "some log\n[claude-code:unrecognized_model] {\"model\":\"x\"}\nmore log",
			wantKind:   "unrecognized_model",
			wantMsgHas: "x",
		},
		{
			name:   "plain stderr not recognized",
			stderr: "something went wrong",
			wantKind: "",
		},
		{
			name:   "other claude-code kind not handled",
			stderr: `[claude-code:rate_limit] {"retry_after":10}`,
			wantKind: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseClaudeCodeError(c.stderr)
			if got.Kind != c.wantKind {
				t.Fatalf("Kind = %q, want %q", got.Kind, c.wantKind)
			}
			if c.wantMsgHas != "" && !strings.Contains(got.Message, c.wantMsgHas) {
				t.Fatalf("Message = %q, want it to contain %q", got.Message, c.wantMsgHas)
			}
		})
	}
}
