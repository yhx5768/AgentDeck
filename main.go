package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	"cc-webui/internal/config"
	"cc-webui/internal/runner"
	"cc-webui/internal/server"
	"cc-webui/internal/store"
)

//go:embed all:web
var webFS embed.FS

func main() {
	configPath := flag.String("config", config.DefaultPath(), "path to config.yaml")
	listen := flag.String("listen", "", "override listen address (e.g. 0.0.0.0:8321)")
	flag.Parse()

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

	static, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("embed fs: %v", err)
	}

	srv := server.New(cm, st, mgr, static)

	// Respawn resident-mode sessions (peer-messaging reachable) in the
	// background; staggered internally to avoid a boot storm.
	go mgr.RestoreResidents()

	// Hot-reload config.yaml (clis/providers/models); on every successful
	// reload, tell connected clients so the UI refreshes model lists. The
	// initial config itself is served via cm for later reads.
	go cm.Watch(3*time.Second, mgr.BroadcastConfigChanged)

	fmt.Printf("cc-webui\n")
	fmt.Printf("  listen:      %s\n", cfg.Listen)
	fmt.Printf("  data dir:    %s\n", cfg.DataDir)
	fmt.Printf("  clis:        %d configured\n", len(cfg.CLIs))
	fmt.Printf("  providers:   %d configured\n", len(cfg.Providers))
	if cfg.TokenGenerated {
		fmt.Printf("  GATEWAY_TOKEN (generated, set your own in config/env to persist):\n    %s\n", cfg.GatewayToken)
	}
	if err := srv.Run(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
