// Package main is the aiclibridge command-line entry point.
//
// aiclibridge exposes a unified HTTP API in front of multiple AI coding
// CLIs (Claude Code, Codex, OpenCode, OpenClaw). The daemon logic lives
// under internal/ and is wired up here: config → logger → store →
// catalog → facade → HTTP server, with graceful shutdown on SIGINT /
// SIGTERM. The shutdown order is HTTP (reject new requests) → facade
// (cancel in-flight runs) → store (release SQLite handles).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tgcz2011/aiclibridge/internal/api"
	"github.com/tgcz2011/aiclibridge/internal/config"
	"github.com/tgcz2011/aiclibridge/internal/detect"
	"github.com/tgcz2011/aiclibridge/internal/facade"
	"github.com/tgcz2011/aiclibridge/internal/store"
)

func main() {
	configPath := flag.String("config", "", "path to config file (default: search order)")
	flag.Parse()

	// 1. Load config (env overrides applied by Load).
	resolved, err := config.ResolveConfigPath(*configPath)
	if err != nil {
		fail("resolve config path: %v", err)
	}
	cfg, err := config.Load(resolved)
	if err != nil {
		fail("load config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		fail("invalid config: %v", err)
	}

	// 2. Logger — JSON to stdout, level from cfg.LogLevel.
	logger := setupLogger(cfg.LogLevel)

	// 3. Data dir + SQLite store. MkdirAll is idempotent.
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		fail("mkdir data dir: %v", err)
	}
	dbPath := filepath.Join(cfg.DataDir, "aiclibridge.db")
	st, err := store.Open(dbPath)
	if err != nil {
		fail("open store: %v", err)
	}

	// 4. Detect CLIs. Best effort: a discovery failure is logged and
	// falls back to the hardcoded catalog so the daemon still starts.
	ctx := context.Background()
	catalog, err := detect.Discover(ctx)
	if err != nil {
		logger.Warn("detect failed, using default catalog", "error", err)
		catalog = detect.DefaultCatalog()
	}
	logCatalogSummary(logger, catalog)

	// 5. Facade — wires adapters to the store + catalog.
	fc, err := facade.New(cfg, st, catalog, logger)
	if err != nil {
		cleanupStore(st)
		fail("build facade: %v", err)
	}

	// 6. HTTP server. ReadHeaderTimeout caps slowloris; no Read /
	// Write timeout because SSE streams and long-running runs must
	// hold connections open indefinitely.
	srv := api.NewServer(fc, cfg, logger)
	httpSrv := &http.Server{
		Addr:              srv.ListenAddr(),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 7. Run + wait for signal. A ListenAndServe failure (port in use,
	// bind error) is fatal; runtime request errors are handled by the
	// API layer and never reach here.
	errCh := make(chan error, 1)
	go func() {
		logger.Info("aiclibridge listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		// Server failed to serve — shut down best-effort and exit.
		logger.Error("http server failed", "error", err)
		_ = fc.Close()
		_ = st.Close()
		os.Exit(1)
	case sig := <-sigCh:
		logger.Info("shutting down", "signal", sig.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// HTTP first (reject new requests, drain short in-flight),
		// then facade (cancel running runs), then store.
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("http shutdown exceeded budget", "error", err)
		}
		if err := fc.Close(); err != nil {
			logger.Warn("facade close error", "error", err)
		}
		if err := st.Close(); err != nil {
			logger.Warn("store close error", "error", err)
		}
		logger.Info("aiclibridge stopped")
	}
}

// setupLogger builds a slog.Logger writing JSON to stdout at the requested
// level. An unknown level falls back to info so a typo never silences the
// daemon.
func setupLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}

// logCatalogSummary logs one line per detected CLI and a final summary.
// Available CLIs are logged at info; missing ones at warn so operators
// can spot a host missing an expected CLI at a glance.
func logCatalogSummary(logger *slog.Logger, catalog []detect.CLIInfo) {
	available := 0
	for _, cli := range catalog {
		if cli.Available {
			available++
			logger.Info("detected CLI",
				"name", cli.Name,
				"version", cli.Version,
				"path", cli.Path,
				"providers", len(cli.Providers))
		} else {
			logger.Warn("CLI not available", "name", cli.Name)
		}
	}
	logger.Info("catalog summary", "total", len(catalog), "available", available)
}

// cleanupStore closes the store on a startup failure path. Errors are
// ignored — the daemon is exiting anyway and the caller already has a
// more informative error to report.
func cleanupStore(st *store.Store) { _ = st.Close() }

// fail prints a message to stderr and exits 1. Used for startup failures
// where the logger may not yet be initialised (or the failure is severe
// enough that the operator needs a plain-text stderr line).
func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "aiclibridge: "+format+"\n", args...)
	os.Exit(1)
}
