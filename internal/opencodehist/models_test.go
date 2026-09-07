package opencodehist

import (
	"reflect"
	"testing"
)

// modelsFixture mirrors the interleaved layout of `opencode models <provider>
// --verbose` stdout (one bare "providerID/modelID" line, then that model's
// JSON metadata block), cut down from a real 1.18.x dump.
const modelsFixture = `opencode-go/deepseek-v4-flash
{
  "id": "deepseek-v4-flash",
  "providerID": "opencode-go",
  "name": "DeepSeek V4 Flash",
  "api": {"id": "deepseek-v4-flash"},
  "options": {},
  "cost": {"input": 0.22, "output": 0.66},
  "capabilities": {"reasoning": true},
  "variants": {
    "high": {"reasoningEffort": "high"},
    "max": {"reasoningEffort": "max"},
    "low": {"reasoningEffort": "low"}
  }
}
opencode-go/gpt-5.6-luna
{
  "id": "gpt-5.6-luna",
  "variants": {
    "max": {"reasoningEffort": "max"},
    "medium": {"reasoningEffort": "medium"},
    "xhigh": {"reasoningEffort": "xhigh"},
    "none": {"reasoningEffort": "none"},
    "low": {"reasoningEffort": "low"},
    "high": {"reasoningEffort": "high"}
  }
}
opencode-go/plain-model
{
  "id": "plain-model",
  "variants": {}
}
opencode-go/broken-model
{
  "id": "broken-model",
  "variants": { "oops"
`

// TestParseModelsOutput: variant keys come back per model, ordered by
// semantic rank (none → minimal → low → … → max → thinking), not
// alphabetically or by insertion order; models with no variants are absent;
// a malformed trailing block must not fail the whole parse.
func TestParseModelsOutput(t *testing.T) {
	got := parseModelsOutput([]byte(modelsFixture))
	want := map[string][]string{
		"opencode-go/deepseek-v4-flash": {"low", "high", "max"},
		"opencode-go/gpt-5.6-luna":      {"none", "low", "medium", "high", "xhigh", "max"},
	}
	if len(got) != 2 {
		t.Fatalf("want 2 models, got %d: %v", len(got), got)
	}
	for m, keys := range want {
		if !reflect.DeepEqual(got[m], keys) {
			t.Errorf("model %s variants = %v, want %v", m, got[m], keys)
		}
	}
}
