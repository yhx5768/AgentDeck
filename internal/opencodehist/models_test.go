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
  "capabilities": {"input": {"text": true, "image": true, "video": false}},
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
  "capabilities": {"input": {"text": true, "image": false}},
  "variants": {}
}
opencode-go/broken-model
{
  "id": "broken-model",
  "variants": { "oops"
`

// TestParseCatalogVariants: variant keys come back per model, ordered by
// semantic rank (none → minimal → low → … → max → thinking), not
// alphabetically or by insertion order; models with no variants are absent;
// a malformed trailing block must not fail the whole parse.
func TestParseCatalogVariants(t *testing.T) {
	got := parseCatalog([]byte(modelsFixture)).Variants
	want := map[string][]string{
		"opencode-go/deepseek-v4-flash": {"low", "high", "max"},
		"opencode-go/gpt-5.6-luna":      {"none", "low", "medium", "high", "xhigh", "max"},
	}
	if len(got) != 2 {
		t.Fatalf("want 2 models with variants, got %d: %v", len(got), got)
	}
	for m, keys := range want {
		if !reflect.DeepEqual(got[m], keys) {
			t.Errorf("model %s variants = %v, want %v", m, got[m], keys)
		}
	}
}

// TestParseCatalogModels: every parseable model lands in the list in CLI
// order — including ones with no variants, which are still selectable — and a
// block that fails to parse is dropped without taking its neighbours with it.
func TestParseCatalogModels(t *testing.T) {
	cat := parseCatalog([]byte(modelsFixture))
	got := cat.Models
	wantIDs := []string{"deepseek-v4-flash", "gpt-5.6-luna", "plain-model"}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d models (%+v), want %d", len(got), got, len(wantIDs))
	}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Errorf("model[%d].ID = %q, want %q (CLI order must be preserved: the picker treats the first as the default)", i, got[i].ID, id)
		}
	}
	if cat.Find("broken-model") {
		t.Error("the unparseable trailing block must be skipped without taking its neighbours with it")
	}
}

// TestParseCatalogInput: modalities come from capabilities.input.image, and
// "text only" is stated explicitly rather than left as an empty list, which
// would read as "declares nothing" to the picker.
func TestParseCatalogInput(t *testing.T) {
	got := parseCatalog([]byte(modelsFixture)).Models
	if !reflect.DeepEqual(got[0].Input, []string{"text", "image"}) {
		t.Errorf("image model input = %v, want [text image]", got[0].Input)
	}
	if !reflect.DeepEqual(got[2].Input, []string{"text"}) {
		t.Errorf("text-only model input = %v, want [text]", got[2].Input)
	}
	// A model that declares no capabilities at all is still text-answerable.
	if !reflect.DeepEqual(got[1].Input, []string{"text"}) {
		t.Errorf("model with no capabilities input = %v, want [text]", got[1].Input)
	}
}

// TestParseCatalogIDIsNotTheHeaderLine: the model name cc-webui must use is the
// registry's own "id", not the "<providerID>/<id>" line printed above it. They
// diverge whenever the model id already carries its provider prefix, which
// cline-pass does for every model — runner's openCodeModelRef passes the config
// name straight through as ModelRef.ID, so taking the header line here would
// address every cline-pass model wrong.
func TestParseCatalogIDIsNotTheHeaderLine(t *testing.T) {
	const fixture = `cline-pass/cline-pass/deepseek-v4-pro
{
  "id": "cline-pass/deepseek-v4-pro",
  "providerID": "cline-pass",
  "capabilities": {"input": {"text": true, "image": false}}
}
`
	got := parseCatalog([]byte(fixture))
	if len(got.Models) != 1 {
		t.Fatalf("got %d models, want 1", len(got.Models))
	}
	if got.Models[0].ID != "cline-pass/deepseek-v4-pro" {
		t.Errorf("ID = %q, want the id field %q", got.Models[0].ID, "cline-pass/deepseek-v4-pro")
	}
	if !got.Find("cline-pass/deepseek-v4-pro") {
		t.Error("Find must locate the model by its id")
	}
	if got.Find("deepseek-v4-pro") {
		t.Error("Find must not match the bare id — opencode is addressed by the full id")
	}
}

func TestCatalogFindNilSafe(t *testing.T) {
	var c *Catalog
	if c.Find("anything") {
		t.Error("a nil catalog must not claim to know a model")
	}
}

// TestParseCatalogSkipsUnnamedBlock: a block whose id is missing cannot be
// selected by name, so it must not enter the list.
func TestParseCatalogSkipsUnnamedBlock(t *testing.T) {
	const fixture = `opencode-go/nameless
{
  "providerID": "opencode-go"
}
opencode-go/named
{
  "id": "named"
}
`
	got := parseCatalog([]byte(fixture))
	if len(got.Models) != 1 || got.Models[0].ID != "named" {
		t.Fatalf("models = %+v, want just the named one", got.Models)
	}
}
