package opencodehist

import (
	"reflect"
	"testing"
)

func TestParseProviderIDs(t *testing.T) {
	const fixture = `cline-pass/cline-pass/deepseek-v4-pro
cline-pass/cline-pass/glm-5.3
opencode-go/glm-5.3
opencode/big-pickle
https://example.com/not-a-provider
error: failed to load /tmp/cache
model-without-provider
`
	got := ParseProviderIDs(fixture)
	want := []string{"cline-pass", "opencode-go", "opencode"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("providers = %v, want %v", got, want)
	}
}
