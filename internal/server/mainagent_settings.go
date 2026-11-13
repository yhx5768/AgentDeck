package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"cc-webui/internal/config"
)

// mainAgentWorkerFile is the runtime override for config.yaml's
// main_agent.worker. It follows the same pattern as the browser settings
// switch (data/browser/settings.json): the Settings page writes it, the file
// survives restarts, and config.yaml remains the fallback. A field left empty
// means "inherit config.yaml"; resident uses a nil pointer for the same reason.
const mainAgentWorkerFile = "main-agent/worker.json"

// mainAgentWorkerStore owns the persisted override. Reads are lazy so a Server
// built directly in tests (without New) still picks up <data_dir>/main-agent/
// worker.json on first use.
type mainAgentWorkerStore struct {
	mu     sync.Mutex
	path   string
	loaded bool
	worker config.WorkerDefaults
}

func newMainAgentWorkerStore(path string) *mainAgentWorkerStore {
	return &mainAgentWorkerStore{path: path}
}

func (st *mainAgentWorkerStore) loadLocked() {
	if st.loaded {
		return
	}
	st.loaded = true
	if st.path == "" {
		return
	}
	raw, err := os.ReadFile(st.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("read main agent worker settings %s: %v", st.path, err)
		}
		return
	}
	var w config.WorkerDefaults
	if err := json.Unmarshal(raw, &w); err != nil {
		log.Printf("parse main agent worker settings %s: %v", st.path, err)
		return
	}
	st.worker = normalizeWorkerDefaults(w)
}

func (st *mainAgentWorkerStore) get() config.WorkerDefaults {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	return st.worker
}

// save persists the override atomically (temp+rename, 0600) before publishing
// it in memory, so a failed write leaves the previous setting active.
func (st *mainAgentWorkerStore) save(w config.WorkerDefaults) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	w = normalizeWorkerDefaults(w)
	if st.path != "" {
		if err := os.MkdirAll(filepath.Dir(st.path), 0o700); err != nil {
			return err
		}
		raw, err := json.Marshal(w)
		if err != nil {
			return err
		}
		tmp := st.path + ".tmp"
		if err := os.WriteFile(tmp, raw, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, st.path); err != nil {
			return err
		}
	}
	st.worker = w
	return nil
}

func normalizeWorkerDefaults(w config.WorkerDefaults) config.WorkerDefaults {
	w.CLI = strings.TrimSpace(w.CLI)
	w.Provider = strings.TrimSpace(w.Provider)
	w.Model = strings.TrimSpace(w.Model)
	w.Effort = strings.TrimSpace(w.Effort)
	return w
}

// mainAgentWorker returns the lazy per-Server settings store. The path is
// derived from the live config so a data_dir change (hot reload) does not
// silently write somewhere else — listen/data_dir are startup-only anyway.
func (s *Server) mainAgentWorker() *mainAgentWorkerStore {
	s.maWorkerMu.Lock()
	defer s.maWorkerMu.Unlock()
	if s.maWorkerStore == nil {
		path := ""
		if cfg := s.cfg(); cfg != nil && cfg.DataDir != "" {
			path = filepath.Join(cfg.DataDir, filepath.FromSlash(mainAgentWorkerFile))
		}
		s.maWorkerStore = newMainAgentWorkerStore(path)
	}
	return s.maWorkerStore
}

// mergeWorkerDefaults applies the runtime override field by field on top of
// config.yaml's main_agent.worker. Empty override fields (and a nil resident)
// inherit the config value; sources records where each effective value came
// from ("setting", "config", or "" for the built-in default).
func mergeWorkerDefaults(over, base config.WorkerDefaults) (config.WorkerDefaults, map[string]string) {
	src := map[string]string{}
	pick := func(name, ov, cfg string) string {
		switch {
		case ov != "":
			src[name] = "setting"
			return ov
		case cfg != "":
			src[name] = "config"
			return cfg
		default:
			src[name] = ""
			return ""
		}
	}
	out := config.WorkerDefaults{
		CLI:      pick("cli", over.CLI, base.CLI),
		Provider: pick("provider", over.Provider, base.Provider),
		Model:    pick("model", over.Model, base.Model),
		Effort:   pick("effort", over.Effort, base.Effort),
	}
	switch {
	case over.Resident != nil:
		out.Resident = over.Resident
		src["resident"] = "setting"
	case base.Resident != nil:
		out.Resident = base.Resident
		src["resident"] = "config"
	default:
		src["resident"] = ""
	}
	return out, src
}

// effectiveWorkerDefaults is the merged profile the built-in prompt
// interpolates: runtime setting → config.yaml → unset (the template then omits
// the worker-default clause).
func (s *Server) effectiveWorkerDefaults() config.WorkerDefaults {
	var base config.WorkerDefaults
	if cfg := s.cfg(); cfg != nil && cfg.MainAgent != nil {
		base = normalizeWorkerDefaults(cfg.MainAgent.Worker)
	}
	eff, _ := mergeWorkerDefaults(s.mainAgentWorker().get(), base)
	return eff
}

type mainAgentWorkerView struct {
	// Worker is the effective profile (runtime override merged over config).
	Worker config.WorkerDefaults `json:"worker"`
	// ConfigWorker is config.yaml's main_agent.worker untouched, so the UI can
	// label "inherit config (<value>)" without another request.
	ConfigWorker config.WorkerDefaults `json:"config_worker"`
	// Overrides is only the runtime override, so the UI knows which selects
	// have an explicit (possibly-to-be-cleared) value.
	Overrides config.WorkerDefaults `json:"overrides"`
	// Sources maps each field to "setting" / "config" / "".
	Sources map[string]string `json:"sources"`
	// WorkerLine is the exact clause interpolated into the built-in prompt
	// (empty when nothing is configured).
	WorkerLine string `json:"worker_line"`
	// PromptCustomized is true when config.yaml's main_agent.prompt replaces
	// the whole built-in template — the worker defaults then never appear.
	PromptCustomized bool `json:"prompt_customized"`
}

func (s *Server) mainAgentWorkerView() mainAgentWorkerView {
	var base config.WorkerDefaults
	customized := false
	if cfg := s.cfg(); cfg != nil && cfg.MainAgent != nil {
		base = normalizeWorkerDefaults(cfg.MainAgent.Worker)
		customized = strings.TrimSpace(cfg.MainAgent.Prompt) != ""
	}
	over := s.mainAgentWorker().get()
	eff, src := mergeWorkerDefaults(over, base)
	return mainAgentWorkerView{
		Worker:           eff,
		ConfigWorker:     base,
		Overrides:        over,
		Sources:          src,
		WorkerLine:       workerDefaultLine(eff),
		PromptCustomized: customized,
	}
}

// validateWorkerDefaults checks the merged profile that will actually be
// rendered. Validation is deliberately against the effective values (not just
// the new overrides): a cli/provider that only makes sense when paired is
// rejected here instead of producing a silently broken prompt.
func (s *Server) validateWorkerDefaults(over config.WorkerDefaults) error {
	cfg := s.cfg()
	if cfg == nil {
		return fmt.Errorf("配置未加载")
	}
	var base config.WorkerDefaults
	if cfg.MainAgent != nil {
		base = normalizeWorkerDefaults(cfg.MainAgent.Worker)
	}
	eff, _ := mergeWorkerDefaults(over, base)

	var cli *config.CLI
	if eff.CLI != "" {
		cli = cfg.CLIByName(eff.CLI)
		if cli == nil {
			return fmt.Errorf("未知 cli：%s", eff.CLI)
		}
	}
	var prov *config.Provider
	if eff.Provider != "" {
		prov = cfg.ProviderByName(eff.Provider)
		if prov == nil {
			return fmt.Errorf("未知 provider：%s", eff.Provider)
		}
		if cli != nil && prov.Kind != cli.Kind {
			return fmt.Errorf("引擎不匹配：cli %q 是 %s，provider %q 是 %s", cli.Name, cli.Kind, prov.Name, prov.Kind)
		}
	}
	if eff.Model != "" {
		if prov == nil {
			return fmt.Errorf("设置 model 前先选择 provider")
		}
		if !s.modelKnown(cli, prov, eff.Model) {
			return fmt.Errorf("未知模型：%s（provider %s）", eff.Model, prov.Name)
		}
	}
	if eff.Effort != "" {
		if err := s.validateEffort(cli, prov, eff.Model, eff.Effort); err != nil {
			return err
		}
	}
	if eff.Resident != nil && *eff.Resident {
		if cli == nil {
			return fmt.Errorf("开启常驻前先选择 claude cli")
		}
		if cli.Kind != config.KindClaude {
			return fmt.Errorf("只有 claude 会话支持常驻")
		}
	}
	return nil
}

func (s *Server) handleMainAgentWorker(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mainAgentWorkerView())
}

// handleSaveMainAgentWorker replaces the runtime override wholesale: empty
// strings (and a null/absent resident) clear that field back to config.yaml.
func (s *Server) handleSaveMainAgentWorker(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CLI      string `json:"cli"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Effort   string `json:"effort"`
		Resident *bool  `json:"resident"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	over := normalizeWorkerDefaults(config.WorkerDefaults{
		CLI: body.CLI, Provider: body.Provider, Model: body.Model,
		Effort: body.Effort, Resident: body.Resident,
	})
	if err := s.validateWorkerDefaults(over); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.mainAgentWorker().save(over); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.mainAgentWorkerView())
}