package server

// opencode 的可用模型由 CLI 自己的注册表决定（`opencode models <provider>
// --verbose`），不写在 config.yaml 里。这里用一个假 CLI 替身钉住三件事：
// 目录真的被用来校验模型、两个端点共用一次调用、过期时先返回旧值再后台刷新。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cc-webui/internal/config"
)

// fakeOpenCodeModels 是替身 CLI 打印的 `opencode models <provider> --verbose`
// 输出。两个模型：image 能力一个有、一个没有；其中一个带 variants。
const fakeOpenCodeModels = `fake/kept-model
{
  "id": "kept-model",
  "providerID": "fake",
  "capabilities": {"input": {"text": true, "image": true}},
  "variants": {"high": {}, "low": {}}
}
fake/vision-model
{
  "id": "vision-model",
  "providerID": "fake",
  "capabilities": {"input": {"text": true, "image": false}}
}
`

// fakeOpenCodeCLI writes a stand-in for the opencode binary that prints
// output and appends its argv to a log file, so a test can count how many
// times the server shelled out.
func fakeOpenCodeCLI(t *testing.T, output string) (path string, calls func() []string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "opencode")
	logPath := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + logPath + "\ncat <<'EOF'\n" + output + "EOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}
	var mu sync.Mutex
	return path, func() []string {
		mu.Lock()
		defer mu.Unlock()
		b, err := os.ReadFile(logPath)
		if err != nil {
			return nil
		}
		var out []string
		start := 0
		for i, c := range b {
			if c == '\n' {
				out = append(out, string(b[start:i]))
				start = i + 1
			}
		}
		return out
	}
}

func openCodeCatalogServer(t *testing.T, cliPath string) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		DataDir: dir,
		CLIs: []config.CLI{
			{Name: "oc", Kind: config.KindOpenCode, Path: cliPath},
			{Name: "claude", Path: "claude"},
		},
		Providers: []config.Provider{
			{Name: "fake", Kind: config.KindOpenCode, Models: []config.Model{}},
			// A legacy provider that still declares models in config: that list
			// must keep working unchanged while configs are migrated.
			{Name: "legacy", Kind: config.KindOpenCode, Models: []config.Model{{Name: "pinned-model"}}},
			{Name: "gw", Models: []config.Model{{Name: "model-a"}}},
		},
	}
	cm := config.NewManager(filepath.Join(dir, "config.yaml"), cfg)
	return &Server{
		cm:           cm,
		ocCatalog:    map[string]ocCatalogEntry{},
		ocRefreshing: map[string]bool{},
	}
}

func TestModelKnownUsesOpenCodeCatalog(t *testing.T) {
	cliPath, _ := fakeOpenCodeCLI(t, fakeOpenCodeModels)
	s := openCodeCatalogServer(t, cliPath)
	cli := s.cfg().CLIByName("oc")
	prov := s.cfg().ProviderByName("fake")

	if !s.modelKnown(cli, prov, "kept-model") {
		t.Error("a model the CLI registry declares must be selectable")
	}
	if !s.modelKnown(cli, prov, "vision-model") {
		t.Error("a model without variants is still selectable — only the effort list is empty")
	}
	if s.modelKnown(cli, prov, "ghost-model") {
		t.Error("the catalog was readable, so an unknown id must be rejected")
	}
}

func TestModelKnownOpenCodeSkipsCheckWhenCatalogUnreadable(t *testing.T) {
	// No binary at all: the catalog cannot be read, so the check must be
	// skipped rather than refusing the session — the CLI reports a bad model
	// itself on turn one, which is a better failure than no session.
	s := openCodeCatalogServer(t, filepath.Join(t.TempDir(), "nope"))
	cli := s.cfg().CLIByName("oc")
	prov := s.cfg().ProviderByName("fake")
	if !s.modelKnown(cli, prov, "whatever") {
		t.Error("an unreadable catalog must skip the check, not reject every model")
	}
}

func TestModelKnownConfigListStillWins(t *testing.T) {
	cliPath, _ := fakeOpenCodeCLI(t, fakeOpenCodeModels)
	s := openCodeCatalogServer(t, cliPath)
	cli := s.cfg().CLIByName("oc")
	prov := s.cfg().ProviderByName("legacy")

	// Config membership is consulted first, so a model the config pins is
	// accepted even though this provider's registry does not list it — an
	// existing config keeps working until it is emptied on purpose.
	if !s.modelKnown(cli, prov, "pinned-model") {
		t.Error("a config-declared model must stay valid during migration")
	}
	// ...and a name neither the config nor the registry knows is still refused.
	if s.modelKnown(cli, prov, "not-in-registry") {
		t.Error("an unknown name must not slip through the migration path")
	}
}

func TestOpenCodeEndpointsShareOneCLICall(t *testing.T) {
	cliPath, calls := fakeOpenCodeCLI(t, fakeOpenCodeModels)
	s := openCodeCatalogServer(t, cliPath)

	for i := 0; i < 3; i++ {
		if cat, err := s.opencodeCatalog(s.cfg().CLIByName("oc"), s.cfg().ProviderByName("fake"), false); cat == nil || err != nil {
			t.Fatalf("catalog must be readable: %v", err)
		}
	}
	got := calls()
	if len(got) != 1 {
		t.Fatalf("CLI ran %d times, want 1 for 3 reads of one memo: %v", len(got), got)
	}
	// The hot path must not pay for a models.dev round trip.
	if got[0] != "models fake --verbose" {
		t.Errorf("argv = %q, want the cached read without --refresh", got[0])
	}
}

func TestOpenCodeStaleServesNowAndRefreshesBehind(t *testing.T) {
	cliPath, calls := fakeOpenCodeCLI(t, fakeOpenCodeModels)
	s := openCodeCatalogServer(t, cliPath)
	cli, prov := s.cfg().CLIByName("oc"), s.cfg().ProviderByName("fake")

	if _, err := s.opencodeCatalog(cli, prov, false); err != nil {
		t.Fatalf("cold fetch: %v", err)
	}
	// Age the entry past the TTL, as if it had been sitting there.
	key := cli.Name + "\x00" + prov.Name
	s.ocCatalogMu.Lock()
	e := s.ocCatalog[key]
	e.ts = time.Now().Add(-2 * variantsTTL)
	s.ocCatalog[key] = e
	s.ocCatalogMu.Unlock()

	start := time.Now()
	cat, err := s.opencodeCatalog(cli, prov, false)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("stale read must still succeed, got %v", err)
	}
	if len(cat.Models) != 2 {
		t.Errorf("stale read returned %d models, want the memoized 2", len(cat.Models))
	}
	if elapsed > variantsTTL/10 {
		t.Errorf("stale read blocked for %v; it must return the memo and refresh behind it", elapsed)
	}

	// The background refresh lands for the next reader, and it is the only
	// call that asks models.dev.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.ocCatalogMu.Lock()
		got := s.ocCatalog[key]
		s.ocCatalogMu.Unlock()
		if time.Since(got.ts) < time.Second {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := calls()
	if len(got) != 2 {
		t.Fatalf("CLI ran %d times, want 2 (cold read + background refresh): %v", len(got), got)
	}
	if got[1] != "models fake --verbose --refresh" {
		t.Errorf("refresh argv = %q, want the --refresh form", got[1])
	}
}

func TestOpenCodeRefreshFailureKeepsPreviousCatalog(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "opencode")
	// Prints the models only when --refresh is absent, so the background
	// refresh fails and the serving path must stay on the old value.
	script := "#!/bin/sh\ncase \"$*\" in *--refresh*) exit 1;; esac\ncat <<'EOF'\n" + fakeOpenCodeModels + "EOF\n"
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}
	s := openCodeCatalogServer(t, cliPath)
	cli, prov := s.cfg().CLIByName("oc"), s.cfg().ProviderByName("fake")
	if _, err := s.opencodeCatalog(cli, prov, false); err != nil {
		t.Fatalf("cold fetch: %v", err)
	}
	key := cli.Name + "\x00" + prov.Name
	s.ocCatalogMu.Lock()
	e := s.ocCatalog[key]
	e.ts = time.Now().Add(-2 * variantsTTL)
	s.ocCatalog[key] = e
	s.ocCatalogMu.Unlock()

	if _, err := s.opencodeCatalog(cli, prov, false); err != nil {
		t.Fatalf("stale read: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // let the failing refresh finish
	cat, err := s.opencodeCatalog(cli, prov, false)
	if err != nil {
		t.Fatalf("a failed refresh must not poison the catalog: %v", err)
	}
	if len(cat.Models) != 2 {
		t.Errorf("models = %d, want the previous 2 to survive a failed refresh", len(cat.Models))
	}
}

// getOpenCodeModels calls the handler directly, the way the other handler
// tests in this package do: the query string is all these tests vary, and
// routing and auth are not what is under test.
func getOpenCodeModels(t *testing.T, s *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/opencode-models?"+query, nil)
	w := httptest.NewRecorder()
	s.handleOpenCodeModels(w, req)
	return w
}

func TestHandleOpenCodeModelsShape(t *testing.T) {
	cliPath, _ := fakeOpenCodeCLI(t, fakeOpenCodeModels)
	s := openCodeCatalogServer(t, cliPath)

	w := getOpenCodeModels(t, s, "cli=oc&provider=fake")
	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got struct {
		Models []config.Model `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Models) != 2 {
		t.Fatalf("models = %+v, want 2", got.Models)
	}
	if got.Models[0].Name != "kept-model" || got.Models[1].Name != "vision-model" {
		t.Errorf("names = %q, %q — CLI order must be preserved", got.Models[0].Name, got.Models[1].Name)
	}
	if len(got.Models[0].Input) != 2 || got.Models[0].Input[1] != "image" {
		t.Errorf("image model input = %v, want [text image]", got.Models[0].Input)
	}
	if len(got.Models[1].Input) != 1 || got.Models[1].Input[0] != "text" {
		t.Errorf("text-only model input = %v, want [text]", got.Models[1].Input)
	}
}

func TestHandleOpenCodeModelsRejectsWrongKind(t *testing.T) {
	cliPath, _ := fakeOpenCodeCLI(t, fakeOpenCodeModels)
	s := openCodeCatalogServer(t, cliPath)

	for _, q := range []string{
		"cli=claude&provider=gw", // cli is not opencode
		"cli=oc&provider=gw",     // provider is not opencode
		"cli=oc&provider=nope",   // no such provider
		"provider=fake",          // no cli at all
	} {
		w := getOpenCodeModels(t, s, q)
		if w.Code != 400 {
			t.Errorf("%s: status = %d, want 400 (a wrong cli/provider is the caller's mistake, not a server failure)", q, w.Code)
		}
	}
}
