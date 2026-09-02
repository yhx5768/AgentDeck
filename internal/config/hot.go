package config

import (
	"log"
	"os"
	"sync/atomic"
	"time"
)

// Manager holds the live config behind an atomic pointer so the process can
// hot-reload config.yaml without a restart. Reload swaps the whole pointer, so
// concurrent readers never observe a partially-updated config.
type Manager struct {
	path string
	cur  atomic.Pointer[Config]
}

// NewManager wraps a boot config loaded from path.
func NewManager(path string, boot *Config) *Manager {
	m := &Manager{path: path}
	m.cur.Store(boot)
	return m
}

// Get returns the current config.
func (m *Manager) Get() *Config { return m.cur.Load() }

// Reload re-reads the config file and swaps it in on success; the previous
// config stays active on error. Listen and DataDir are startup-only fields
// (the server is bound and the store is opened at boot paths) and keep their
// boot values; a gateway_token that is still auto-generated is preserved too
// so that editing the file does not log out every client.
func (m *Manager) Reload() (*Config, error) {
	cfg, err := Load(m.path)
	if err != nil {
		return nil, err
	}
	boot := m.Get()
	cfg.Listen = boot.Listen
	cfg.DataDir = boot.DataDir
	if cfg.TokenGenerated { // no explicit token in file/env: keep boot's
		cfg.GatewayToken = boot.GatewayToken
		cfg.TokenGenerated = boot.TokenGenerated
	}
	m.cur.Store(cfg)
	return cfg, nil
}

// Watch polls the config file every interval; when it changed, Reloads and
// calls onChange. A file that fails to parse keeps the previous config and is
// retried on the next tick, so a save still in progress is picked up once it
// completes (watch out: editors that write in place can update mtime before
// the content is finished).
func (m *Manager) Watch(interval time.Duration, onChange func()) {
	var lastMod time.Time
	var lastSize int64
	if st, err := os.Stat(m.path); err == nil {
		lastMod, lastSize = st.ModTime(), st.Size()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		st, err := os.Stat(m.path)
		if err != nil || (st.ModTime().Equal(lastMod) && st.Size() == lastSize) {
			continue
		}
		if _, err := m.Reload(); err != nil {
			log.Printf("reload config %s: %v", m.path, err)
			continue // keep lastMod: retry until the (possibly partial) write is done
		}
		lastMod, lastSize = st.ModTime(), st.Size()
		log.Printf("config %s reloaded", m.path)
		if onChange != nil {
			onChange()
		}
	}
}
