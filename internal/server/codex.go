// Codex CLI HTTP surface: model catalog, session import source and preview.
//
// Codex keeps its sessions as rollout JSONL files under $CODEX_HOME/sessions
// (see codexhist). Baidu's ducx wrapper is the same protocol but keeps its home
// in ~/.baidu-cx and ignores CODEX_HOME, so the home is resolved per cli
// (cli.home, then the wrapper's known directory, then ~/.codex).
package server

import (
	"net/http"
	"path/filepath"
	"strings"

	"cc-webui/internal/codexhist"
	"cc-webui/internal/config"
)

// codexHome resolves the home directory of a codex-kind cli.
func codexHome(cli *config.CLI) string {
	if cli == nil {
		return codexhist.ResolveHome("", "codex")
	}
	return codexhist.ResolveHome(cli.Home, cli.Path)
}

// handleCodexModels reports the model catalog the CLI ships (`debug models
// --bundled`). The list, its reasoning levels and input modalities all change
// with CLI updates and account tiers, so the dropdowns read it from the CLI
// instead of config. ?fresh=1 bypasses the 10-minute memo.
func (s *Server) handleCodexModels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cli := s.cfg().CLIByName(strings.TrimSpace(q.Get("cli")))
	if cli == nil || !cli.IsCodex() {
		writeErr(w, http.StatusBadRequest, "a codex kind cli is required")
		return
	}
	prov := s.cfg().ProviderByName(strings.TrimSpace(q.Get("provider")))
	if prov == nil || !prov.IsCodex() {
		writeErr(w, http.StatusBadRequest, "a codex kind provider is required")
		return
	}
	cat, err := codexhist.LoadCatalog(cli.Path, q.Get("fresh") == "1")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cat)
}

// handleCodexSessions lists the rollouts under the cli's codex home, the import
// source for the codex tab. The rollout records each session's cwd, so cwd is
// a filter here (unlike mcode).
func (s *Server) handleCodexSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cli := s.cfg().CLIByName(strings.TrimSpace(q.Get("cli")))
	if cli == nil || !cli.IsCodex() {
		writeErr(w, http.StatusBadRequest, "a codex kind cli is required")
		return
	}
	prefix := strings.ToLower(strings.TrimSpace(q.Get("prefix")))
	cwd := strings.TrimSpace(q.Get("cwd"))
	want := ""
	if cwd != "" {
		want = filepath.Clean(cwd)
	}
	list, err := codexhist.List(codexHome(cli))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	filtered := list[:0]
	for _, item := range list {
		if prefix != "" && !strings.HasPrefix(strings.ToLower(item.SessionID), prefix) {
			continue
		}
		if want != "" && item.Cwd != "" && !inDir(item.Cwd, want) {
			continue
		}
		filtered = append(filtered, item)
	}
	list = filtered
	if list == nil {
		list = []codexhist.SessionInfo{}
	}
	if len(list) > 200 {
		list = list[:200]
	}
	writeJSON(w, http.StatusOK, list)
}

func inDir(path, dir string) bool {
	p := filepath.Clean(path)
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// handleCodexSessionPreview returns one rollout's import preview: metadata plus
// its most recent messages.
func (s *Server) handleCodexSessionPreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cli := s.cfg().CLIByName(strings.TrimSpace(q.Get("cli")))
	if cli == nil || !cli.IsCodex() {
		writeErr(w, http.StatusBadRequest, "a codex kind cli is required")
		return
	}
	id := strings.TrimSpace(q.Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	cx, err := codexhist.FetchSession(codexHome(cli), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "codex session not found: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": cx.ID,
		"cwd":        cx.Cwd,
		"branch":     gitBranch(cx.Cwd),
		"updated_at": cx.Updated,
		"messages":   cx.Preview(8),
	})
}
