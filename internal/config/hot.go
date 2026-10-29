package config

import (
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Manager holds the live config behind an atomic pointer so the process can
// hot-reload config.yaml without a restart. Reload swaps the whole pointer, so
// concurrent readers never observe a partially-updated config.
type Manager struct {
	path string
	cur  atomic.Pointer[Config]

	// mu serializes the two write paths (Reload and SetDiscoveredProviders) so
	// a discovery result cannot be lost under a concurrent hot reload. Reads
	// stay lock-free through cur.
	mu         sync.Mutex
	discovered []Provider
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
	m.mu.Lock()
	defer m.mu.Unlock()
	boot := m.cur.Load()
	cfg.Listen = boot.Listen
	cfg.DataDir = boot.DataDir
	if cfg.TokenGenerated { // no explicit token in file/env: keep boot's
		cfg.GatewayToken = boot.GatewayToken
		cfg.TokenGenerated = boot.TokenGenerated
	}
	// Discovery is runtime state, not file state: a reload must not wipe the
	// opencode providers the server already found.
	cfg.DiscoveredProviders = m.discovered
	m.cur.Store(cfg)
	return cfg, nil
}

// SetDiscoveredProviders installs the providers found in the engines' own
// runtime config (currently opencode logins) on the live config. The set is
// carried across later Reloads until the next Set. It reports whether anything
// changed so callers broadcast config_changed only on real updates.
func (m *Manager) SetDiscoveredProviders(ps []Provider) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sameProviders(m.discovered, ps) {
		return false
	}
	// Copy: the caller's slice may be reused after return, and this snapshot is
	// shared by the live config and future Reloads. It is never mutated in
	// place — each Set replaces it wholesale.
	m.discovered = append([]Provider(nil), ps...)
	next := *m.cur.Load()
	next.DiscoveredProviders = m.discovered
	m.cur.Store(&next)
	return true
}

// sameProviders compares the fields discovery actually sets. Discovered
// entries are always kind-only (no env, no models), so this is enough to tell
// "same set" apart from a real change.
func sameProviders(a, b []Provider) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Kind != b[i].Kind {
			return false
		}
	}
	return true
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
