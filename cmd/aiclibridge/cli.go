// Package main hosts the subcommand implementations for aiclibridge.
//
// cli.go wires the single-binary + subcommand dispatch model: every
// verb (serve / run / agents / models / cancel / get / version) is a
// standalone run<Name>(args []string) int entry point that main.go
// switches on. The shared in-process stack (config → logger →
// in-memory store → detect → facade) is bundled into the cli struct so
// `run` / `agents` / `models` — the local, daemon-free verbs — pay the
// setup cost once and clean up via a single defer. `serve` retains its
// own persistent setup (it owns the SQLite file) and `cancel` / `get`
// skip the facade entirely (they only need the config to know where the
// daemon lives).
//
// # Event routing for `run`
//
// streamEvents routes protocol.Event frames to two streams so a user
// can pipe stdout to the next tool while still seeing tool/progress
// chatter on stderr:
//
//   - text + thinking content → stdout (raw, no prefix)
//   - tool_use / tool_result / status / error / log / result → stderr
//
// The terminal EventResult is printed to stderr with status, duration,
// session_id, and (on failure) the error string, then mapped to an exit
// code by exitCodeForStatus: completed=0, failed=1, cancelled=130
// (128+SIGINT), timeout=124 (the `timeout(1)` convention).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tgcz2011/aiclibridge/internal/api"
	"github.com/tgcz2011/aiclibridge/internal/config"
	"github.com/tgcz2011/aiclibridge/internal/detect"
	"github.com/tgcz2011/aiclibridge/internal/facade"
	"github.com/tgcz2011/aiclibridge/internal/store"
	"github.com/tgcz2011/aiclibridge/pkg/protocol"
)

// cli bundles the in-process stack shared by the local verbs (run /
// agents / models): config, logger, an in-memory SQLite store, the
// detected catalog, and the wired facade. It is constructed by newCLI
// and torn down by close. A zero cli is NOT usable — always construct
// via newCLI.
type cli struct {
	cfg     *config.Config
	logger  *slog.Logger
	store   *store.Store
	catalog []detect.CLIInfo
	facade  *facade.Facade
}

// newCLI loads config + logger + an in-memory store + the detected
// catalog + the facade. The store is ":memory:" because the local
// verbs do not need persistence — a one-shot `run` invocation should
// not leave an aiclibridge.db file behind, and `agents` / `models`
// never touch the store at all. The caller MUST defer c.close() so the
// facade's forwarder goroutines and the SQLite handle are released.
//
// Detect failures are non-fatal: a discovery error falls back to
// detect.DefaultCatalog so `run` can still proceed against an
// explicitly-named model even on a host where the version probes
// misbehaved. This mirrors serve's behaviour so the two modes agree.
//
// debug controls the logger verbosity for the local verbs. Local
// commands are quiet by default (LevelError) so the detect INFO/WARN
// chatter and catalog summary do not clutter user-facing output — pass
// debug=true (from `run`/`agents`/`models --debug`) to bump to
// LevelDebug and re-enable the catalog summary. The daemon
// (serve/start) is unaffected: it builds its own logger from
// cfg.LogLevel (default "info") so operators still get INFO logs in the
// daemon log file.
func newCLI(configPath string, debug bool) (*cli, error) {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return nil, err
	}
	// Local commands are quiet by default; cfg.LogLevel is intentionally
	// ignored here so a daemon-oriented "info" config does not spam a
	// one-shot `run` / `agents` / `models` invocation.
	level := "error"
	if debug {
		level = "debug"
	}
	logger := setupLogger(level)

	// In-memory store: run / agents / models never read back a prior
	// run, and a one-shot invocation must not litter the cwd with a
	// SQLite file. ":memory:" is per-connection and store.Open caps
	// conns to 1, so the schema applies to the single in-memory DB.
	st, err := store.Open(":memory:")
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	ctx := context.Background()
	catalog, err := detect.Discover(ctx)
	if err != nil {
		logger.Warn("detect failed, using default catalog", "error", err)
		catalog = detect.DefaultCatalog()
	}
	// The catalog summary is debug-only diagnostics; with the default
	// LevelError logger it would be silenced anyway, but guard it so the
	// intent is explicit and a future level change cannot reintroduce
	// the noise.
	if debug {
		logCatalogSummary(logger, catalog)
	}

	fc, err := facade.New(cfg, st, catalog, logger)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("build facade: %w", err)
	}

	return &cli{
		cfg:     cfg,
		logger:  logger,
		store:   st,
		catalog: catalog,
		facade:  fc,
	}, nil
}

// close releases the facade (cancelling any in-flight runs and waiting
// for their forwarder goroutines to finalise) and then the SQLite
// handle. Errors are logged at warn level — the caller is exiting and
// the more informative error (if any) was already reported. close is
// idempotent in practice because the facade's Close cancels every live
// run; calling it twice is a cheap no-op.
func (c *cli) close() {
	if err := c.facade.Close(); err != nil {
		c.logger.Warn("facade close error", "error", err)
	}
	if err := c.store.Close(); err != nil {
		c.logger.Warn("store close error", "error", err)
	}
}

// loadConfig resolves and loads the config file, applies env overrides,
// and validates it. It is the shared front-end for every subcommand
// that needs cfg: newCLI (for the in-process stack), runServe (for the
// persistent daemon), and runCancel / runGet (which only need Listen +
// APIKey to talk to a running daemon). A nil cfg from this function is
// never returned — an error is returned instead.
func loadConfig(configPath string) (*config.Config, error) {
	resolved, err := config.ResolveConfigPath(configPath)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	cfg, err := config.Load(resolved)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

// setupLogger builds a slog.Logger writing JSON to stderr at the
// requested level. An unknown level falls back to info so a typo never
// silences the daemon. stderr (not stdout) is intentional: stdout is
// reserved for user-facing output — `run` streams model text there and
// `models` / `agents` print their listings there — so logging to stdout
// would pollute parseable output. The daemon redirects both stdout and
// stderr to its log file (see logFilePath), so daemon logs still land in
// the same place; only the local verbs benefit from the clean stdout.
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
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}

// logCatalogSummary logs one line per detected CLI and a final summary.
// Available CLIs are logged at info; missing ones at warn so operators
// can spot a host missing an expected CLI at a glance. This is the same
// helper serve used in the original main.go — hoisted here so newCLI
// and runServe share the exact same diagnostic output.
func logCatalogSummary(logger *slog.Logger, catalog []detect.CLIInfo) {
	available := 0
	for _, cli := range catalog {
		if cli.Available {
			available++
			logger.Info("detected CLI",
				"name", cli.Name,
				"version", cli.Version,
				"path", cli.Path,
				"providers", len(cli.Providers),
			)
		} else {
			logger.Warn("CLI not available", "name", cli.Name)
		}
	}
	logger.Info("catalog summary", "total", len(catalog), "available", available)
}

// ── runServe ──

// runServe is the original daemon entry point, parameterised by
// --config and --listen. It parses flags, loads config, honours the
// --listen override, then delegates to serveStack for the server
// lifecycle (data dir → SQLite store → detect → facade → HTTP server →
// signal-driven graceful shutdown). runServe is foreground-only: it
// does NOT write a pid file — that is runDaemonForeground's job.
//
// Returns the process exit code so main.go can os.Exit with it; the
// function never calls os.Exit itself so deferred cleanups run.
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file (default: search order)")
	listenOverride := fs.String("listen", "", "listen address (default: from config)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge serve: %v\n", err)
		return 1
	}
	if *listenOverride != "" {
		cfg.Listen = *listenOverride
	}

	logger := setupLogger(cfg.LogLevel)
	return serveStack(cfg, logger, "serve")
}

// serveStack builds the (data dir → SQLite store → detect → facade →
// HTTP server) stack and runs it until a signal is received or the
// server fails, returning the exit code. It is the shared core of
// runServe (foreground, no pid file) and runDaemonForeground (daemon
// child, pid file managed by the caller). The verb ("serve" or "start")
// is used in error messages so the user sees the right prefix.
//
// The shutdown order is preserved from the original implementations:
// HTTP first (reject new requests, drain short in-flight) → facade
// (cancel running runs) → store (release SQLite handles). A
// ListenAndServe failure (port in use, bind error) is fatal; runtime
// request errors are handled by the API layer and never reach here.
//
// ReadHeaderTimeout caps slowloris; no Read / Write timeout because SSE
// streams and long-running runs must hold connections open indefinitely.
func serveStack(cfg *config.Config, logger *slog.Logger, verb string) int {
	// Data dir + SQLite store. MkdirAll is idempotent and ensures the
	// pid file's parent (in daemon mode) exists even if a user wiped
	// the data dir between the parent's loadConfig and the child's
	// startup.
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge %s: mkdir data dir: %v\n", verb, err)
		return 1
	}
	dbPath := filepath.Join(cfg.DataDir, "aiclibridge.db")
	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge %s: open store: %v\n", verb, err)
		return 1
	}

	// Detect CLIs. Best effort: a discovery failure is logged and
	// falls back to the hardcoded catalog so the daemon still starts.
	ctx := context.Background()
	catalog, err := detect.Discover(ctx)
	if err != nil {
		logger.Warn("detect failed, using default catalog", "error", err)
		catalog = detect.DefaultCatalog()
	}
	logCatalogSummary(logger, catalog)

	// Facade — wires adapters to the store + catalog.
	fc, err := facade.New(cfg, st, catalog, logger)
	if err != nil {
		_ = st.Close()
		fmt.Fprintf(os.Stderr, "aiclibridge %s: build facade: %v\n", verb, err)
		return 1
	}

	// HTTP server. ReadHeaderTimeout caps slowloris; no Read / Write
	// timeout because SSE streams and long-running runs must hold
	// connections open indefinitely.
	srv := api.NewServer(fc, cfg, logger)
	httpSrv := &http.Server{
		Addr:              srv.ListenAddr(),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Run + wait for signal. A ListenAndServe failure (port in use,
	// bind error) is fatal; runtime request errors are handled by the
	// API layer and never reach here.
	errCh := make(chan error, 1)
	go func() {
		logger.Info("aiclibridge listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// Best-effort background update check: logs a one-line hint to the
	// daemon log if a newer release is available. Non-blocking and
	// silent on failure — startup must not depend on GitHub.
	maybeAsyncUpdateCheck(func(format string, args ...any) {
		logger.Info("update check", "message", fmt.Sprintf(format, args...))
	})

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		// Server failed to serve — shut down best-effort and exit.
		logger.Error("http server failed", "error", err)
		_ = fc.Close()
		_ = st.Close()
		return 1
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
		return 0
	}
}

// ── runRun ──

// runRun is the one-shot CLI invocation: load the in-process stack,
// start a run, stream / aggregate its events to stdout+stderr, and exit
// with a status-derived code. It does NOT depend on a running daemon —
// the facade is constructed locally with an in-memory store, the
// adapter subprocess is spawned directly, and the process exits when
// the run terminates.
//
// Exit codes (exitCodeForStatus): completed=0, failed=1,
// cancelled=130, timeout=124. A SIGINT / SIGTERM mid-run cancels the
// run's context, which propagates to the adapter; the forwarder then
// emits a terminal cancelled result so the stream loop unblocks
// cleanly rather than wedging on a dead adapter.
func runRun(args []string) int {
	// Split at the first standalone "--": everything after is passed
	// verbatim to the underlying CLI as CustomArgs (e.g. opencode's
	// --pure to disable plugins). Go's flag package treats "--" as a
	// terminator but folds the tail into positional args, which would
	// pollute the prompt — so we split first and parse flags only from
	// the head.
	flagArgs, customArgs := splitCustomArgs(args)

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file (default: search order)")
	model := fs.String("model", "", "CLI/provider/model routing key (default: first enabled agent)")
	cwd := fs.String("cwd", "", "working directory for the agent subprocess (default: inherit)")
	systemPrompt := fs.String("system-prompt", "", "developer/system instruction (ignored by process-stdin CLIs)")
	maxTurns := fs.Int("max-turns", 0, "cap the agent's turn count (0 = unlimited)")
	timeoutDur := fs.Duration("timeout", 0, "hard wall-clock timeout (e.g. 30s, 2m; 0 = none)")
	resume := fs.String("resume", "", "session id to resume")
	noStream := fs.Bool("no-stream", false, "disable live streaming; print aggregated output at the end")
	debug := fs.Bool("debug", false, "enable debug logging (shows detect/catalog logs)")
	fs.BoolVar(debug, "d", false, "shorthand for --debug")
	if err := fs.Parse(flagArgs); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	prompt, err := collectPrompt(fs.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge run: %v\n", err)
		return 2
	}
	if prompt == "" {
		fmt.Fprintln(os.Stderr, "aiclibridge run: prompt is required (positional args or piped stdin)")
		return 2
	}

	c, err := newCLI(*configPath, *debug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge run: %v\n", err)
		return 1
	}
	defer c.close()

	req := facade.RunRequest{
		Model:           *model,
		Prompt:          prompt,
		Cwd:             *cwd,
		SystemPrompt:    *systemPrompt,
		MaxTurns:        *maxTurns,
		ResumeSessionID: *resume,
		CustomArgs:      customArgs,
		// Stream=true so we own the Events drain; the live-vs-aggregate
		// decision is made below by which consumer we attach.
		Stream: true,
	}
	if *timeoutDur > 0 {
		req.TimeoutMs = int64((*timeoutDur).Milliseconds())
	}

	// SIGINT / SIGTERM cancel the run's context, which propagates to
	// the adapter. A second SIGINT (the user mashing Ctrl-C) falls
	// through to the default disposition and kills the process — the
	// facade's recover path still finalises the store because the
	// forwarder's defers run on goroutine exit, not process exit.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	handle, err := c.facade.StartRun(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge run: %v\n", err)
		return 1
	}

	var status string
	if *noStream {
		status = drainAndSummarize(handle, os.Stdout, os.Stderr)
	} else {
		status = streamEvents(handle, os.Stdout, os.Stderr)
	}
	return exitCodeForStatus(status)
}

// ── runAgents / runModels ──

// runAgents lists every detected CLI with its availability, version,
// resolved path, provider count, and the full provider/model tree
// beneath it. The output is tab-delimited for the CLI summary line and
// indented `  cli/provider/model` for each model, so it is both
// human-scannable and trivially greppable. Local detect — no daemon.
func runAgents(args []string) int {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file (default: search order)")
	debug := fs.Bool("debug", false, "enable debug logging (shows detect/catalog logs)")
	fs.BoolVar(debug, "d", false, "shorthand for --debug")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	c, err := newCLI(*configPath, *debug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge agents: %v\n", err)
		return 1
	}
	defer c.close()

	agents, err := c.facade.ListAgents(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge agents: %v\n", err)
		return 1
	}

	for _, cli := range agents {
		avail := "no"
		if cli.Available {
			avail = "yes"
		}
		fmt.Fprintf(os.Stdout, "%s\tavailable=%s\tversion=%s\tpath=%s\tproviders=%d\n",
			cli.Name, avail, cli.Version, cli.Path, len(cli.Providers))
		for _, p := range cli.Providers {
			for _, m := range p.Models {
				fmt.Fprintf(os.Stdout, "  %s/%s/%s\n", cli.Name, p.Name, m.Name)
			}
		}
	}
	return 0
}

// runModels lists every CLI/provider/model routing key, one per line.
// It is the flat form of runAgents: no availability / version metadata,
// just the bare identifiers a user can paste into `--model`. Local
// detect — no daemon. The order matches supportedCLIs so the output is
// stable across hosts and runs.
func runModels(args []string) int {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file (default: search order)")
	debug := fs.Bool("debug", false, "enable debug logging (shows detect/catalog logs)")
	fs.BoolVar(debug, "d", false, "shorthand for --debug")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	c, err := newCLI(*configPath, *debug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge models: %v\n", err)
		return 1
	}
	defer c.close()

	agents, err := c.facade.ListAgents(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge models: %v\n", err)
		return 1
	}

	for _, cli := range agents {
		for _, p := range cli.Providers {
			for _, m := range p.Models {
				fmt.Fprintf(os.Stdout, "%s/%s/%s\n", cli.Name, p.Name, m.Name)
			}
		}
	}
	return 0
}

// ── runCancel / runGet ──

// runCancel cancels a running run via the daemon's HTTP API. It does
// NOT spin up a local facade — it loads config (for Listen + APIKey),
// honours an optional --listen override, and POSTs
// /v1/runs/{id}/cancel. This means cancel only works when a daemon is
// already running; a missing daemon surfaces as a connection error.
// The run id is the first positional arg and is required.
func runCancel(args []string) int {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file (default: search order)")
	listenOverride := fs.String("listen", "", "daemon listen address (default: from config)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "aiclibridge cancel: run-id is required")
		fmt.Fprintln(os.Stderr, "usage: aiclibridge cancel <run-id> [--config <path>] [--listen <addr>]")
		return 2
	}
	runID := fs.Arg(0)

	addr, apiKey, err := resolveDaemonAddr(*configPath, *listenOverride)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge cancel: %v\n", err)
		return 1
	}

	reqURL := fmt.Sprintf("http://%s/v1/runs/%s/cancel", addr, url.PathEscape(runID))
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, reqURL, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge cancel: %v\n", err)
		return 1
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge cancel: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "aiclibridge cancel: HTTP %d: %s\n", resp.StatusCode, bytes.TrimSpace(body))
		return 1
	}
	os.Stdout.Write(body)
	if len(body) > 0 && !bytes.HasSuffix(body, []byte("\n")) {
		fmt.Fprintln(os.Stdout)
	}
	return 0
}

// runGet fetches a run's stored history via the daemon's HTTP API. Like
// cancel it depends on a running daemon. The response is JSON; if it
// parses cleanly it is re-indented for readability, otherwise the raw
// body is printed verbatim. The run id is the first positional arg and
// is required.
func runGet(args []string) int {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file (default: search order)")
	listenOverride := fs.String("listen", "", "daemon listen address (default: from config)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "aiclibridge get: run-id is required")
		fmt.Fprintln(os.Stderr, "usage: aiclibridge get <run-id> [--config <path>] [--listen <addr>]")
		return 2
	}
	runID := fs.Arg(0)

	addr, apiKey, err := resolveDaemonAddr(*configPath, *listenOverride)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge get: %v\n", err)
		return 1
	}

	reqURL := fmt.Sprintf("http://%s/v1/runs/%s", addr, url.PathEscape(runID))
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, reqURL, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge get: %v\n", err)
		return 1
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiclibridge get: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "aiclibridge get: HTTP %d: %s\n", resp.StatusCode, bytes.TrimSpace(body))
		return 1
	}
	// Pretty-print JSON when valid so a human can scan it; fall back to
	// the raw body if the daemon returned something non-JSON (e.g. an
	// empty 200 or a text/plain diagnostic).
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err == nil {
		fmt.Fprintln(os.Stdout, pretty.String())
	} else {
		os.Stdout.Write(body)
		if len(body) > 0 && !bytes.HasSuffix(body, []byte("\n")) {
			fmt.Fprintln(os.Stdout)
		}
	}
	return 0
}

// resolveDaemonAddr loads config and returns (listen addr, api key)
// for the daemon a cancel/get subcommand should talk to. The optional
// listenOverride wins over cfg.Listen so an operator can point at a
// non-default daemon without editing config. Used only by the HTTP-
// based verbs; the local verbs (run/agents/models) talk to the facade
// in-process and never need this.
func resolveDaemonAddr(configPath, listenOverride string) (addr, apiKey string, err error) {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return "", "", err
	}
	addr = cfg.Listen
	if listenOverride != "" {
		addr = listenOverride
	}
	return addr, cfg.APIKey, nil
}

// ── runVersion ──

// runVersion prints the version banner to stdout and exits 0. It
// ignores its args — v0.1.0 has no version-specific flags. The actual
// formatting lives in version.go:printVersion so tests can capture the
// output without redirecting os.Stdout.
func runVersion(args []string) int {
	printVersion(os.Stdout)
	return 0
}

// ── Helpers ──

// collectPrompt assembles the user prompt from positional args or
// stdin. Positional args are joined with a single space ("hello world"
// from ["hello", "world"]). When no positional args are present and
// stdin is not a TTY (i.e. something is piped in), stdin is read in
// full and trimmed of surrounding whitespace. When no positional args
// and stdin IS a TTY, the empty string is returned — runRun treats an
// empty prompt as a usage error.
//
// The TTY check uses os.Stdin.Stat() + os.ModeCharDevice rather than
// pulling in golang.org/x/term or mattn/go-isatty: both would work,
// but ModeCharDevice is stdlib-only and sufficient for the "is there a
// pipe here?" decision the CLI needs to make.

// splitCustomArgs separates args at the first standalone "--" into flag
// args (before) and custom CLI args (after). The "--" itself is consumed.
// Returns the original args unchanged (with nil customArgs) if no "--" is
// present. This lets `run` forward extra flags to the underlying CLI,
// e.g. `aiclibridge run --model opencode/... "fix bug" -- --pure` passes
// "--pure" to opencode. A "--" that appears as the very first token is
// legal: flagArgs is empty and the prompt is read from stdin.
func splitCustomArgs(args []string) (flagArgs, customArgs []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func collectPrompt(positional []string) (string, error) {
	if len(positional) > 0 {
		return strings.Join(positional, " "), nil
	}
	fi, err := os.Stdin.Stat()
	if err != nil {
		return "", fmt.Errorf("stat stdin: %w", err)
	}
	// A char device (terminal) means no pipe — return empty so the
	// caller can surface a "prompt required" error. A pipe or regular
	// file means content was piped in; read it.
	if fi.Mode()&os.ModeCharDevice != 0 {
		return "", nil
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// printEvent writes a single protocol.Event to w in a human-friendly
// form. text and thinking content are written raw (no prefix) so they
// can be piped to the next tool when routed to stdout; every other
// event type gets a `[type]` prefix and a single trailing newline so
// the stderr stream stays line-oriented and greppable. The result event
// carries status / duration_ms / session_id / error so a user can see
// at a glance how the run ended without parsing JSON.
//
// printEvent is the single formatter used by both streamEvents (live)
// and drainAndSummarize (terminal summary) so the two modes agree on
// the rendering of every event type.
func printEvent(e protocol.Event, w io.Writer) {
	switch e.Type {
	case protocol.EventText:
		fmt.Fprint(w, e.Content)
	case protocol.EventThinking:
		fmt.Fprint(w, e.Content)
	case protocol.EventToolUse:
		fmt.Fprintf(w, "[tool_use] tool=%s call_id=%s input=%s\n",
			e.Tool, e.CallID, string(e.Input))
	case protocol.EventToolResult:
		fmt.Fprintf(w, "[tool_result] tool=%s call_id=%s output=%s\n",
			e.Tool, e.CallID, e.Output)
	case protocol.EventStatus:
		fmt.Fprintf(w, "[status] %s", e.Status)
		if e.SessionID != "" {
			fmt.Fprintf(w, " session_id=%s", e.SessionID)
		}
		fmt.Fprintln(w)
	case protocol.EventError:
		fmt.Fprintf(w, "[error] %s\n", e.Content)
	case protocol.EventLog:
		fmt.Fprintf(w, "[log:%s] %s\n", e.Level, e.Content)
	case protocol.EventResult:
		if e.Result != nil {
			fmt.Fprintf(w, "[result] status=%s duration_ms=%d",
				e.Result.Status, e.Result.DurationMs)
			if e.Result.SessionID != "" {
				fmt.Fprintf(w, " session_id=%s", e.Result.SessionID)
			}
			if e.Result.Error != "" {
				fmt.Fprintf(w, " error=%q", e.Result.Error)
			}
			fmt.Fprintln(w)
		}
	default:
		fmt.Fprintf(w, "[%s] %s\n", e.Type, e.Content)
	}
}

// exitCodeForStatus maps a run's terminal status to a process exit
// code. The mapping follows the conventions a shell user expects:
//
//   - completed → 0 (success)
//   - failed    → 1 (generic failure)
//   - cancelled → 130 (128 + SIGINT(2); mirrors what bash sets $? to
//     when a process is killed by Ctrl-C)
//   - timeout   → 124 (the timeout(1) convention; distinct from
//     failed so a CI step can branch on "ran out of time" vs "broke")
//   - unknown   → 1 (treat as failed; the facade always emits a known
//     status, so this only fires if the stream closed without one)
func exitCodeForStatus(status string) int {
	switch status {
	case "completed":
		return 0
	case "cancelled":
		return 130
	case "timeout":
		return 124
	case "failed":
		return 1
	default:
		return 1
	}
}

// streamEvents is the live consumer for `run` (default mode). It reads
// handle.Events until it closes, routing text + thinking content to
// stdout (raw, no prefix) and every other event type to stderr via
// printEvent. The terminal EventResult supplies the returned status;
// if no result event was seen (a facade contract violation) the
// function falls back to "completed" so a clean close still maps to
// exit 0 — mirroring buildRunResult's default in the API layer.
func streamEvents(handle *facade.RunHandle, stdout, stderr io.Writer) string {
	var status string
	for ev := range handle.Events {
		switch ev.Type {
		case protocol.EventText, protocol.EventThinking:
			printEvent(ev, stdout)
		case protocol.EventResult:
			printEvent(ev, stderr)
			if ev.Result != nil {
				status = ev.Result.Status
			}
		default:
			printEvent(ev, stderr)
		}
	}
	if status == "" {
		status = "completed"
	}
	return status
}

// drainAndSummarize is the non-streaming consumer for `run --no-stream`.
// It reads every event into a slice (no live output), then extracts the
// terminal ResultPayload for status / output / session_id, prints the
// aggregated output to stdout, and writes a single `[result]` summary
// line to stderr. If the terminal result had no Output, text events
// are concatenated as a fallback (mirroring aggregateText in the API
// layer) so a run that emitted text but no terminal output still
// yields its content.
func drainAndSummarize(handle *facade.RunHandle, stdout, stderr io.Writer) string {
	var events []protocol.Event
	for ev := range handle.Events {
		events = append(events, ev)
	}

	var status, output, sessionID, errMsg string
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == protocol.EventResult && events[i].Result != nil {
			r := events[i].Result
			status = r.Status
			output = r.Output
			sessionID = r.SessionID
			errMsg = r.Error
			break
		}
	}
	// Fallback: concatenate text events when no terminal Output.
	if output == "" {
		var sb strings.Builder
		for _, ev := range events {
			if ev.Type == protocol.EventText {
				sb.WriteString(ev.Content)
			}
		}
		output = sb.String()
	}
	if status == "" {
		status = "completed"
	}

	if output != "" {
		fmt.Fprint(stdout, output)
		if !strings.HasSuffix(output, "\n") {
			fmt.Fprintln(stdout)
		}
	}
	// Reuse printEvent's result formatter so the summary line matches
	// the live stream's rendering exactly.
	resultEv := protocol.Event{Type: protocol.EventResult, Result: &protocol.ResultPayload{
		Status: status, Output: output, SessionID: sessionID, Error: errMsg,
	}}
	printEvent(resultEv, stderr)
	return status
}
