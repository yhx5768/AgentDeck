package server

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// mainAgentAutocreateFile is the runtime override for config.yaml's
// main_agent.autocreate. The Settings page writes it; the file survives
// restarts. Missing/unreadable means "inherit config.yaml" (whose default is
// off).
const mainAgentAutocreateFile = "main-agent/autocreate.json"

type mainAgentAutocreateStore struct {
	mu     sync.Mutex
	path   string
	loaded bool
	value  bool
	set    bool
}

func newMainAgentAutocreateStore(path string) *mainAgentAutocreateStore {
	return &mainAgentAutocreateStore{path: path}
}

func (st *mainAgentAutocreateStore) loadLocked() {
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
			log.Printf("read main agent autocreate settings %s: %v", st.path, err)
		}
		return
	}
	var v struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.Enabled == nil {
		if err != nil {
			log.Printf("parse main agent autocreate settings %s: %v", st.path, err)
		}
		return
	}
	st.value = *v.Enabled
	st.set = true
}

func (st *mainAgentAutocreateStore) get() (bool, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	return st.value, st.set
}

func (st *mainAgentAutocreateStore) save(enabled bool) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.path != "" {
		if err := os.MkdirAll(filepath.Dir(st.path), 0o700); err != nil {
			return err
		}
		raw, err := json.Marshal(struct {
			Enabled bool `json:"enabled"`
		}{Enabled: enabled})
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
	st.value = enabled
	st.set = true
	return nil
}

func (s *Server) mainAgentAutocreate() *mainAgentAutocreateStore {
	s.maAutocreateMu.Lock()
	defer s.maAutocreateMu.Unlock()
	if s.maAutocreateStore == nil {
		path := ""
		if cfg := s.cfg(); cfg != nil && cfg.DataDir != "" {
			path = filepath.Join(cfg.DataDir, filepath.FromSlash(mainAgentAutocreateFile))
		}
		s.maAutocreateStore = newMainAgentAutocreateStore(path)
	}
	return s.maAutocreateStore
}

// mainAgentAutocreateEnabled is the effective switch: Settings override first,
// then config.yaml's main_agent.autocreate (default off).
func (s *Server) mainAgentAutocreateEnabled() bool {
	if v, ok := s.mainAgentAutocreate().get(); ok {
		return v
	}
	return s.cfg().MainAgentAutocreateEnabled()
}

type mainAgentAutocreateView struct {
	Enabled bool   `json:"enabled"`
	Source  string `json:"source"` // setting | config | default
}

func (s *Server) mainAgentAutocreateView() mainAgentAutocreateView {
	v, ok := s.mainAgentAutocreate().get()
	if ok {
		return mainAgentAutocreateView{Enabled: v, Source: "setting"}
	}
	cfg := s.cfg()
	if cfg != nil && cfg.MainAgent != nil && cfg.MainAgent.Autocreate != nil {
		return mainAgentAutocreateView{Enabled: *cfg.MainAgent.Autocreate, Source: "config"}
	}
	return mainAgentAutocreateView{Enabled: false, Source: "default"}
}

func (s *Server) handleMainAgentAutocreate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mainAgentAutocreateView())
}

func (s *Server) handleSaveMainAgentAutocreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decode(r, &body); err != nil || body.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "enabled (boolean) is required")
		return
	}
	if err := s.mainAgentAutocreate().save(*body.Enabled); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.mainAgentAutocreateView())
}
