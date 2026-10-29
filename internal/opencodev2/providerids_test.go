package opencodev2

import (
	"reflect"
	"testing"
)

func TestParseProviderIDs(t *testing.T) {
	const fixture = `{"data":[
  {"id":"a/1","providerID":"cline-pass","status":"active","enabled":true},
  {"id":"a/2","providerID":"cline-pass","status":"active","enabled":true},
  {"id":"b/1","providerID":"opencode-go","status":"active"},
  {"id":"c/1","providerID":"retired","status":"deprecated","enabled":true},
  {"id":"d/1","providerID":"disabled","status":"active","enabled":false},
  {"id":"e/1","providerID":"","status":"active"}
]}`
	got, err := ParseProviderIDs([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cline-pass", "opencode-go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("providers = %v, want %v", got, want)
	}
}

func TestParseProviderIDsRejectsGarbage(t *testing.T) {
	if _, err := ParseProviderIDs([]byte("not json")); err == nil {
		t.Fatal("expected a parse error")
	}
}
