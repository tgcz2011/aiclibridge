// Package facade is part of aiclibridge.
//
// This file hosts the orchestration layer that wires the HTTP API to the
// per-CLI adapters. The Facade owns run lifecycle: it routes a RunRequest to
// the right adapter.Backend by parsing the `CLI/provider/model` routing key,
// spawns the adapter session in its own goroutine, aggregates the adapter's
// Message stream into a single protocol.Event channel with monotonically
// increasing sequence numbers, persists every event + the terminal result to
// the store, and exposes cancellation + history replay to the API layer.
//
// # Concurrency model
//
// There is NO global concurrency cap. Each run executes in its own goroutine
// (decision 6: "designed for a large number of calls"). The Facade tracks
// live runs in a sync.Map so CancelRun and Close can reach them without
// holding a mutex. The store is the only shared mutable state and modernc.org/
// sqlite serialises writes at the connection level.
//
// # Fault isolation
//
// A single CLI failure NEVER affects the Facade or other runs:
//   - adapter.Execute is wrapped in a panic-recover that converts a panic
//     into an error, so a buggy adapter cannot crash the daemon.
//   - The event-forwarding goroutine has its own panic-recover that logs the
//     panic, emits a terminal EventResult with status=failed, and closes the
//     Events channel so the caller unblocks.
//   - Store write failures (CreateRun/AppendEvent/FinishRun) are logged as
//     warnings and never abort a run — the store is a persistence helper, not
//     the source of truth. The adapter.Session is the source of truth.
//   - The Events channel is buffered (256). Intermediate events use trySend
//     (non-blocking; a full buffer drops the event from the live stream but the
//     store still has it for replay). The terminal EventResult uses a blocking
//     send with a timeout fallback so a dead consumer cannot wedge the
//     forwarder forever.
package facade

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tgcz2011/aiclibridge/internal/adapter"
	"github.com/tgcz2011/aiclibridge/internal/config"
	"github.com/tgcz2011/aiclibridge/internal/detect"
	"github.com/tgcz2011/aiclibridge/internal/store"
	"github.com/tgcz2011/aiclibridge/pkg/protocol"
)

// eventsChanBuffer caps the live event stream per run. v0.3 raises this
// from 256 to 1024 so a fast adapter (e.g. claude streaming a long file)
// paired with a momentarily slow SSE consumer (e.g. a proxy buffering)
// does not drop intermediate events from the live stream. The store
// still receives every event for replay regardless of buffer state.
const eventsChanBuffer = 1024

// resultWaitTimeout bounds how long the forwarder waits for the adapter to
// deliver its single Result value after the Messages channel closes. A
// well-behaved adapter sends Result immediately after closing Messages; this
// timeout catches a buggy adapter that closes Messages but never sends Result
// so the forwarder never wedges.
const resultWaitTimeout = 30 * time.Second

// terminalSendTimeout bounds the blocking send for the terminal EventResult.
// A streaming caller is expected to drain Events until close; a non-streaming
// caller has a drain goroutine started by the Facade. This timeout is the last
// line of defence against a consumer that vanished (e.g. HTTP client gone) so
// the forwarder goroutine is never leaked forever. The store already holds the
// event, so dropping it from the live stream only affects the (already gone)
// consumer.
const terminalSendTimeout = 30 * time.Second

// Facade is the orchestration layer between HTTP API and adapters. It is
// safe for concurrent use: adapters are built once at construction and never
// mutated; live runs are tracked in a sync.Map; the store serialises its own
// writes. A zero Facade is NOT usable — always construct via New.
type Facade struct {
	// cfg is the daemon configuration retained for per-run ExecOptions
	// assembly (ExtraArgs, MCPConfig, ThinkingLevel, OpenclawMode come from
	// the per-agent AgentConfig at run time, not at Backend construction).
	cfg *config.Config
	// adapters maps CLI name ("claude", "codex", "opencode", "openclaw")
	// to its Backend. Built once in New from cfg.Agents[cli].Enabled; an
	// agent not in this map is "not enabled" and StartRun rejects it.
	adapters map[string]adapter.Backend
	store    *store.Store
	// catalog is the optional cached detect result used by ListAgents /
	// ListProviders. May be empty; the methods fall back to detect.Discover
	// or detect.DefaultCatalog at call time.
	catalog []detect.CLIInfo
	logger  *slog.Logger
	// runs maps runID -> *RunHandle for every live run. A run is removed
	// when its forwarder goroutine exits. Used by CancelRun and Close.
	runs sync.Map
}

// RunHandle is a running run's handle, returned by StartRun for streaming.
// The caller reads Events until it closes (that signals the run is fully
// done — terminal EventResult delivered, store finalised). Cancel cancels
// the run's context, which propagates to the adapter and unblocks Events.
type RunHandle struct {
	ID      string
	Adapter string
	Model   string
	// Events is the unified protocol.Event stream. Closed when the run
	// terminates (after the terminal EventResult is delivered). Every
	// event on this channel has already been persisted to the store.
	Events <-chan protocol.Event
	// Cancel cancels the run's derived context, propagating to the
	// adapter. Safe to call multiple times; a no-op after the first call.
	Cancel context.CancelFunc
	// done is closed by the forwarder goroutine after Events is closed
	// and the store is finalised. Close() waits on this for every live
	// run so the daemon does not exit mid-finalise.
	done chan struct{}
}

// RunRequest is the unified request shape for starting a run. It is the
// API-agnostic form: the HTTP layer decodes its wire type into this and
// hands it to StartRun.
type RunRequest struct {
	// Model is the `CLI/provider/model` routing key. Empty means "use the
	// default agent" (first enabled agent + its first catalog provider/model).
	Model string
	// Prompt is the user's input. Required; an empty prompt is passed to
	// the adapter verbatim (most adapters will reject it, but the Facade
	// does not pre-validate).
	Prompt string
	// Cwd is the working directory for the adapter subprocess. Empty
	// means inherit the daemon's cwd.
	Cwd string
	// SystemPrompt is the developer/system instruction. Adapters that
	// cannot honour it (process-stdin CLIs) ignore it.
	SystemPrompt string
	// MaxTurns caps the agent's turn count. 0 means unlimited.
	MaxTurns int
	// TimeoutMs imposes a hard wall-clock deadline on the run. 0 means
	// no hard timeout (decision: a session that keeps emitting events is
	// never killed merely for running long).
	TimeoutMs int64
	// ResumeSessionID, if non-empty, asks the adapter to resume a prior
	// CLI session.
	ResumeSessionID string
	// CustomArgs are per-run CLI arguments appended after the agent's
	// configured CustomArgs (req wins on conflicting flags).
	CustomArgs []string
	// CustomEnv are per-run environment overrides. NOTE: the current
	// adapter.ExecOptions has no Env field; per-run env override is a
	// known limitation. cfg.Agents[cli].Env is applied at Backend
	// construction time. This field is accepted for API completeness
	// and will be honoured once the adapter interface grows an Env slot.
	CustomEnv map[string]string
	// Stream controls the live event channel. true (default) means the
	// caller drains Events. false means the Facade starts an internal
	// drain goroutine so the forwarder never blocks on a missing reader;
	// the caller retrieves results via GetRun.
	Stream bool
}

// RunResult is the final outcome for non-streaming callers or history replay.
// For a live run, GetRun assembles this from the store (which is written in
// real time by the forwarder).
type RunResult struct {
	ID         string
	Status     string
	Output     string
	Error      string
	DurationMs int64
	SessionID  string
	Usage      map[string]protocol.TokenUsagePayload
	// Events is the full event timeline, populated only by GetRun (replay
	// from the store). StartRun does not populate this — use the RunHandle
	// Events channel for live streaming.
	Events []protocol.Event
}

// New constructs a Facade from the daemon config. For every agent in
// cfg.Agents with Enabled=true, it instantiates the corresponding
// adapter.Backend via adapter.New. Disabled agents are absent from the
// returned Facade's adapter map and StartRun will reject them.
//
// catalog may be empty; ListAgents / ListProviders fall back to
// detect.Discover or detect.DefaultCatalog at call time.
func New(cfg *config.Config, store *store.Store, catalog []detect.CLIInfo, logger *slog.Logger) (*Facade, error) {
	return NewWithBackends(cfg, store, catalog, logger, nil)
}

// NewWithBackends is the testable constructor. It accepts a pre-built
// backends map so tests can inject stub/panic/error backends without going
// through adapter.New (which returns real CLI-spawning backends). When
// backends is non-nil, it is used verbatim and cfg.Agents is only consulted
// for per-run ExecOptions assembly. When backends is nil, the production
// path runs: adapter.New is called for every enabled agent.
//
// This is exported so tests in other packages can construct a Facade with
// fake backends; production code uses New.
func NewWithBackends(cfg *config.Config, store *store.Store, catalog []detect.CLIInfo, logger *slog.Logger, backends map[string]adapter.Backend) (*Facade, error) {
	if cfg == nil {
		return nil, errors.New("facade: config must not be nil")
	}
	if store == nil {
		return nil, errors.New("facade: store must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}

	f := &Facade{
		cfg:      cfg,
		store:    store,
		catalog:  catalog,
		logger:   logger,
		adapters: make(map[string]adapter.Backend),
	}

	if backends != nil {
		for name, b := range backends {
			f.adapters[name] = b
		}
	} else {
		for _, name := range config.KnownAgents {
			agent, ok := cfg.Agents[name]
			if !ok || !agent.Enabled {
				continue
			}
			b, err := adapter.New(name, adapter.Config{
				ExecutablePath: agent.ExecutablePath,
				Env:            agent.Env,
				Logger:         logger,
			})
			if err != nil {
				return nil, fmt.Errorf("facade: create adapter %q: %w", name, err)
			}
			f.adapters[name] = b
		}
	}

	return f, nil
}

// StartRun begins a run: resolves the routing key, persists the run row,
// invokes the adapter (panic-guarded), and spawns a forwarder goroutine
// that aggregates adapter messages into a protocol.Event stream. The
// returned RunHandle's Events channel is the live stream; close signals
// the run is fully done.
//
// Every error path finalises the run (FinishRun status=failed) and cleans
// up, so the caller never sees a RunHandle for a run that did not start.
func (f *Facade) StartRun(ctx context.Context, req RunRequest) (*RunHandle, error) {
	// 1. Resolve routing key.
	cliName, _, modelName, err := f.resolveRoute(req.Model)
	if err != nil {
		return nil, err
	}

	// 4. Find adapter.
	backend, ok := f.adapters[cliName]
	if !ok {
		return nil, fmt.Errorf("facade: agent %q not enabled", cliName)
	}

	// 2. Generate run ID.
	runID := newRunID()

	// 3. Persist run row. Store failure is non-fatal: the run still
	// executes, just without history. The forwarder will retry store
	// writes per-event.
	storeCtx := context.Background()
	if err := f.store.CreateRun(storeCtx, runID, cliName, modelName, req.Cwd); err != nil {
		f.logger.Error("facade: create run in store (continuing)", "run_id", runID, "error", err)
	}

	// 5. Build ExecOptions from agent config + request overrides.
	agentCfg := f.cfg.Agents[cliName]
	opts := f.buildExecOptions(req, agentCfg, modelName)

	// 6. Derive context with cancel / timeout.
	runCtx, cancel := f.deriveContext(ctx, req.TimeoutMs)

	// 7. Execute (panic-guarded). A panicking adapter returns an error,
	// never crashes the daemon.
	session, err := f.safeExecute(runCtx, cliName, backend, req.Prompt, opts)
	if err != nil {
		cancel()
		f.finishRun(storeCtx, runID, "failed", "", err.Error(), "")
		return nil, err
	}

	// 8. Set up handle + forwarder.
	eventsCh := make(chan protocol.Event, eventsChanBuffer)
	handle := &RunHandle{
		ID:      runID,
		Adapter: cliName,
		Model:   modelName,
		Events:  eventsCh,
		Cancel:  cancel,
		done:    make(chan struct{}),
	}
	f.runs.Store(runID, handle)

	go f.forwardEvents(runCtx, storeCtx, runID, cliName, session, eventsCh, handle)

	// 9. Non-streaming: start a drain so the forwarder's terminal send
	// always has a consumer.
	if !req.Stream {
		go drainEvents(eventsCh)
	}

	return handle, nil
}

// GetRun returns the current state of a run from the store. For a live run,
// the store is written in real time by the forwarder, so this returns the
// partial timeline up to the moment of the call. The Events slice is
// reconstructed from the store's event rows.
func (f *Facade) GetRun(ctx context.Context, id string) (*RunResult, error) {
	run, err := f.store.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := f.store.ListEvents(ctx, id)
	if err != nil {
		return nil, err
	}

	events := make([]protocol.Event, 0, len(rows))
	for _, row := range rows {
		var ev protocol.Event
		if err := json.Unmarshal(row.Payload, &ev); err != nil {
			// A malformed row should not break replay; skip it.
			continue
		}
		if ev.Type == "" {
			ev.Type = protocol.EventType(row.Type)
		}
		events = append(events, ev)
	}

	result := &RunResult{
		ID:        run.ID,
		Status:    run.Status,
		Error:     run.Error,
		SessionID: run.CLISessionID,
		Events:    events,
	}

	// Populate Output / DurationMs / Usage from the terminal EventResult
	// (the last result event in the timeline).
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == protocol.EventResult && events[i].Result != nil {
			r := events[i].Result
			result.Output = r.Output
			result.DurationMs = r.DurationMs
			result.Usage = r.Usage
			break
		}
	}

	return result, nil
}

// GetUsageStats aggregates token usage across runs in the [since, until]
// unix-second window. It is the thin store passthrough backing the
// /v1/stats/usage and /v1/stats/summary endpoints; the api layer prices
// the rows using the catalog → provider mapping and the pricing table.
func (f *Facade) GetUsageStats(ctx context.Context, since, until int64) ([]store.UsageStatRow, error) {
	return f.store.GetUsageStats(ctx, since, until)
}

// CancelRun cancels a live run by ID. It returns an error if the run is not
// found in the live map — this typically means the run already finished (the
// forwarder removed it). The error is informational, not a hard failure.
func (f *Facade) CancelRun(ctx context.Context, id string) error {
	v, ok := f.runs.Load(id)
	if !ok {
		return fmt.Errorf("facade: run %q not found (already finished?)", id)
	}
	handle := v.(*RunHandle)
	handle.Cancel()
	return nil
}

// ListAgents returns the cached catalog, or discovers it if empty. A
// discovery failure is non-fatal: the default catalog is returned so the
// HTTP catalog endpoint always renders something.
func (f *Facade) ListAgents(ctx context.Context) ([]detect.CLIInfo, error) {
	if len(f.catalog) > 0 {
		return f.catalog, nil
	}
	discovered, err := detect.Discover(ctx)
	if err != nil {
		return detect.DefaultCatalog(), nil
	}
	return discovered, nil
}

// ListProviders returns the provider/model catalog for a single CLI. It
// looks up the CLI in the cached catalog (falling back to Discover if the
// cache is empty). An unknown CLI name returns an error.
func (f *Facade) ListProviders(ctx context.Context, cli string) ([]detect.ProviderInfo, error) {
	cat := f.catalog
	if len(cat) == 0 {
		discovered, err := detect.Discover(ctx)
		if err != nil {
			cat = detect.DefaultCatalog()
		} else {
			cat = discovered
		}
	}
	for _, info := range cat {
		if info.Name == cli {
			return info.Providers, nil
		}
	}
	return nil, fmt.Errorf("facade: unknown CLI %q", cli)
}

// Close cancels every live run and waits for all forwarder goroutines to
// finish finalising the store. This ensures the daemon does not exit while a
// run is mid-write. After Close returns, no new runs should be started.
func (f *Facade) Close() error {
	var handles []*RunHandle
	f.runs.Range(func(_, v any) bool {
		h := v.(*RunHandle)
		handles = append(handles, h)
		return true
	})
	for _, h := range handles {
		h.Cancel()
	}
	for _, h := range handles {
		<-h.done
	}
	return nil
}

// ── Internal helpers ──

// resolveRoute parses the model routing key. An empty model resolves to the
// default agent (first enabled agent in config.KnownAgents order whose
// adapter is instantiated). The default agent's first catalog provider/model
// is used; if the catalog has no models for that CLI, provider/model are
// returned empty (the adapter uses its own default).
func (f *Facade) resolveRoute(model string) (cli, provider, modelName string, err error) {
	if model == "" {
		return f.resolveDefault()
	}
	cli, provider, modelName, err = detect.ParseModelName(model)
	if err != nil {
		return "", "", "", err
	}
	return cli, provider, modelName, nil
}

// resolveDefault picks the first enabled agent (in config.KnownAgents order)
// whose adapter is live, and its first catalog provider/model. Returns an
// error if no agent is configured.
func (f *Facade) resolveDefault() (cli, provider, modelName string, err error) {
	for _, name := range config.KnownAgents {
		if _, ok := f.adapters[name]; !ok {
			continue
		}
		cat := f.catalog
		if len(cat) == 0 {
			cat = detect.DefaultCatalog()
		}
		for _, info := range cat {
			if info.Name != name {
				continue
			}
			for _, p := range info.Providers {
				if len(p.Models) == 0 {
					continue
				}
				return name, p.Name, p.Models[0].Name, nil
			}
		}
		// Catalog has no models for this CLI; return with empty
		// provider/model so the adapter uses its own default.
		return name, "", "", nil
	}
	return "", "", "", errors.New("facade: no agent configured")
}

// buildExecOptions assembles adapter.ExecOptions from the request and the
// agent's static config. cfg provides ExtraArgs, MCPConfig, ThinkingLevel,
// OpenclawMode, and the base CustomArgs; req provides per-run overrides
// (CustomArgs appended after cfg's so req wins on conflicting flags).
func (f *Facade) buildExecOptions(req RunRequest, agent config.AgentConfig, modelName string) adapter.ExecOptions {
	opts := adapter.ExecOptions{
		Cwd:            req.Cwd,
		Model:          modelName,
		SystemPrompt:   req.SystemPrompt,
		MaxTurns:       req.MaxTurns,
		ResumeSessionID: req.ResumeSessionID,
		ExtraArgs:      agent.ExtraArgs,
		McpConfig:      agent.MCPConfig.Raw(),
		ThinkingLevel:  agent.ThinkingLevel,
		OpenclawMode:   agent.OpenclawMode,
	}

	// Merge CustomArgs: cfg base first, req overrides last.
	opts.CustomArgs = make([]string, 0, len(agent.CustomArgs)+len(req.CustomArgs))
	opts.CustomArgs = append(opts.CustomArgs, agent.CustomArgs...)
	opts.CustomArgs = append(opts.CustomArgs, req.CustomArgs...)

	// Timeout: req wins; 0 means no hard timeout.
	if req.TimeoutMs > 0 {
		opts.Timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}

	return opts
}

// deriveContext wraps the caller's context with cancel (or timeout if
// TimeoutMs > 0). The returned CancelFunc is stored on the RunHandle so
// CancelRun can abort the run. A 0 TimeoutMs yields a pure cancel context
// (no hard deadline) so a long-running session that keeps emitting events
// is never killed merely for running long.
func (f *Facade) deriveContext(ctx context.Context, timeoutMs int64) (context.Context, context.CancelFunc) {
	if timeoutMs > 0 {
		return context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	}
	return context.WithCancel(ctx)
}

// safeExecute wraps backend.Execute in a panic recover so a buggy adapter
// cannot crash the daemon. A panic is converted to an error and returned;
// the caller (StartRun) finalises the run as failed.
func (f *Facade) safeExecute(ctx context.Context, cli string, backend adapter.Backend, prompt string, opts adapter.ExecOptions) (session *adapter.Session, err error) {
	defer func() {
		if r := recover(); r != nil {
			f.logger.Error("facade: adapter execute panicked", "adapter", cli, "panic", r)
			err = fmt.Errorf("adapter %s panicked: %v", cli, r)
			session = nil
		}
	}()
	return backend.Execute(ctx, prompt, opts)
}

// forwardEvents is the per-run goroutine that bridges adapter.Session to the
// Facade's protocol.Event stream and the store. It:
//  1. Reads each Message from session.Messages, converts to protocol.Event
//     with an incrementing seq, persists to the store, and trySend to Events.
//  2. Reads the single Result from session.Result.
//  3. Emits the terminal EventResult (blocking send with timeout fallback).
//  4. Finalises the store (FinishRun + SaveSession).
//
// Panic safety: the entire body is wrapped in a recover. A panic logs an
// error, emits a terminal failed event, and finalises the run — the Events
// channel is always closed so the caller never wedges.
//
// Defer order matters: the recover MUST be registered last (LIFO: runs
// first) so it can still send to eventsCh before the close(eventsCh) defer
// runs. close(eventsCh) is registered second-to-last so it runs after the
// recover but before close(done)/Delete/Cancel.
func (f *Facade) forwardEvents(
	runCtx context.Context,
	storeCtx context.Context,
	runID, cliName string,
	session *adapter.Session,
	eventsCh chan protocol.Event,
	handle *RunHandle,
) {
	seq := 0
	terminalSent := false

	// LIFO defer stack: last registered runs first.
	// Cancel runs last (release context resources).
	defer handle.Cancel()
	// Remove from live map.
	defer f.runs.Delete(runID)
	// Signal Close() that this run is done.
	defer close(handle.done)
	// Close the Events channel (after recover, so recover can still send).
	defer close(eventsCh)
	// Recover: runs FIRST on panic, before close(eventsCh).
	defer func() {
		if r := recover(); r != nil {
			func() {
				defer func() { _ = recover() }() // guard the guard
				f.logger.Error("facade: event forwarder panicked",
					"run_id", runID, "adapter", cliName, "panic", r)
				if !terminalSent {
					errEv := protocol.Event{
						Type:    protocol.EventError,
						Seq:     seq,
						Content: fmt.Sprintf("facade internal error: %v", r),
					}
					f.appendEvent(storeCtx, runID, errEv)
					trySendEvent(eventsCh, errEv)
					seq++

					terminalEv := protocol.Event{
						Type: protocol.EventResult,
						Seq:  seq,
						Result: &protocol.ResultPayload{
							Status: "failed",
							Error:  fmt.Sprintf("facade internal error: %v", r),
						},
					}
					f.appendEvent(storeCtx, runID, terminalEv)
					select {
					case eventsCh <- terminalEv:
					case <-time.After(terminalSendTimeout):
						f.logger.Warn("facade: terminal send timed out after panic",
							"run_id", runID)
					}
					f.finishRun(storeCtx, runID, "failed", "",
						fmt.Sprintf("facade internal error: %v", r), "")
				}
			}()
		}
	}()

	// 1. Forward messages.
	if session.Messages != nil {
		for msg := range session.Messages {
			ev := messageToEvent(msg, seq)
			seq++
			f.appendEvent(storeCtx, runID, ev)
			trySendEvent(eventsCh, ev)
		}
	}

	// 2. Read the final result.
	var res adapter.Result
	if session.Result != nil {
		select {
		case res = <-session.Result:
		case <-time.After(resultWaitTimeout):
			res = adapter.Result{
				Status: "failed",
				Error:  fmt.Sprintf("adapter did not produce result within %s", resultWaitTimeout),
			}
		}
	} else {
		res = adapter.Result{
			Status: "failed",
			Error:  "adapter returned nil result channel",
		}
	}

	status := normalizeStatus(res.Status)

	// 3. Emit terminal event (persist first so store always has it).
	terminalEv := resultToEvent(res, seq)
	f.appendEvent(storeCtx, runID, terminalEv)
	select {
	case eventsCh <- terminalEv:
	case <-time.After(terminalSendTimeout):
		f.logger.Warn("facade: terminal event send timed out; consumer may be gone",
			"run_id", runID, "timeout", terminalSendTimeout)
	}

	// 4. Finalise store. Persist the terminal result event's usage so the
	// stats API can price the run without re-reading the event timeline.
	f.finishRun(storeCtx, runID, status, res.SessionID, res.Error,
		marshalUsage(terminalEv.Result.Usage))
	f.saveSession(storeCtx, runID, cliName, res.SessionID)
	terminalSent = true
}

// appendEvent marshals ev and appends it to the store. Failures are logged
// and swallowed — the store is a persistence helper, not the source of
// truth. A store failure never aborts the event stream.
func (f *Facade) appendEvent(ctx context.Context, runID string, ev protocol.Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		f.logger.Warn("facade: marshal event for store",
			"run_id", runID, "seq", ev.Seq, "error", err)
		return
	}
	if err := f.store.AppendEvent(ctx, runID, ev.Seq, string(ev.Type), data); err != nil {
		f.logger.Warn("facade: append event to store",
			"run_id", runID, "seq", ev.Seq, "error", err)
	}
}

// finishRun marks the run complete in the store, persisting the terminal
// result event's usage (serialised as JSON) so the stats API can price
// the run without re-reading the event timeline. Failures are logged and
// swallowed (the run is already done from the adapter's perspective).
// usageJSON may be empty (e.g. a failed run with no usage).
func (f *Facade) finishRun(ctx context.Context, runID, status, sessionID, errMsg, usageJSON string) {
	if err := f.store.FinishRunWithUsage(ctx, runID, status, sessionID, errMsg, usageJSON); err != nil {
		f.logger.Warn("facade: finish run in store",
			"run_id", runID, "status", status, "error", err)
	}
}

// saveSession persists the CLI session id mapping (for resume). Skipped if
// the adapter did not produce a session id. Failures are logged and
// swallowed.
func (f *Facade) saveSession(ctx context.Context, id, adapter, cliSessionID string) {
	if cliSessionID == "" {
		return
	}
	if err := f.store.SaveSession(ctx, id, adapter, cliSessionID); err != nil {
		f.logger.Warn("facade: save session",
			"id", id, "adapter", adapter, "error", err)
	}
}

// drainEvents consumes and discards every event from ch until it closes.
// Used for non-streaming runs so the forwarder's terminal send always has
// a consumer and never blocks.
func drainEvents(ch <-chan protocol.Event) {
	for range ch {
	}
}

// messageToEvent converts an adapter.Message to a protocol.Event with the
// given sequence number. The Input map (for tool_use) is marshalled to
// json.RawMessage; a marshal failure leaves Input nil rather than panicking.
func messageToEvent(msg adapter.Message, seq int) protocol.Event {
	e := protocol.Event{Seq: seq}
	switch msg.Type {
	case adapter.MessageText:
		e.Type = protocol.EventText
		e.Content = msg.Content
	case adapter.MessageThinking:
		e.Type = protocol.EventThinking
		e.Content = msg.Content
	case adapter.MessageToolUse:
		e.Type = protocol.EventToolUse
		e.Tool = msg.Tool
		e.CallID = msg.CallID
		if msg.Input != nil {
			if data, err := json.Marshal(msg.Input); err == nil {
				e.Input = data
			}
		}
	case adapter.MessageToolResult:
		e.Type = protocol.EventToolResult
		e.Tool = msg.Tool
		e.CallID = msg.CallID
		e.Output = msg.Output
	case adapter.MessageStatus:
		e.Type = protocol.EventStatus
		e.Status = msg.Status
		e.SessionID = msg.SessionID
	case adapter.MessageError:
		e.Type = protocol.EventError
		e.Content = msg.Content
	case adapter.MessageLog:
		e.Type = protocol.EventLog
		e.Level = msg.Level
		e.Content = msg.Content
	default:
		// Unknown message type — surface as a log event so it is not
		// silently lost in replay.
		e.Type = protocol.EventLog
		e.Content = msg.Content
	}
	return e
}

// resultToEvent converts an adapter.Result to the terminal protocol.Event
// with the given sequence number.
func resultToEvent(res adapter.Result, seq int) protocol.Event {
	return protocol.Event{
		Type: protocol.EventResult,
		Seq:  seq,
		Result: &protocol.ResultPayload{
			Status:     normalizeStatus(res.Status),
			Output:     res.Output,
			Error:      res.Error,
			DurationMs: res.DurationMs,
			SessionID:  res.SessionID,
			Usage:      convertUsage(res.Usage),
		},
	}
}

// convertUsage translates adapter.TokenUsage (int64) to
// protocol.TokenUsagePayload (int). Returns nil for an empty map so the
// JSON omits the field.
func convertUsage(in map[string]adapter.TokenUsage) map[string]protocol.TokenUsagePayload {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]protocol.TokenUsagePayload, len(in))
	for k, v := range in {
		out[k] = protocol.TokenUsagePayload{
			InputTokens:      int(v.InputTokens),
			OutputTokens:     int(v.OutputTokens),
			CacheReadTokens:  int(v.CacheReadTokens),
			CacheWriteTokens: int(v.CacheWriteTokens),
		}
	}
	return out
}

// marshalUsage serialises the terminal result event's usage map to JSON
// for store persistence. An empty/nil map yields "" so the store column
// keeps its DEFAULT ''. A marshal failure (should not happen for the
// protocol's own type) also yields "" rather than blocking finalisation.
func marshalUsage(usage map[string]protocol.TokenUsagePayload) string {
	if len(usage) == 0 {
		return ""
	}
	b, err := json.Marshal(usage)
	if err != nil {
		return ""
	}
	return string(b)
}

// normalizeStatus maps adapter status strings to the protocol's canonical
// set. "aborted" (used by some adapters on context cancellation) is folded
// to "cancelled" so the wire form matches the documented EventResult
// status set (completed|failed|cancelled|timeout). An empty status falls
// back to "failed" rather than emitting an empty string.
func normalizeStatus(s string) string {
	switch s {
	case "aborted", "cancelled":
		return "cancelled"
	case "":
		return "failed"
	default:
		return s
	}
}

// trySendEvent performs a non-blocking send on ch. If the channel is full
// the event is dropped from the live stream — the store already holds it
// for replay. This mirrors adapter.trySend's drop-on-full semantics.
func trySendEvent(ch chan<- protocol.Event, ev protocol.Event) {
	select {
	case ch <- ev:
	default:
	}
}

// newRunID generates a 16-byte random hex string (32 chars). crypto/rand
// is used so IDs are unguessable (not just unique). The collision
// probability is negligible (2^128 space); a fallback to a time-based ID
// guards against a catastrophic rand failure.
func newRunID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	// crypto/rand should never fail on a healthy host; this fallback
	// ensures we still return a unique-ish ID if it does.
	return fmt.Sprintf("%016x", time.Now().UnixNano())
}
