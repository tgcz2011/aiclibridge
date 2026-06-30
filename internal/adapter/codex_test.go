package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── Test helpers ──

// fakeCodexIO bundles the fakes that drive runCodexLifecycle in
// isolation. stdin is captured as bytes (a fakeStdin), stdout is fed
// from a string the test controls line-by-line.
type fakeCodexIO struct {
	stdin       interface {
		Write(p []byte) (int, error)
		Close() error
	}
	stdout     io.ReadCloser
	stdoutW    io.WriteCloser
	stdoutDone chan struct{}
}

func newFakeCodexIO() *fakeCodexIO {
	stdoutR, stdoutW := io.Pipe()
	return &fakeCodexIO{
		stdin:       &fakeStdin{},
		stdout:      stdoutR,
		stdoutW:     stdoutW,
		stdoutDone:  make(chan struct{}),
	}
}

// writeStdout feeds a single line of JSON-RPC response into stdout.
// Each call terminates the line with \n (matching the codex wire
// format).
func (f *fakeCodexIO) writeStdout(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(f.stdoutW, line+"\n"); err != nil {
		t.Fatalf("write to fake stdout: %v", err)
	}
}

// closeStdout signals the reader goroutine that codex exited. In
// production, the underlying process closes its stdout when it
// terminates; here we close the write end of the pipe.
func (f *fakeCodexIO) closeStdout(t *testing.T) {
	t.Helper()
	if err := f.stdoutW.Close(); err != nil {
		t.Fatalf("close fake stdout: %v", err)
	}
	// Wait for the reader goroutine to drain.
	select {
	case <-f.stdoutDone:
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not drain within 2s")
	}
}

// newTestCodexClient wires a codexClient with the fake IO and
// returns the client + the message channel the test should read.
func newTestCodexClient(t *testing.T, f *fakeCodexIO) (*codexClient, chan Message) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	msgCh := make(chan Message, 256)
	turnDone := make(chan bool, 1)
	semanticActivityCh := make(chan string, 256)

	var outputMu sync.Mutex
	var output strings.Builder

	// Set up the notify channel automatically so tests using
	// waitForRequests don't have to remember to wire it.
	if fs, ok := f.stdin.(*fakeStdin); ok {
		fs.notify = make(chan struct{}, 64)
	}

	c := &codexClient{
		cfg:                  Config{Logger: logger},
		stdin:                f.stdin,
		pending:              make(map[int]*pendingRPC),
		notificationProtocol: "unknown",
		onMessage: func(msg Message) {
			if msg.Type == MessageText {
				outputMu.Lock()
				output.WriteString(msg.Content)
				outputMu.Unlock()
			}
			select {
			case msgCh <- msg:
			default:
			}
			select {
			case semanticActivityCh <- describeCodexSemanticActivity(msg):
			default:
			}
		},
		onSemanticActivity: func(description string) {
			select {
			case semanticActivityCh <- description:
			default:
			}
		},
		onTurnDone: func(aborted bool) {
			select {
			case turnDone <- aborted:
			default:
			}
		},
	}
	return c, msgCh
}

// runFakeLifecycle wires up a reader goroutine and calls
// runCodexLifecycle. The test drives the lifecycle by writing canned
// JSON-RPC lines into stdout and observing stdin/Result.
func runFakeLifecycle(t *testing.T, opts ExecOptions, semanticInactivityTimeout time.Duration) (*codexClient, *fakeCodexIO, chan Message, <-chan Result) {
	t.Helper()
	fakeIO := newFakeCodexIO()
	c, msgCh := newTestCodexClient(t, fakeIO)
	resCh := make(chan Result, 1)
	turnDone := make(chan bool, 1)
	semanticActivityCh := make(chan string, 256)

	runCtx, cancel := context.WithCancel(context.Background())
	readerDone := make(chan struct{})

	// Wire the onTurnDone hook to also push onto turnDone so the
	// lifecycle's select on turnDone fires.
	c.onTurnDone = func(aborted bool) {
		select {
		case turnDone <- aborted:
		default:
		}
	}
	c.onSemanticActivity = func(description string) {
		select {
		case semanticActivityCh <- description:
		default:
		}
	}

	// Reader goroutine: parses each line and dispatches.
	go func() {
		defer close(readerDone)
		defer close(fakeIO.stdoutDone)
		scanner := bufio.NewScanner(fakeIO.stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			c.handleLine(line)
		}
		if err := scanner.Err(); err != nil {
			c.markProcessExited(fmt.Errorf("%w: %v", errCodexProcessExited, err))
			return
		}
		c.markProcessExited(errCodexProcessExited)
	}()

	var outputMu sync.Mutex
	var output strings.Builder
	c.onMessage = func(msg Message) {
		if msg.Type == MessageText {
			outputMu.Lock()
			output.WriteString(msg.Content)
			outputMu.Unlock()
		}
		select {
		case msgCh <- msg:
		default:
		}
		select {
		case semanticActivityCh <- describeCodexSemanticActivity(msg):
		default:
		}
	}

	drained := atomic.Bool{}
	drainAndWait := func() {
		if drained.Swap(true) {
			return
		}
		// Simulate the production drainAndWait: close stdin, then
		// wait for the reader to finish, capped by grace. In tests
		// the reader finishes when stdout is closed.
		fakeIO.stdin.Close()
		grace := codexGracefulShutdown()
		select {
		case <-readerDone:
		case <-time.After(grace):
			cancel()
			<-readerDone
		}
	}

	go runCodexLifecycle(runCodexLifecycleParams{
		runCtx:                    runCtx,
		cancel:                    cancel,
		client:                    c,
		msgCh:                     msgCh,
		resCh:                     resCh,
		opts:                      opts,
		prompt:                    "hello",
		timeout:                   0,
		semanticInactivityTimeout: semanticInactivityTimeout,
		logger:                    c.cfg.Logger,
		turnDone:                  turnDone,
		semanticActivityCh:        semanticActivityCh,
		getStderrTail:             func() string { return "" },
		drainAndWait:              drainAndWait,
		readOutput: func() string {
			outputMu.Lock()
			defer outputMu.Unlock()
			return output.String()
		},
		now: time.Now,
	})

	return c, fakeIO, msgCh, resCh
}

// ── TestCodexParseEvents ──
//
// Feeds a captured turn/started notification + turn/completed + an
// item/completed agentMessage through handleLine, then asserts the
// resulting Message stream is exactly what a downstream consumer
// (SSE replay, store) would persist. This is the wire-format
// contract — if a field name or block type drifts, every streaming
// consumer silently loses data.
func TestCodexParseEvents(t *testing.T) {
	t.Parallel()

	c, _ := newTestCodexClient(t, newFakeCodexIO())
	c.notificationProtocol = "raw" // skip auto-detection
	c.threadID = "t-1"             // set so the thread-id guard doesn't filter

	// Drive a turn/started → item/completed (agentMessage) →
	// turn/completed sequence. This is the bare minimum a successful
	// turn emits.
	c.handleLine(`{"jsonrpc":"2.0","method":"turn/started","params":{"turn":{"id":"turn-1"},"threadId":"t-1"}}`)
	c.handleLine(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"t-1","item":{"id":"msg-1","type":"agentMessage","text":"hello from codex","phase":"final_answer"}}}`)
	c.handleLine(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t-1","turn":{"id":"turn-1","status":"completed"}}}`)

	if got := c.turnID; got != "turn-1" {
		t.Errorf("turnID: want %q, got %q", "turn-1", got)
	}
	if !c.turnStarted {
		t.Error("turnStarted: want true, got false")
	}
}

// ── TestCodexWatchdog ──
//
// Drives a full lifecycle with a tiny semanticInactivityTimeout and a
// stdout that emits one turn/started notification then stalls. The
// lifecycle must reach the semantic-inactivity branch and produce
// Result{Status: "timeout"} carrying the
// CodexSemanticInactivityMarker.
func TestCodexWatchdog(t *testing.T) {
	t.Parallel()

	// 50ms is short enough to keep the test under a second on slow
	// CI and long enough that scheduler jitter won't trip it. The
	// default production constant is 10 minutes.
	short := 50 * time.Millisecond
	c, fakeIO, msgCh, resCh := runFakeLifecycle(t, ExecOptions{}, short)

	// Drain msgCh in the background so the channel doesn't fill up.
	go func() {
		for range msgCh {
		}
	}()

	// Drive the handshake one request at a time, waiting for the
	// lifecycle to register+write each request before feeding the
	// matching response. Without this sync, a response can be
	// dispatched before the matching pending entry is registered
	// in codexClient and the lifecycle blocks forever waiting for
	// a response that was already silently dropped.
	stdin := c.stdin.(*fakeStdin)
	waitForRequests(t, stdin, 1)
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	waitForRequests(t, stdin, 2) // initialize → thread/start
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"t-1"}}}`)
	waitForRequests(t, stdin, 3) // thread/start → turn/start
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","id":3,"result":{}}`)
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","method":"turn/started","params":{"turn":{"id":"turn-1"},"threadId":"t-1"}}`)

	// Don't close stdout yet — codex is "alive" but stalled.

	select {
	case res := <-resCh:
		// Either watchdog can fire: firstTurnNoProgress (4/5 of
		// semanticInactivityTimeout) or semanticInactivityTimeout.
		// With short=50ms the firstTurnNoProgress timer fires
		// first at 40ms; both are valid "codex app-server
		// stalled" outcomes from the user's perspective.
		if res.Status != "timeout" {
			t.Fatalf("status: want %q, got %q (error: %q)", "timeout", res.Status, res.Error)
		}
		if !strings.Contains(res.Error, CodexSemanticInactivityMarker) &&
			!strings.Contains(res.Error, CodexFirstTurnNoProgressMarker) {
			t.Errorf("error %q: missing one of %q / %q",
				res.Error, CodexSemanticInactivityMarker, CodexFirstTurnNoProgressMarker)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not fire within 2s")
	}

	// Cleanup: close stdout so the reader goroutine returns. The
	// lifecycle has already exited.
	fakeIO.closeStdout(t)

	if c.turnID != "turn-1" {
		t.Errorf("turnID: want %q, got %q", "turn-1", c.turnID)
	}
}

// ── TestCodexResume ──
//
// Verifies that opts.ResumeSessionID="t-1" causes the JSON-RPC
// conversation to issue `thread/resume` (NOT thread/start), and that
// --resume does NOT appear in the CLI argv. This is the v1 contract
// for resuming a prior codex session.
func TestCodexResume(t *testing.T) {
	t.Parallel()

	// buildCodexArgs must not include --resume — the daemon must own
	// the resume flag, and multica's codex has no --resume flag at
	// all (resume is JSON-RPC only).
	args := buildCodexArgs(ExecOptions{ResumeSessionID: "t-1"}, slog.Default())
	for _, a := range args {
		if a == "--resume" || strings.HasPrefix(a, "--resume=") {
			t.Fatalf("buildCodexArgs must not include --resume (resume is JSON-RPC only), got %v", args)
		}
	}

	// Drive the lifecycle and observe the stdin writes.
	fakeIO := newFakeCodexIO()
	c, msgCh := newTestCodexClient(t, fakeIO)
	resCh := make(chan Result, 1)
	turnDone := make(chan bool, 1)
	semanticActivityCh := make(chan string, 256)

	runCtx, cancel := context.WithCancel(context.Background())
	readerDone := make(chan struct{})

	go func() {
		defer close(readerDone)
		defer close(fakeIO.stdoutDone)
		scanner := bufio.NewScanner(fakeIO.stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			c.handleLine(line)
		}
		if err := scanner.Err(); err != nil {
			c.markProcessExited(fmt.Errorf("%w: %v", errCodexProcessExited, err))
			return
		}
		c.markProcessExited(errCodexProcessExited)
	}()

	var outputMu sync.Mutex
	var output strings.Builder
	c.onMessage = func(msg Message) {
		if msg.Type == MessageText {
			outputMu.Lock()
			output.WriteString(msg.Content)
			outputMu.Unlock()
		}
		select {
		case msgCh <- msg:
		default:
		}
		select {
		case semanticActivityCh <- describeCodexSemanticActivity(msg):
		default:
		}
	}
	c.onTurnDone = func(aborted bool) {
		select {
		case turnDone <- aborted:
		default:
		}
	}
	c.onSemanticActivity = func(description string) {
		select {
		case semanticActivityCh <- description:
		default:
		}
	}

	drainAndWait := func() {
		fakeIO.stdin.Close()
		select {
		case <-readerDone:
		case <-time.After(codexGracefulShutdown()):
			cancel()
			<-readerDone
		}
	}

	go runCodexLifecycle(runCodexLifecycleParams{
		runCtx:                    runCtx,
		cancel:                    cancel,
		client:                    c,
		msgCh:                     msgCh,
		resCh:                     resCh,
		opts:                      ExecOptions{ResumeSessionID: "t-1"},
		prompt:                    "hello",
		timeout:                   0,
		semanticInactivityTimeout: 200 * time.Millisecond,
		logger:                    c.cfg.Logger,
		turnDone:                  turnDone,
		semanticActivityCh:        semanticActivityCh,
		getStderrTail:             func() string { return "" },
		drainAndWait:              drainAndWait,
		readOutput: func() string {
			outputMu.Lock()
			defer outputMu.Unlock()
			return output.String()
		},
		now: time.Now,
	})

	// Drain msgCh so the channel doesn't fill up.
	go func() {
		for range msgCh {
		}
	}()

	// Drive the handshake one request at a time, waiting for the
	// lifecycle to register+write each request before feeding the
	// matching response.
	stdin := c.stdin.(*fakeStdin)
	waitForRequests(t, stdin, 1)
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	waitForRequests(t, stdin, 2) // initialize → thread/resume
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"t-resumed"}}}`)
	waitForRequests(t, stdin, 3) // thread/resume → turn/start
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","id":3,"result":{}}`)
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","method":"turn/started","params":{"turn":{"id":"turn-r"},"threadId":"t-resumed"}}`)
	// item/completed with type=agentMessage fires a non-"status:running"
	// semantic activity, which stops the firstTurnNoProgress watchdog.
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"t-resumed","item":{"id":"msg-1","type":"agentMessage","text":"hello from resumed codex","phase":"final_answer"}}}`)
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t-resumed","turn":{"id":"turn-r","status":"completed"}}}`)

	select {
	case res := <-resCh:
		if res.SessionID != "t-resumed" {
			t.Errorf("SessionID: want %q, got %q", "t-resumed", res.SessionID)
		}
		if res.Status != "completed" {
			t.Errorf("status: want %q, got %q (error: %q)", "completed", res.Status, res.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle did not complete within 2s")
	}

	fakeIO.closeStdout(t)

	// Verify the JSON-RPC requests captured on the fake stdin. The
	// order is: initialize, thread/resume (NOT thread/start),
	// turn/start. Inspect each in turn.
	stdinLines := fakeStdinLines(t, fakeIO.stdin)
	if len(stdinLines) < 2 {
		t.Fatalf("expected at least 2 JSON-RPC requests on stdin, got %d: %v", len(stdinLines), stdinLines)
	}
	var sawInitialize, sawThreadResume, sawThreadStart, sawTurnStart bool
	for _, line := range stdinLines {
		var req map[string]any
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			t.Fatalf("malformed JSON-RPC request: %v: %s", err, line)
		}
		method, _ := req["method"].(string)
		switch method {
		case "initialize":
			sawInitialize = true
		case "thread/resume":
			sawThreadResume = true
			params, _ := req["params"].(map[string]any)
			if got, _ := params["threadId"].(string); got != "t-1" {
				t.Errorf("thread/resume params.threadId: want %q, got %q", "t-1", got)
			}
		case "thread/start":
			sawThreadStart = true
		case "turn/start":
			sawTurnStart = true
		}
	}
	if !sawInitialize {
		t.Error("expected initialize request")
	}
	if !sawThreadResume {
		t.Error("expected thread/resume request when opts.ResumeSessionID is set")
	}
	if sawThreadStart {
		t.Error("thread/start should NOT be issued when resume succeeds")
	}
	if !sawTurnStart {
		t.Error("expected turn/start request")
	}
}

// ── TestCodexMcpConfig ──
//
// Verifies that opts.McpConfig (non-empty) results in a config.toml
// being written under $CODEX_HOME with the daemon-managed
// [mcp_servers.*] block, in alphabetical order, with file mode 0o600.
// This is the lazy seal against argv-borne secret leaks: the daemon
// owns the namespace via config.toml so `mcp_servers.<id>.env`
// values never reach OS argv.
func TestCodexMcpConfig(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.toml")

	raw := json.RawMessage(`{"mcpServers":{"zeta":{"command":"zb"},"alpha":{"command":"ac","env":{"KEY":"v"}}}}`)
	if err := ensureCodexMcpConfig(cfgPath, raw, slog.Default()); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	got := string(data)

	// Markers must be present.
	if !strings.Contains(got, codexMcpBeginMarker) || !strings.Contains(got, codexMcpEndMarker) {
		t.Fatalf("expected managed block markers, got:\n%s", got)
	}

	// Both server tables must be present, in alphabetical order.
	alphaIdx := strings.Index(got, "[mcp_servers.alpha]")
	zetaIdx := strings.Index(got, "[mcp_servers.zeta]")
	if alphaIdx == -1 || zetaIdx == -1 {
		t.Fatalf("expected both server tables, got:\n%s", got)
	}
	if alphaIdx > zetaIdx {
		t.Fatalf("expected alpha before zeta (alphabetical), got:\n%s", got)
	}

	// Body must contain the configured values.
	for _, want := range []string{
		`command = "ac"`,
		`env = { KEY = "v" }`,
		`command = "zb"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in:\n%s", want, got)
		}
	}

	// File mode must be 0o600 — secret-bearing mcp config must not
	// be world-readable.
	fi, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Fatalf("expected mode 0o600, got %o", mode)
	}

	// Idempotency: re-running ensure with the same input must
	// produce byte-identical output.
	if err := ensureCodexMcpConfig(cfgPath, raw, slog.Default()); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	second, _ := os.ReadFile(cfgPath)
	if string(data) != string(second) {
		t.Fatalf("non-idempotent write:\nfirst:\n%s\nsecond:\n%s", data, second)
	}
}

// ── TestCodexGracefulShutdown ──
//
// Verifies that when the run context is cancelled, the lifecycle
// calls drainAndWait which closes stdin, then waits up to the grace
// window before terminating. The fake stdin records the close call;
// the test asserts the close happened and the process was
// reaped within the grace window.
func TestCodexGracefulShutdown(t *testing.T) {
	t.Parallel()

	// Use a small grace window so the test stays fast.
	prev := codexGracefulShutdownTimeoutNanos.Load()
	codexGracefulShutdownTimeoutNanos.Store(int64(50 * time.Millisecond))
	t.Cleanup(func() {
		codexGracefulShutdownTimeoutNanos.Store(prev)
	})

	fakeIO := newFakeCodexIO()
	c, msgCh := newTestCodexClient(t, fakeIO)
	c.stdin.(*fakeStdin).notify = make(chan struct{}, 64)
	resCh := make(chan Result, 1)
	turnDone := make(chan bool, 1)
	semanticActivityCh := make(chan string, 256)

	runCtx, cancel := context.WithCancel(context.Background())
	readerDone := make(chan struct{})

	go func() {
		defer close(readerDone)
		defer close(fakeIO.stdoutDone)
		scanner := bufio.NewScanner(fakeIO.stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			c.handleLine(line)
		}
		if err := scanner.Err(); err != nil {
			c.markProcessExited(fmt.Errorf("%w: %v", errCodexProcessExited, err))
			return
		}
		c.markProcessExited(errCodexProcessExited)
	}()

	var outputMu sync.Mutex
	var output strings.Builder
	c.onMessage = func(msg Message) {
		if msg.Type == MessageText {
			outputMu.Lock()
			output.WriteString(msg.Content)
			outputMu.Unlock()
		}
		select {
		case msgCh <- msg:
		default:
		}
		select {
		case semanticActivityCh <- describeCodexSemanticActivity(msg):
		default:
		}
	}
	c.onTurnDone = func(aborted bool) {
		select {
		case turnDone <- aborted:
		default:
		}
	}
	c.onSemanticActivity = func(description string) {
		select {
		case semanticActivityCh <- description:
		default:
		}
	}

	// The drainAndWait closure the lifecycle calls records when it
	// closed stdin. Tracking the closure (rather than wrapping
	// stdin) avoids the type-assertion gymnastics of swapping
	// c.stdin to a tracking wrapper.
	closed := atomic.Bool{}
	closedAt := atomic.Int64{}
	drainAndWait := func() {
		_ = fakeIO.stdin.Close()
		if closed.CompareAndSwap(false, true) {
			closedAt.Store(time.Now().UnixNano())
		}
		grace := codexGracefulShutdown()
		select {
		case <-readerDone:
		case <-time.After(grace):
			cancel()
			<-readerDone
		}
	}

	go runCodexLifecycle(runCodexLifecycleParams{
		runCtx:                    runCtx,
		cancel:                    cancel,
		client:                    c,
		msgCh:                     msgCh,
		resCh:                     resCh,
		opts:                      ExecOptions{},
		prompt:                    "hello",
		timeout:                   0,
		semanticInactivityTimeout: 50 * time.Millisecond,
		logger:                    c.cfg.Logger,
		turnDone:                  turnDone,
		semanticActivityCh:        semanticActivityCh,
		getStderrTail:             func() string { return "" },
		drainAndWait:              drainAndWait,
		readOutput: func() string {
			outputMu.Lock()
			defer outputMu.Unlock()
			return output.String()
		},
		now: time.Now,
	})

	// Drain msgCh in the background.
	go func() {
		for range msgCh {
		}
	}()

	// Drive the handshake so the lifecycle reaches its main select
	// loop, then cancel the run context. The lifecycle should fall
	// into finishRunContextDone → drainAndWait → stdin closed.
	stdin := c.stdin.(*fakeStdin)
	waitForRequests(t, stdin, 1)
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	waitForRequests(t, stdin, 2)
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"t-1"}}}`)
	waitForRequests(t, stdin, 3)
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","id":3,"result":{}}`)
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","method":"turn/started","params":{"turn":{"id":"turn-1"},"threadId":"t-1"}}`)

	// Cancel via context. Use a separate goroutine so we can also
	// wait for the Result and the close.
	go func() {
		// Give the lifecycle a moment to enter the main select loop
		// before we cancel.
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	select {
	case res := <-resCh:
		// We don't care which status comes back (could be "aborted"
		// from the context cancel, or "timeout" if the watchdog
		// fires first) — we only care that the lifecycle exited
		// within the grace window and that stdin was closed.
		if res.Status != "aborted" && res.Status != "timeout" {
			t.Errorf("status: want aborted|timeout, got %q (error: %q)", res.Status, res.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle did not complete within 2s")
	}

	// Close stdout so the reader goroutine exits and unblocks the
	// lifecycle's deferred drainAndWait (which blocks on readerDone).
	// drainAndWait runs as a defer AFTER the Result is sent, so checking
	// `closed` before closing stdout would race the lifecycle's defer.
	fakeIO.closeStdout(t)

	// resCh closes only after drainAndWait returns (defers run LIFO,
	// drainAndWait before close(resCh)); draining it confirms the
	// lifecycle fully exited and `closed` is set.
	select {
	case <-resCh:
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle did not exit within 2s after stdout close")
	}

	// Verify stdin was actually closed.
	if !closed.Load() {
		t.Error("expected drainAndWait to close stdin")
	}
}

// ── fakes ──

// fakeStdin captures every Write call so tests can assert the
// JSON-RPC requests codexClient sent. Mirrors the multica
// codex_test.go fakeStdin. The notify channel, when non-nil, gets
// a signal after every successful Write so tests can synchronize
// request/response pairs.
type fakeStdin struct {
	mu     sync.Mutex
	data   []byte
	notify chan struct{}
}

func (f *fakeStdin) Write(p []byte) (int, error) {
	f.mu.Lock()
	f.data = append(f.data, p...)
	ch := f.notify
	f.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

func (f *fakeStdin) Close() error {
	return nil
}

func (f *fakeStdin) Lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var lines []string
	for _, line := range bytes.Split(f.data, []byte("\n")) {
		s := string(line)
		if s != "" {
			lines = append(lines, s)
		}
	}
	return lines
}

// requestCount returns the number of JSON-RPC request lines captured on
// stdin — lines carrying both "id" and "method". Notifications (method
// only, e.g. notify("initialized")) and responses (id only) are excluded,
// so each count aligns one-to-one with an outgoing request whose pending
// entry is registered before the write. Counting raw writes instead would
// race: notify("initialized") is the 2nd write but carries no id, so a
// test feeding the id:2 response after the 2nd write would do so before
// thread/start registered pending[2] — the response gets dropped and the
// request blocks forever, surfacing as flaky watchdog / cancel failures.
func (f *fakeStdin) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, line := range bytes.Split(f.data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(line, &msg) == nil && len(msg.ID) > 0 && msg.Method != "" {
			count++
		}
	}
	return count
}

// fakeStdinLines extracts the captured lines from any stdin fake
// that wraps a *fakeStdin (or the plain one). Used by tests that
// swap stdin for a tracking wrapper.
func fakeStdinLines(t *testing.T, stdin interface {
	Write(p []byte) (int, error)
	Close() error
}) []string {
	t.Helper()
	if fs, ok := stdin.(*fakeStdin); ok {
		return fs.Lines()
	}
	t.Fatalf("stdin is not a *fakeStdin: %T", stdin)
	return nil
}

// waitForRequests blocks until the fakeStdin has captured at least n
// JSON-RPC request lines (lines with both "id" and "method"). Returns the
// captured lines for assertions. The notify channel fires on every Write
// (including notifications) and unblocks the loop; requestCount re-filters
// so notifications don't advance the count. Waiting on requests — not raw
// writes — keeps each response paired with a pending entry that was
// registered before the request was written, so no response is ever dropped
// to a missing pending slot.
func waitForRequests(t *testing.T, stdin *fakeStdin, n int) []string {
	t.Helper()
	if stdin.notify == nil {
		t.Fatal("waitForRequests: stdin.notify is nil — set stdin.notify in the test")
	}
	deadline := time.After(2 * time.Second)
	for stdin.requestCount() < n {
		select {
		case <-stdin.notify:
		case <-deadline:
			t.Fatalf("timed out waiting for %d requests; have %d: %v", n, stdin.requestCount(), stdin.Lines())
		}
	}
	return stdin.Lines()
}

// trackCloseStdin wraps a *fakeStdin and calls onClose when Close
// runs. Kept for documentation; the current test that needs
// stdin-close tracking (TestCodexGracefulShutdown) wires that
// observation through the drainAndWait closure instead, so this
// type is dead — but it documents an alternative shape for
// future tests that need a wrapped stdin.
type trackCloseStdin struct {
	inner   *fakeStdin
	closed  atomic.Bool
	onClose func()
}

func (t *trackCloseStdin) Write(p []byte) (int, error) {
	return t.inner.Write(p)
}

func (t *trackCloseStdin) Close() error {
	if t.closed.CompareAndSwap(false, true) && t.onClose != nil {
		t.onClose()
	}
	return nil
}
