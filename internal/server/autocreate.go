package server

import (
	"log"
	"os"
	"strings"
	"time"

	"cc-webui/internal/config"
	"cc-webui/internal/runner"
	"cc-webui/internal/store"
)

// EnsureMainAgent creates a default mainAgent session on a fresh store so a
// new install can land in Personal Agent without the user first creating one by
// hand. It is called once at boot, before RestoreResidents, so the new resident
// flag is picked up by the normal restore pass.
//
// Existing live main agents are never touched. main_agent.autocreate=false
// disables the whole step; a config with no usable cli/provider/cwd logs why it
// was skipped and leaves the store unchanged.
func (s *Server) EnsureMainAgent() {
	if s == nil || s.st == nil || s.mgr == nil {
		return
	}
	cfg := s.cfg()
	if cfg == nil || !cfg.MainAgentAutocreateEnabled() {
		return
	}
	for _, sess := range s.st.List() {
		if sess.MainAgent && !sess.Archived {
			return
		}
	}
	if len(cfg.CLIs) == 0 {
		log.Printf("autocreate main agent skipped: no cli configured")
		return
	}

	var wd config.WorkerDefaults
	if cfg.MainAgent != nil {
		wd = cfg.MainAgent.Worker
	}
	cli := cfg.CLIByName(strings.TrimSpace(wd.CLI))
	if cli == nil {
		cli = &cfg.CLIs[0]
	}
	prov := cfg.ProviderByName(strings.TrimSpace(wd.Provider))
	if prov == nil || prov.Kind != cli.Kind {
		prov = s.firstProviderOfKind(cli.Kind)
	}
	if prov == nil {
		log.Printf("autocreate main agent skipped: no provider for cli %q (%s)", cli.Name, cli.Kind)
		return
	}

	cwd := strings.TrimSpace(cfg.DefaultCwd)
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	if cwd == "" {
		log.Printf("autocreate main agent skipped: no default cwd and home is unavailable")
		return
	}
	resolvedCwd, err := runner.ResolveCwd(cwd)
	if err != nil {
		log.Printf("autocreate main agent skipped: %v", err)
		return
	}

	effort := strings.TrimSpace(wd.Effort)
	if effort != "" && !effortOKFor(cli.Kind, effort) {
		log.Printf("autocreate main agent: ignoring effort %q unsupported by %s", effort, cli.Kind)
		effort = ""
	}
	now := time.Now()
	sess := &store.Session{
		ID:             newID(),
		Name:           "mainAgent",
		CLI:            cli.Name,
		Provider:       prov.Name,
		Cwd:            resolvedCwd,
		ModelOverride:  strings.TrimSpace(wd.Model),
		EffortOverride: effort,
		// Resident is a Claude Code protocol feature. On a fresh install whose
		// first cli is another engine we still create the main agent, just
		// without a resident process; A2A delivery does not require one.
		Resident:   cli.IsClaude(),
		PeerName:   runner.CleanPeerName("mainAgent"),
		RolePrompt: s.mainAgentPrompt(),
		MainAgent:  true,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.st.Upsert(sess, nil); err != nil {
		log.Printf("autocreate main agent failed: %v", err)
		return
	}
	log.Printf("autocreated main agent %s (cli=%s provider=%s resident=%v cwd=%s)", sess.ID, sess.CLI, sess.Provider, sess.Resident, sess.Cwd)
}
