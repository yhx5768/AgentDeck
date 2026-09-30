package server

// Qoder CLI CN (qoderclicn) is a Claude Code fork: same stream-json protocol,
// same on-disk transcript layout under its own home directory. Only three
// things are engine-specific enough to deserve code — the model list (the CLI
// owns it, not config.yaml), the import listing, and the import preview.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"cc-webui/internal/claudehist"
	"cc-webui/internal/config"
)

// qoderListModelsTimeout bounds the `qoderclicn --list-models` call behind the
// model dropdown. The CLI needs a network round-trip to the account, so this is
// slower than a local read but still interactive.
const qoderListModelsTimeout = 20 * time.Second

// qoderModelsTTL memoizes the model list. The set only changes when the
// account's plan or the CLI changes, both rare; the same TTL the opencode
// variant keys and the mcode catalog use.
const qoderModelsTTL = 10 * time.Minute

var qoderModelsCache = struct {
	sync.Mutex
	m map[string]qoderModelsEntry
}{m: map[string]qoderModelsEntry{}}

type qoderModelsEntry struct {
	ts   time.Time
	data []config.Model
}

// errQoderNoModels covers an empty parse: a logged-out CLI prints usage or an
// auth error rather than a table, and treating that as "this account has no
// models" would silently empty the dropdown instead of surfacing a problem.
var errQoderNoModels = errors.New("qoderclicn --list-models 未返回任何模型（CLI 是否已登录？）")

// qoderModels returns the models the account may use, from
// `qoderclicn --list-models`. fresh bypasses the memo.
//
// The list is deliberately NOT declared in config.yaml: it is per-account and
// changes with the CLI (a pinned copy would go stale and start rejecting models
// the user can actually run), the same reasoning as mcode's catalog.
func qoderModels(cliPath string, fresh bool) ([]config.Model, error) {
	if cliPath == "" {
		cliPath = "qoderclicn"
	}
	qoderModelsCache.Lock()
	defer qoderModelsCache.Unlock()
	if e, ok := qoderModelsCache.m[cliPath]; ok && time.Since(e.ts) < qoderModelsTTL && !fresh {
		return e.data, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), qoderListModelsTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, cliPath, "--list-models").Output()
	if err != nil {
		return nil, err
	}
	models := parseQoderModels(string(out))
	if len(models) == 0 {
		return nil, errQoderNoModels
	}
	qoderModelsCache.m[cliPath] = qoderModelsEntry{ts: time.Now(), data: models}
	return models, nil
}

// parseQoderModels reads the `--list-models` table. The observed output is a
// "MODEL" header line followed by one model name per line:
//
//	MODEL
//	Qwen3.8-Max
//	Qwen3.8-Flash
//
// Only the first whitespace-separated field of each row is taken, so a future
// extra column (context window, quota, …) cannot be mistaken for part of a
// model name. Rows whose first field is not a plausible model name are dropped:
// the CLI reports trouble on stdout too (a logged-out run prints an error line,
// and "error:" would otherwise be parsed as a model and cached for 10 minutes).
//
// Input modalities: Qwen3.8-Flash was verified to accept a base64 image over
// --input-format stream-json (it named the color of a synthetic PNG correctly),
// and the CLI advertises no per-model modality metadata, so every model is
// declared text+image. The UI only uses this to decide whether to offer the
// image picker — declaring "text" would block a capability that works.
func parseQoderModels(out string) []config.Model {
	rows := strings.Split(out, "\n")
	models := []config.Model{}
	seen := map[string]bool{}
	for i, row := range rows {
		fields := strings.Fields(row)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if i == 0 && strings.EqualFold(name, "model") {
			continue // header row
		}
		if !plausibleModelName(name) {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		models = append(models, config.Model{Name: name, Input: []string{"text", "image"}})
	}
	return models
}

// plausibleModelName reports whether a table cell looks like a model id rather
// than a diagnostic message. Every model name the CLIs here use is made of
// letters, digits, dot, dash and underscore; a colon (as in "error:") or a
// stray word does not.
func plausibleModelName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// handleQoderModels reports the models and reasoning-effort levels of the
// qoder engine. Both are owned by the CLI, so both are served from here and the
// model/effort dropdowns are rebuilt from the response — a session can never be
// created with a model or level the CLI would reject.
// ?fresh=1 bypasses the memo.
func (s *Server) handleQoderModels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cli := s.cfg().CLIByName(strings.TrimSpace(q.Get("cli")))
	if cli == nil || !cli.IsQoder() {
		writeErr(w, http.StatusBadRequest, "需要 qoder 类型的 cli")
		return
	}
	prov := s.cfg().ProviderByName(strings.TrimSpace(q.Get("provider")))
	if prov == nil || !prov.IsQoder() {
		writeErr(w, http.StatusBadRequest, "需要 qoder 类型的 provider")
		return
	}
	models, err := qoderModels(cli.Path, q.Get("fresh") == "1")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if models == nil {
		models = []config.Model{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models, "efforts": qoderEfforts})
}

// handleQoderSessions lists the qoder sessions on disk, the import source for
// the qoder tab. Same shape as the claude endpoint (cwd selects a project
// bucket, "" lists every project) since both read the same layout.
func (s *Server) handleQoderSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cwd := q.Get("cwd")
	prefix := strings.ToLower(strings.TrimSpace(q.Get("prefix")))
	list, err := claudehist.ListIn(claudehist.QoderProfile, cwd)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if prefix != "" {
		filtered := list[:0]
		for _, item := range list {
			if strings.HasPrefix(strings.ToLower(item.SessionID), prefix) {
				filtered = append(filtered, item)
			}
		}
		list = filtered
	}
	if list == nil {
		list = []*claudehist.SessionInfo{}
	}
	// Cap to keep the UI snappy.
	if len(list) > 200 {
		list = list[:200]
	}
	writeJSON(w, http.StatusOK, list)
}

// handleQoderSessionPreview mirrors handleClaudeSessionPreview over the qoder
// transcript root: metadata + the last few messages of one session. branch is a
// lazy git query ("" outside a repo or on timeout).
func (s *Server) handleQoderSessionPreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := strings.TrimSpace(q.Get("id"))
	cwd := strings.TrimSpace(q.Get("cwd"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if cwd == "" {
		// 客户端没给 cwd（旧数据）时按 id 在全部项目里定位
		if list, err := claudehist.ListIn(claudehist.QoderProfile, ""); err == nil {
			for _, it := range list {
				if it.SessionID == id {
					cwd = it.Cwd
					break
				}
			}
		}
	}
	path := claudehist.HistoryPathIn(claudehist.QoderProfile, cwd, id)
	fi, err := os.Stat(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "qoder session history not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": id,
		"cwd":        cwd,
		"branch":     gitBranch(cwd),
		"updated_at": fi.ModTime(),
		"messages":   claudehist.PreviewIn(claudehist.QoderProfile, cwd, id, 8),
	})
}
