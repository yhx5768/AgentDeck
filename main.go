package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cc-webui/internal/config"
	"cc-webui/internal/runner"
	"cc-webui/internal/server"
	"cc-webui/internal/store"
)

//go:embed all:web
var webFS embed.FS

// version is the release version, injected at build time via
// -ldflags "-X main.version=<tag>"; "dev" for local/non-release builds.
var version = "dev"

func main() {
	configPath := flag.String("config", config.DefaultPath(), "path to config.yaml")
	listen := flag.String("listen", "", "override listen address (e.g. 0.0.0.0:8321)")
	showVersion := flag.Bool("version", false, "print version and exit")
	permHelper := flag.Bool("perm-helper", false, "run as PermissionRequest hook helper (internal, spawned by the CLI)")
	flag.Parse()

	if *permHelper {
		runner.RunPermHelper()
		return
	}

	if *showVersion {
		fmt.Printf("cc-webui %s\n", version)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\nCopy config.example.yaml to config.yaml and edit it.\n", err)
		os.Exit(1)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	cm := config.NewManager(*configPath, cfg)

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	mgr := runner.NewManager(cm, st)
	// Owns the shared `opencode serve` child processes (they hold provider
	// credentials in memory), so they are killed on shutdown.
	defer mgr.Close()

	static, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("embed fs: %v", err)
	}

	srv := server.New(cm, st, mgr, static)

	// opencode providers that were logged into via the CLI are not listed in
	// config.yaml; the discovery loop merges them into the live config and
	// broadcasts config_changed so the picker refreshes without a reload.
	go srv.StartProviderDiscovery(server.OpenCodeDiscoveryInterval)

	// Respawn resident-mode sessions (peer-messaging reachable) in the
	// background; staggered internally to avoid a boot storm.
	go mgr.RestoreResidents()

	// Hot-reload config.yaml (clis/providers/models); on every successful
	// reload, tell connected clients so the UI refreshes model lists, and kick
	// provider discovery in case the opencode cli set changed.
	go cm.Watch(3*time.Second, func() {
		mgr.BroadcastConfigChanged()
		srv.KickProviderDiscovery()
	})

	fmt.Printf("cc-webui\n")
	fmt.Printf("  listen:      %s\n", cfg.Listen)
	fmt.Printf("  data dir:    %s\n", cfg.DataDir)
	fmt.Printf("  clis:        %d configured\n", len(cfg.CLIs))
	fmt.Printf("  providers:   %d configured\n", len(cfg.Providers))
	if cfg.TokenGenerated {
		fmt.Printf("  GATEWAY_TOKEN (generated, set your own in config/env to persist):\n    %s\n", cfg.GatewayToken)
	}
	// Graceful shutdown must be wired BEFORE Run blocks: Run only returns when
	// the server stops, so a handler registered after it never runs.
	//
	// This matters most under a supervisor like screen/tmux, where Ctrl-C sends
	// SIGINT to the foreground process group. Without a handler the default
	// disposition kills cc-webui outright, every pooled `opencode serve` child is
	// reparented to launchd and keeps running — orphans holding provider
	// credentials in memory, re-accumulating on every restart.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		sig := <-sigs
		log.Printf("收到 %s，正在关闭（回收 opencode serve 子进程）", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("http shutdown: %v", err)
		}
	}()

	if err = srv.Run(); err != nil && err != http.ErrServerClosed {
		// Died on its own (port in use, listener failure) — no shutdown
		// goroutine will close shutdownDone, so don't block on it.
		log.Fatal(err)
	}
	// Run returned because Shutdown closed the listener. Wait (bounded) for the
	// shutdown goroutine so the deferred mgr.Close() only reaps serve children
	// after the HTTP server stopped handing out new turns.
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
	}
}
