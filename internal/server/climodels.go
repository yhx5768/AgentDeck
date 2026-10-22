// Dynamic model lists for launcher CLIs.
//
// Some wrappers own the model catalogue for their gateway: `ducc models` and
// `ducx models` print "Available Models:" followed by a comma-separated list.
// Keeping that list in config.yaml is wrong on both ends — a model added to the
// account needs a config edit, and a removed/renamed one keeps showing up
// (ducc no longer accepts the [1m] aliases its old config listed). A cli can opt
// in with `models_command: ["models"]`; this file runs it, parses the ids and
// merges the input modalities declared in the provider's static list as hints.
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"cc-webui/internal/config"
)

const cliModelsTimeout = 20 * time.Second
const cliModelsTTL = 10 * time.Minute

var cliModelsCache = struct {
	sync.Mutex
	m map[string]cliModelsEntry
}{m: map[string]cliModelsEntry{}}

type cliModelsEntry struct {
	ts     time.Time
	models []config.Model
}

// cliModels runs the cli's models_command and returns the models it lists.
// fresh bypasses the 10-minute memo; input modalities are taken from matching
// entries in the provider's static list when they have one, otherwise text is
// assumed.
func (s *Server) cliModels(cli *config.CLI, prov *config.Provider, fresh bool) ([]config.Model, error) {
	if cli == nil || len(cli.ModelsCommand) == 0 {
		return nil, errors.New("cli 未配置 models_command")
	}
	key := cli.Path + "\x00" + strings.Join(cli.ModelsCommand, "\x00") + "\x00" + prov.Name
	cliModelsCache.Lock()
	defer cliModelsCache.Unlock()
	if e, ok := cliModelsCache.m[key]; ok && !fresh && time.Since(e.ts) < cliModelsTTL {
		return e.models, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), cliModelsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli.Path, cli.ModelsCommand...)
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("读取模型列表失败（%s %s）: %w", cli.Path, strings.Join(cli.ModelsCommand, " "), err)
	}
	ids, err := parseModelList(raw)
	if err != nil {
		return nil, err
	}
	models := make([]config.Model, 0, len(ids))
	for _, id := range ids {
		input := modelInputHint(prov, id)
		models = append(models, config.Model{Name: id, Input: input})
	}
	if len(cliModelsCache.m) > 16 {
		cliModelsCache.m = map[string]cliModelsEntry{}
	}
	cliModelsCache.m[key] = cliModelsEntry{ts: time.Now(), models: models}
	return models, nil
}

// parseModelList reads the launcher's "Available Models:" output. The list is
// one comma-separated line; blank entries and duplicates are dropped. A future
// CLI that prints one id per line (no header) is tolerated too.
func parseModelList(raw []byte) ([]string, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil, errors.New("模型列表为空")
	}
	if i := strings.Index(text, "Available Models:"); i >= 0 {
		// The header and the list are separate lines; TrimSpace drops the
		// leading newline before taking the first line of the list.
		text = strings.TrimSpace(text[i+len("Available Models:"):])
		text = strings.SplitN(text, "\n", 2)[0]
	}
	var ids []string
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == '\n' }) {
		id := strings.TrimSpace(part)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, errors.New("模型列表里没有可用的模型")
	}
	return ids, nil
}

// modelInputHint finds the input modalities the provider's static list
// declared for this model. Wrapper names drift in casing, spacing and suffixes
// ("GLM-5.3-Flash-内部" vs "glm-5.3-flash-internal[1m]"), so both sides are
// normalized before comparing. Unknown models default to text.
func modelInputHint(prov *config.Provider, id string) []string {
	if prov != nil {
		want := normalizeModelName(id)
		for _, m := range prov.Models {
			if len(m.Input) > 0 && normalizeModelName(m.Name) == want {
				return m.Input
			}
		}
	}
	return []string{"text"}
}

func normalizeModelName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, drop := range []string{"[1m]", "内部", "internal"} {
		s = strings.ReplaceAll(s, drop, "")
	}
	for _, ch := range []string{" ", "-", "_", "/", "(", ")"} {
		s = strings.ReplaceAll(s, ch, "")
	}
	return s
}

// handleCLIModels serves the dynamic list for a cli with models_command set.
// The response shape matches /api/providers' models array so the frontend can
// reuse the same dropdown code.
func (s *Server) handleCLIModels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cli := s.cfg().CLIByName(strings.TrimSpace(q.Get("cli")))
	if cli == nil {
		writeErr(w, http.StatusBadRequest, "unknown cli")
		return
	}
	if len(cli.ModelsCommand) == 0 {
		writeErr(w, http.StatusBadRequest, "该 cli 未配置 models_command")
		return
	}
	prov := s.cfg().ProviderByName(strings.TrimSpace(q.Get("provider")))
	if prov == nil {
		writeErr(w, http.StatusBadRequest, "unknown provider")
		return
	}
	if prov.Kind != cli.Kind {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("engine mismatch: cli %q (%s) with provider %q (%s)", cli.Name, cli.Kind, prov.Name, prov.Kind))
		return
	}
	models, err := s.cliModels(cli, prov, q.Get("fresh") == "1")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}
