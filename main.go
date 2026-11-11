// Command opencode-cc is a local reverse proxy that lets Claude Code talk to
// OpenCode Zen (and other OpenAI-compatible endpoints) by translating the
// Anthropic Messages API to OpenAI Chat Completions on the fly, with a
// built-in web control panel.
//
// Quick start:
//
//	set ZEN_API_KEY=sk-...
//	opencode-cc.exe
//	# then point Claude Code at it:
//	set ANTHROPIC_BASE_URL=http://localhost:8787
//	set ANTHROPIC_AUTH_TOKEN=anything
//	claude
package main

import (
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/PravinMandal/OpenCode-CC/internal/assets"
	"github.com/PravinMandal/OpenCode-CC/internal/config"
	"github.com/PravinMandal/OpenCode-CC/internal/server"
	"github.com/PravinMandal/OpenCode-CC/internal/store"
)

func main() {
	dataDir := flag.String("data", "data", "directory for config + SQLite database")
	flag.Parse()

	abs, err := filepath.Abs(*dataDir)
	if err != nil {
		log.Fatalf("resolve data dir: %v", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		log.Fatalf("create data dir: %v", err)
	}
	// The data dir holds the config, API-key hashes and request logs. Repair a
	// pre-existing 0755 directory from an older build; a manual (non-service)
	// run with umask 022 must not leave them world-readable.
	if err := os.Chmod(abs, 0o700); err != nil {
		log.Printf("WARNING: tighten data dir permissions: %v", err)
	}

	cfg, err := config.Load(abs)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	st, err := store.Open(filepath.Join(abs, "opencode-cc.db"))
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer st.Close()

	srv := server.New(cfg, st)

	// Resolve panel assets (embedded build or on-disk web/dist).
	panelFS, hasAssets := assets.FileSystem()
	var panelHandler http.Handler
	if hasAssets {
		panelHandler = assets.SPAMux(panelFS)
	}

	handler := srv.Handler(panelFS, panelHandler)

	addr := cfg.Snapshot().ListenAddr
	log.Printf("opencode-cc listening on http://%s  (panel: %v, data: %s)", addr, hasAssets, abs)
	if cfg.Snapshot().ZenAPIKey == "" {
		log.Printf("WARNING: no ZEN_API_KEY set — set it via env var or the web panel at http://%s", addr)
	} else {
		log.Printf("upstream: %s   default model: %s", cfg.Snapshot().UpstreamBase, cfg.Snapshot().DefaultModel)
	}

	// Graceful shutdown on Ctrl+C / service stop.
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Start(handler)
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server: %v", err)
			// Start failed (e.g. the port is taken). Still tear down the
			// sidecar, background loops and database before exiting;
			// log.Fatalf skipped every defer and orphaned the Bun process.
			if shutdownErr := srv.Shutdown(); shutdownErr != nil {
				log.Printf("shutdown: %v", shutdownErr)
			}
			if closeErr := st.Close(); closeErr != nil {
				log.Printf("close database: %v", closeErr)
			}
			os.Exit(1)
		}
		return
	case <-sig:
	}

	log.Printf("shutting down...")
	// Shutdown closes the keep-alive and retention loops, stops the Bun
	// sidecar (a whole process group) and drains in-flight requests. Without
	// this every restart orphaned the sidecar until its parent died.
	if err := srv.Shutdown(); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
