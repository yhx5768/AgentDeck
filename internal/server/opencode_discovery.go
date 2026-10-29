package server

import (
	"log"
	"time"

	"cc-webui/internal/config"
	"cc-webui/internal/opencodehist"
	"cc-webui/internal/opencodev2"
)

// OpenCodeDiscoveryInterval is how often the server re-reads the opencode
// clis' logged-in provider lists. New integrations are rare and the CLI call
// costs a second or two, so this only needs to be good enough for "log in,
// switch back to the UI": a config reload kicks an immediate run, and page
// loads see whatever the last run found.
const OpenCodeDiscoveryInterval = 10 * time.Minute

// ocProviderIDs asks one cli which providers it currently exposes. The protocol
// split mirrors every other opencode read path: v2 moved the list to the API,
// v1 still prints it as "<provider>/<model>" lines.
func ocProviderIDs(cli *config.CLI) ([]string, error) {
	if ocIsV2(cli) {
		return opencodev2.ProviderIDs(cli.Path)
	}
	return opencodehist.ProviderIDs(cli.Path)
}

// StartProviderDiscovery runs the auto-provider loop: it reads every
// opencode-kind cli's provider list, merges the ids into the live config as
// discovered providers, and repeats on interval (immediately, then after each
// tick or KickProviderDiscovery). Call it from main only — tests construct
// Servers without expecting CLI processes to be spawned.
func (s *Server) StartProviderDiscovery(interval time.Duration) {
	if s.cm == nil {
		return
	}
	if interval <= 0 {
		interval = OpenCodeDiscoveryInterval
	}
	if s.ocDiscoverKick == nil {
		s.ocDiscoverKick = make(chan struct{}, 1)
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			s.discoverOpenCodeProviders()
			select {
			case <-ticker.C:
			case <-s.ocDiscoverKick:
			}
		}
	}()
}

// KickProviderDiscovery asks the discovery loop to run again as soon as it
// finishes the current pass (used after config.yaml changed, so a newly added
// opencode cli does not wait for the next tick).
func (s *Server) KickProviderDiscovery() {
	if s.ocDiscoverKick == nil {
		return
	}
	select {
	case s.ocDiscoverKick <- struct{}{}:
	default: // one pending kick is enough
	}
}

// discoverOpenCodeProviders refreshes the per-cli provider lists and installs
// the merged set on the config manager. It is add/remove-live within a process:
// providers whose cli disappeared or no longer reports them are dropped, while
// a cli whose read failed keeps its previous list (a transient CLI failure must
// not empty a working picker). Config-declared providers always shadow
// discovered ones — see mergeOpenCodeProviders.
func (s *Server) discoverOpenCodeProviders() {
	cfg := s.cfg()
	liveCLIs := map[string]bool{}
	reads := map[string][]string{}
	for i := range cfg.CLIs {
		cli := &cfg.CLIs[i]
		if !cli.IsOpenCode() {
			continue
		}
		liveCLIs[cli.Name] = true
		ids, err := ocProviderIDs(cli)
		if err != nil {
			log.Printf("opencode provider discovery for cli %q failed (keeping previous list): %v", cli.Name, err)
			continue
		}
		reads[cli.Name] = ids
	}

	s.ocProvidersMu.Lock()
	if s.ocProviders == nil {
		s.ocProviders = map[string][]string{}
	}
	for name, ids := range reads {
		s.ocProviders[name] = ids
	}
	for name := range s.ocProviders {
		if !liveCLIs[name] {
			delete(s.ocProviders, name)
		}
	}
	perCLI := make(map[string][]string, len(s.ocProviders))
	for name, ids := range s.ocProviders {
		perCLI[name] = ids
	}
	s.ocProvidersMu.Unlock()

	discovered := mergeOpenCodeProviders(cfg, perCLI)
	if s.cm.SetDiscoveredProviders(discovered) {
		log.Printf("opencode providers discovered: %d auto provider(s)", len(discovered))
		if s.mgr != nil {
			s.mgr.BroadcastConfigChanged()
		}
	}
}

// mergeOpenCodeProviders turns per-cli provider-id lists into discovered
// provider entries. Config-declared names always win (they may carry env or a
// static model/input hint list), duplicates across clis are collapsed, and the
// order is deterministic: cli order, then the order the cli reported its
// providers. Pure so the merge rules are testable without running a CLI.
func mergeOpenCodeProviders(cfg *config.Config, perCLI map[string][]string) []config.Provider {
	if cfg == nil {
		return nil
	}
	declared := make(map[string]bool, len(cfg.Providers))
	for i := range cfg.Providers {
		declared[cfg.Providers[i].Name] = true
	}
	seen := map[string]bool{}
	var out []config.Provider
	for i := range cfg.CLIs {
		cli := &cfg.CLIs[i]
		if !cli.IsOpenCode() {
			continue
		}
		for _, id := range perCLI[cli.Name] {
			if id == "" || declared[id] || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, config.Provider{Name: id, Kind: config.KindOpenCode})
		}
	}
	return out
}
