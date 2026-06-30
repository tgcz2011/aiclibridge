// Package adapter — ACP (Agent Client Protocol) backend.
//
// EXPERIMENTAL. This file adapts a family of coding CLIs that speak ACP
// (Agent Client Protocol, the AionUi-driven standard) over stdio JSON-RPC
// 2.0. ACP clients (the daemon) send requests (initialize / session/new /
// session/load / session/prompt / session/cancel); servers (the CLI
// process) reply with responses and push notifications (session/message /
// session/finished / session/error).
//
// Only `copilot` has been installed on the dev machine; the other seven
// (goose / cursor / kimi / kiro / qoder / hermes / auggie) are unverified.
// The differences between the eight are confined to the binary name, the
// ACP entry args (--acp flag vs acp subcommand), the MCP-injection flag
// (--additional-mcp-config for copilot, --mcp-config for the rest), and
// resume semantics. Those deltas are captured in acpBackends; everything
// else (JSON-RPC framing, event parsing, status mapping) is shared.
//
// The session/message parsing is intentionally liberal: it accepts both the
// direct {type, content} shape described in the task and the nested
// {message:{content:[...]}} Claude-style envelope, since the on-the-w ACP
// schema has not been confirmed against a real capture for most CLIs. Field
// names follow camelCase (sessionId / inputTokens), the ACP convention.
//
// Do not rely on this adapter for production correctness until a smoke
// round-trip is captured per CLI; treat the shared code as a best-effort
// implementation of the ACP standard.
package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ── Per-CLI configuration ──

// acpBackendConfig describes how to launch one ACP-speaking CLI. The eight
// supported CLIs differ only in the fields below; the JSON-RPC protocol
// they speak on top is identical.
type acpBackendConfig struct {
	// binary is the CLI executable name looked up via exec.LookPath when
	// Config.ExecutablePath is empty.
	binary string
	// acpArgs are the ACP entry arguments appended right after the binary:
	// ["--acp"] for flag-style entry (copilot/cursor/kiro/qoder/hermes/
	// auggie) or ["acp"] for subcommand-style entry (goose/kimi).
	acpArgs []string
	// mcpFlag is the flag used to pass an MCP config file. copilot uses
	// --additional-mcp-config; the rest use --mcp-config. Empty means the
	// CLI has no documented MCP-injection flag and MCP config is silently
	// skipped (not fatal — matches the project's incremental-support policy).
	mcpFlag string
	// mcpInline records whether the CLI accepts inline JSON for its MCP
	// flag (copilot does). Currently unused for routing — all backends
	// route through a temp file via writeMcpConfigToTemp so the lifecycle
	// and cleanup contract stays identical to claude/qwen/codebuddy — but
	// kept on the config so a future inline path can key off it without
	// touching the table.
	mcpInline bool
	// resumeMode documents the CLI's native resume capability. ACP
	// standardises resume on the session/load request, so this field is
	// informational only; it does not change args. "session" = native
	// session-id resume (copilot), "continue" = -c/--continue style,
	// "none" = unconfirmed.
	resumeMode string
}

// acpBackends maps each supported ACP CLI name to its launch config. Keys
// are the agent-type strings the daemon dispatches on (copilot / goose /
// cursor / kimi / kiro / qoder / hermes / auggie). Adding a new ACP CLI is
// a one-line table edit; no other change to this file is required.
var acpBackends = map[string]acpBackendConfig{
	"copilot": {
		binary:    "copilot",
		acpArgs:   []string{"--acp"},
		mcpFlag:   "--additional-mcp-config",
		mcpInline: true,
		resumeMode: "session",
	},
	"goose": {
		binary:    "goose",
		acpArgs:   []string{"acp"},
		mcpFlag:   "--mcp-config",
		mcpInline: false,
		resumeMode: "none",
	},
	"cursor": {
		binary:    "cursor-agent",
		acpArgs:   []string{"--acp"},
		mcpFlag:   "--mcp-config",
		mcpInline: false,
		resumeMode: "none",
	},
	"kimi": {
		binary:    "kimi",
		acpArgs:   []string{"acp"},
		mcpFlag:   "--mcp-config",
		mcpInline: false,
		resumeMode: "none",
	},
	"kiro": {
		binary:    "kiro",
		acpArgs:   []string{"--acp"},
		mcpFlag:   "--mcp-config",
		mcpInline: false,
		resumeMode: "none",
	},
	"qoder": {
		binary:    "qoder",
		acpArgs:   []string{"--acp"},
		mcpFlag:   "--mcp-config",
		mcpInline: false,
		resumeMode: "none",
	},
	"hermes": {
		binary:    "hermes",
		acpArgs:   []string{"--acp"},
		mcpFlag:   "--mcp-config",
		mcpInline: false,
		resumeMode: "none",
	},
	"auggie": {
		binary:    "auggie",
		acpArgs:   []string{"--acp"},
		mcpFlag:   "--mcp-config",
		mcpInline: false,
		resumeMode: "none",
	},
}

// acpBlockedArgs are flags the daemon owns and must not be overridden by
// user-configured custom_args. Overriding the MCP-injection flag or the ACP
// entry flag would break the daemon↔CLI protocol contract. Mirrors
// claudeBlockedArgs / qwenBlockedArgs — same generic filterCustomArgs helper,
// ACP-specific set.
var acpBlockedArgs = map[string]blockedArgMode{
	"--mcp-config":           blockedWithValue, // set by daemon from agent.mcp_config
	"--additional-mcp-config": blockedWithValue, // copilot's MCP flag
	"--acp":                  blockedStandalone, // ACP entry flag (daemon-managed)
}

// acpBackend implements Backend by spawning an ACP-speaking CLI and driving
// a JSON-RPC 2.0 conversation over stdin/stdout. One struct serves all
// eight CLIs; the per-CLI delta is looked up from acpBackends by name.
type acpBackend struct {
	name string // key into acpBackends (copilot/goose/...)
	cfg  Config
}

// newAcpBackend constructs an acpBackend for the named ACP CLI. Returns an
// error if name is not a known ACP backend. The caller (the daemon's New
// switch) is expected to validate the agent type before reaching here, but
// the guard keeps the constructor safe to call directly from tests.
func newAcpBackend(name string, cfg Config) (*acpBackend, error) {
	if _, ok := acpBackends[name]; !ok {
		return nil, fmt.Errorf("unknown acp backend %q (supported: %s)", name, acpBackendNames())
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &acpBackend{name: name, cfg: cfg}, nil
}

// acpBackendNames returns the sorted, comma-separated list of supported
// ACP CLI names for error messages.
func acpBackendNames() string {
	names := make([]string, 0, len(acpBackends))
	for k := range acpBackends {
		names = append(names, k)
	}
	return strings.Join(names, "/")
}

// ── JSON-RPC 2.0 framing ──

// jsonrpcRequest is a JSON-RPC 2.0 request sent by the daemon to the CLI.
// id is a monotonic integer (1=initialize, 2=session/*, 3=session/prompt,
// 99=session/cancel). params may be nil for parameterless methods.
type jsonrpcRequest struct {
	Jsonrpc string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// jsonrpcMessage is the generic envelope used to decode any line the CLI
// writes to stdout: a response (id set, result or error populated), a
// notification (no id, method + params), or a request the CLI sends back
// to the daemon (not currently handled but tolerated). id is kept as a
// RawMessage so we can distinguish a missing id (notification) from an
// id of 0 or null.
type jsonrpcMessage struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

// jsonrpcError is the standard JSON-RPC error object carried on responses
// with a non-zero id whose request failed.
type jsonrpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// isResponse reports whether m is a JSON-RPC response (carries an id that
// is not null). Notifications omit id entirely; a literal null id is
// treated as a notification too.
func (m jsonrpcMessage) isResponse() bool {
	trimmed := strings.TrimSpace(string(m.ID))
	return trimmed != "" && trimmed != "null"
}

// intID parses the response id as an integer. Returns 0 (and the caller's
// switch default) when the id is non-numeric or absent.
func (m jsonrpcMessage) intID() int {
	var n int
	_ = json.Unmarshal(m.ID, &n)
	return n
}

// ── ACP method params ──

// acpInitializeParams is the initialize request body. protocolVersion is
// the ACP version the daemon speaks; clientCapabilities advertises which
// notification types the daemon consumes.
type acpInitializeParams struct {
	ProtocolVersion    string          `json:"protocolVersion"`
	ClientCapabilities json.RawMessage `json:"clientCapabilities,omitempty"`
}

// acpSessionNewParams creates a fresh session. cwd is passed when the
// daemon has pinned a working directory so the CLI's file tools resolve
// against the task root.
type acpSessionNewParams struct {
	Cwd string `json:"cwd,omitempty"`
}

// acpSessionLoadParams resumes an existing session by id.
type acpSessionLoadParams struct {
	SessionID string `json:"sessionId"`
}

// acpSessionPromptParams delivers a user turn to an established session.
type acpSessionPromptParams struct {
	Prompt    string `json:"prompt"`
	SessionID string `json:"sessionId,omitempty"`
}

// acpCancelParams carries the session to cancel. sessionID is optional;
// when omitted the server cancels its currently-active session.
type acpCancelParams struct {
	SessionID string `json:"sessionId,omitempty"`
}

// acpSessionResult is the result object of session/new / session/load,
// carrying the established session id. Field name follows the ACP
// camelCase convention; a sessionID (lowercase d) alias is tolerated via
// the secondary parse in extractSessionID.
type acpSessionResult struct {
	SessionID string `json:"sessionId,omitempty"`
}

// acpSessionMessageParams is the params of a session/message notification.
// It accepts two shapes:
//   - direct: {type:"text|thinking|tool_use|tool_result", content/text/...}
//   - nested: {message:{role, content:[{type,text,...}]}} (Claude-style)
//
// All content-bearing fields are json.RawMessage so a string or structured
// value can be unpacked lazily after the shape is decided.
type acpSessionMessageParams struct {
	Type      string          `json:"type,omitempty"`
	Text      string          `json:"text,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	ID        string          `json:"id,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"toolUseId,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

// acpMessageEnvelope is the nested {message:{role, content[]}} form. Only
// the content array is consumed; role is tolerated for completeness.
type acpMessageEnvelope struct {
	Role    string            `json:"role,omitempty"`
	Content []acpContentBlock `json:"content,omitempty"`
}

// acpContentBlock is one block inside a nested message's content array.
// Mirrors the Claude Code SDK / qwen content-block shape so the same
// handler applies.
type acpContentBlock struct {
	Type      string          `json:"type,omitempty"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// acpSessionFinishedParams is the params of session/finished, carrying the
// terminal session id and (optionally) token usage. usage is RawMessage so
// the flat and per-model shapes can both be tried in acpExtractUsage.
type acpSessionFinishedParams struct {
	SessionID string          `json:"sessionId,omitempty"`
	Usage     json.RawMessage `json:"usage,omitempty"`
}

// acpSessionErrorParams is the params of session/error. ACP servers may
// carry either a top-level message or a nested error object; both are
// tolerated.
type acpSessionErrorParams struct {
	SessionID string          `json:"sessionId,omitempty"`
	Message  string          `json:"message,omitempty"`
	Error    json.RawMessage `json:"error,omitempty"`
}

// acpUsageBlock is the flat token-usage shape carried on session/finished
// and (per-model) inside a usage map. Field names follow ACP camelCase.
type acpUsageBlock struct {
	InputTokens              int64 `json:"inputTokens,omitempty"`
	OutputTokens             int64 `json:"outputTokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cacheReadInputTokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cacheCreationInputTokens,omitempty"`
}

// ── stdin writer ──

// acpStdin serialises writes to the CLI's stdin. processAcpEvents and the
// cancel watcher both write requests through this wrapper so concurrent
// writes (a session/cancel arriving while the handshake is in flight)
// cannot interleave bytes and corrupt a JSON-RPC frame.
type acpStdin struct {
	mu sync.Mutex
	w  io.Writer
}

// writeRequest marshals and writes one JSON-RPC request followed by a
// newline. Errors are returned to the caller; processAcpEvents logs them
// and continues, since a failed write usually means the CLI has exited and
// the stdout scanner is about to hit EOF.
func (s *acpStdin) writeRequest(method string, params any, id int) error {
	data, err := encodeJsonrpcRequest(method, params, id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.w.Write(data)
	return err
}

// encodeJsonrpcRequest builds the newline-terminated JSON-RPC 2.0 request
// frame for the given method/params/id. Extracted from acpStdin so the
// encoding is directly unit-testable without an io.Writer.
func encodeJsonrpcRequest(method string, params any, id int) ([]byte, error) {
	req := jsonrpcRequest{Jsonrpc: "2.0", ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal jsonrpc request %q: %w", method, err)
	}
	return append(data, '\n'), nil
}

// ── ACP request IDs ──
//
// Fixed integer ids keep the protocol flow legible and let processAcpEvents
// route each response without a pending-request table. A real multiplexing
// client would hand out ids dynamically, but the daemon runs exactly one
// handshake + one prompt per process, so four fixed ids suffice.
const (
	acpIDInitialize  = 1
	acpIDSession      = 2
	acpIDPrompt      = 3
	acpIDCancel       = 99
)

// acpProtocolVersion is the ACP protocol version the daemon advertises in
// the initialize request. Bumped here when the daemon grows support for a
// newer revision.
const acpProtocolVersion = "1"

// acpClientCapabilities advertises the notification types the daemon
// consumes. Kept as a raw literal so an unknown field set never breaks
// decoding; the server treats unknown capabilities as unsupported.
const acpClientCapabilities = `{"text":true,"thinking":true,"tool_use":true,"tool_result":true}`

// ── Execute ──

// Execute runs a single ACP turn: spawn the CLI in ACP mode, perform the
// initialize → session/new|load → session/prompt handshake, and stream
// session/message notifications to the caller until session/finished or
// session/error arrives (or the run context fires). The function returns
// immediately with a Session whose channels are drained by a background
// goroutine that lives until the CLI exits — mirroring the qwen / claude
// / codebuddy backends.
func (b *acpBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	bc, ok := acpBackends[b.name]
	if !ok {
		return nil, fmt.Errorf("acp backend %q not configured", b.name)
	}

	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = bc.binary
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, fmt.Errorf("acp executable not found at %q: %w", execPath, err)
	}

	timeout := opts.Timeout
	runCtx, cancel := runContext(ctx, timeout)

	args := buildAcpArgs(bc, opts, b.cfg.Logger)

	// MCP config: route through a temp file via the shared
	// writeMcpConfigToTemp helper so the lifecycle and cleanup contract are
	// identical to claude / qwen / codebuddy. copilot's
	// --additional-mcp-config and the others' --mcp-config both accept a
	// file path; CLIs without a documented MCP flag (mcpFlag=="") skip
	// injection silently rather than failing, matching the project's
	// incremental-support policy.
	var mcpConfigPath string
	var mcpFileCleanup func()
	if len(opts.McpConfig) > 0 && bc.mcpFlag != "" {
		path, err := writeMcpConfigToTemp(opts.McpConfig)
		if err != nil {
			cancel()
			return nil, err
		}
		mcpConfigPath = path
		mcpFileCleanup = func() { os.Remove(mcpConfigPath) }
		args = append(args, bc.mcpFlag, mcpConfigPath)
	}
	defer func() {
		if mcpFileCleanup != nil {
			mcpFileCleanup()
		}
	}()

	cmd := exec.CommandContext(runCtx, execPath, args...)
	hideAgentWindow(cmd)
	configureProcessGroup(cmd)
	b.cfg.Logger.Info("agent command", "exec", execPath, "args", args, "backend", "acp", "cli", b.name)
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	env := buildEnv(b.cfg.Env)
	if opts.Cwd != "" {
		// Override PWD so the CLI resolves its discovery root to the task
		// workdir (cmd.Dir alone is not enough; many CLIs read PWD before
		// falling back to process.cwd()).
		env = append(env, "PWD="+opts.Cwd)
	}
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("acp stdout pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("acp stdin pipe: %w", err)
	}
	var closeStdinOnce sync.Once
	closeStdin := func() { closeStdinOnce.Do(func() { _ = stdin.Close() }) }
	// Capture stderr into both the daemon log and a bounded tail buffer so
	// we can include the last few KB in Result.Error when the CLI exits
	// unexpectedly (e.g. auth failure, node panic, ACP version mismatch).
	stderrBuf := newStderrTail(newLogWriter(b.cfg.Logger, "["+b.name+":stderr] "), 0)
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		closeStdin()
		cancel()
		return nil, fmt.Errorf("start %s: %w", b.name, err)
	}

	b.cfg.Logger.Info("acp started", "cli", b.name, "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

	// cmd.Start() succeeded — transfer temp file ownership to the goroutine.
	mcpFileCleanup = nil

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)

	// acpStdin serialises writes between the handshake driver
	// (processAcpEvents) and the cancel watcher. The watcher is best-effort:
	// when runCtx fires it sends session/cancel so the server can drain
	// gracefully; CommandContext remains the kill backstop so a wedged
	// server still terminates.
	sw := &acpStdin{w: stdin}
	done := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			_ = sw.writeRequest("session/cancel", acpCancelParams{}, acpIDCancel)
		case <-done:
		}
	}()

	go func() {
		defer cancel()
		defer close(msgCh)
		defer close(resCh)
		defer close(done) // before cancel() (LIFO) so the watcher exits cleanly on normal completion
		if mcpConfigPath != "" {
			defer os.Remove(mcpConfigPath)
		}

		startTime := time.Now()
		fallbackModel := opts.Model
		if fallbackModel == "" {
			fallbackModel = "unknown"
		}
		scanResult := b.processAcpEvents(stdout, msgCh, sw, closeStdin, prompt, opts.ResumeSessionID, fallbackModel)
		exitErr := cmd.Wait()
		duration := time.Since(startTime)

		status := scanResult.status
		errMsg := scanResult.errMsg

		switch {
		case runCtx.Err() == context.DeadlineExceeded:
			status = "timeout"
			errMsg = fmt.Sprintf("%s timed out after %s", b.name, timeout)
		case runCtx.Err() == context.Canceled:
			status = "aborted"
			errMsg = "execution cancelled"
		case exitErr != nil && status == "completed":
			status = "failed"
			errMsg = fmt.Sprintf("%s exited with error: %v", b.name, exitErr)
		}

		// cmd.Wait() has returned — os/exec's stderr copy goroutine has
		// observed every byte the CLI wrote to stderr before exiting, so
		// stderrBuf.Tail() is safe to sample now. Attach the tail to any
		// non-empty failure message so callers see the real reason instead
		// of a bare exit code.
		if errMsg != "" {
			errMsg = withAcpStderr(b.name, errMsg, stderrBuf.Tail())
		}

		b.cfg.Logger.Info("acp finished", "cli", b.name, "pid", cmd.Process.Pid, "status", status, "duration", duration.Round(time.Millisecond).String())

		reportedSessionID := resolveSessionID(opts.ResumeSessionID, scanResult.sessionID, status == "failed")

		resCh <- Result{
			Status:     status,
			Output:     scanResult.output,
			Error:      errMsg,
			DurationMs: duration.Milliseconds(),
			SessionID:  reportedSessionID,
			Usage:      scanResult.usage,
		}
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
}

// ── Event processing ──

// acpEventResult accumulates the terminal state extracted from an ACP
// stdout scan: final status, error message, accumulated text output,
// session id, and token usage (per-model map). Mirrors qwenEventResult so
// the daemon's billing / reporting path is identical across backends.
type acpEventResult struct {
	status    string
	errMsg    string
	output    string
	sessionID string
	usage     map[string]TokenUsage
}

// processAcpEvents drives the full ACP JSON-RPC flow against the CLI:
//  1. send initialize (id=1), read until its response arrives
//  2. send session/new or session/load (id=2), read until its response
//     arrives — the established session id is captured from the result
//  3. send session/prompt (id=3), then read the notification stream until
//     session/finished or session/error arrives (or stdout hits EOF)
//
// Notifications are unpacked into Message values on ch. Extracted from
// Execute for testability — the pure protocol / parsing path can be
// exercised against a stub io.Reader without spawning a real CLI.
//
// closeStdin is invoked on the terminal notification so the CLI can drain
// its shutdown path promptly; the caller still owns the final cmd.Wait.
func (b *acpBackend) processAcpEvents(r io.Reader, ch chan<- Message, sw *acpStdin, closeStdin func(), prompt, resumeSessionID, fallbackModel string) acpEventResult {
	var output strings.Builder
	var sessionID string
	var usageMap map[string]TokenUsage
	finalStatus := "completed"
	var finalError string

	// Step 1: initialize. Best-effort write — if stdin is already closed
	// (CLI exited), the scanner below will EOF and surface the failure.
	if err := sw.writeRequest("initialize", acpInitializeParams{
		ProtocolVersion:    acpProtocolVersion,
		ClientCapabilities: json.RawMessage(acpClientCapabilities),
	}, acpIDInitialize); err != nil {
		b.cfg.Logger.Warn("acp: write initialize failed", "cli", b.name, "error", err)
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	terminal := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var msg jsonrpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			// Not a JSON-RPC frame — the CLI may print a banner or log line
			// to stdout before the first frame. Silently skip; the stderr
			// tail captures diagnostics for failures.
			continue
		}

		if msg.isResponse() {
			// A response to one of our requests. An error object on any
			// handshake response is fatal for this turn.
			if msg.Error != nil {
				finalStatus = "failed"
				finalError = acpResponseErrorMessage(msg.Error)
				trySend(ch, Message{Type: MessageError, Content: finalError})
				closeStdin()
				terminal = true
				break
			}
			switch msg.intID() {
			case acpIDInitialize:
				// Step 2: session/new or session/load. session/load resumes
				// an existing session when the daemon passes a resume id;
				// otherwise session/new starts a fresh one.
				var params any
				if resumeSessionID != "" {
					params = acpSessionLoadParams{SessionID: resumeSessionID}
				} else {
					params = acpSessionNewParams{}
				}
				if err := sw.writeRequest(acpSessionLoadMethod(resumeSessionID), params, acpIDSession); err != nil {
					b.cfg.Logger.Warn("acp: write session request failed", "cli", b.name, "error", err)
				}
			case acpIDSession:
				// Capture the established session id from the result. When
				// the CLI omits it (some servers return an empty result)
				// fall back to the requested resume id.
				if sid := extractSessionID(msg.Result); sid != "" {
					sessionID = sid
				} else if sessionID == "" {
					sessionID = resumeSessionID
				}
				// Step 3: session/prompt. Send the user turn and let the
				// notification stream drive the rest.
				if err := sw.writeRequest("session/prompt", acpSessionPromptParams{
					Prompt:    prompt,
					SessionID: sessionID,
				}, acpIDPrompt); err != nil {
					b.cfg.Logger.Warn("acp: write session/prompt failed", "cli", b.name, "error", err)
				}
				trySend(ch, Message{Type: MessageStatus, Status: "running", SessionID: sessionID})
			case acpIDPrompt:
				// The prompt's response acks the turn. Some servers send it
				// after session/finished; others before. Treat it as
				// informational only — the terminal state comes from the
				// notifications.
			}
			continue
		}

		// Notification (no id).
		switch msg.Method {
		case "session/message":
			handleAcpSessionMessage(msg.Params, ch, &output, &usageMap, fallbackModel, &sessionID)
		case "session/finished":
			handleAcpFinished(msg.Params, &sessionID, &usageMap, fallbackModel)
			closeStdin()
			terminal = true
		case "session/error":
			finalStatus = "failed"
			finalError = extractAcpError(msg.Params)
			if finalError == "" {
				finalError = b.name + " reported session/error"
			}
			trySend(ch, Message{Type: MessageError, Content: finalError})
			closeStdin()
			terminal = true
		}

		if terminal {
			break
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		b.cfg.Logger.Warn("acp stdout scanner error", "cli", b.name, "error", scanErr)
		if finalStatus == "completed" {
			finalStatus = "failed"
			finalError = fmt.Sprintf("stdout read error: %v", scanErr)
		}
	}

	return acpEventResult{
		status:    finalStatus,
		errMsg:    finalError,
		output:    output.String(),
		sessionID: sessionID,
		usage:     usageMap,
	}
}

// acpSessionLoadMethod returns the ACP method name for the session step:
// "session/load" when resuming, "session/new" otherwise. Kept as a helper
// so the resume routing is testable in isolation.
func acpSessionLoadMethod(resumeSessionID string) string {
	if resumeSessionID != "" {
		return "session/load"
	}
	return "session/new"
}

// extractSessionID pulls the session id out of a session/new or
// session/load result. Tries the camelCase sessionId first (ACP
// convention), then the snake_case sessionID alias defensively, so a
// server using either spelling is handled without per-server branches.
func extractSessionID(result json.RawMessage) string {
	if len(result) == 0 {
		return ""
	}
	var r acpSessionResult
	if err := json.Unmarshal(result, &r); err == nil && r.SessionID != "" {
		return r.SessionID
	}
	// Fall back to snake_case sessionID.
	var alias struct {
		SessionID string `json:"sessionID,omitempty"`
	}
	if err := json.Unmarshal(result, &alias); err == nil && alias.SessionID != "" {
		return alias.SessionID
	}
	return ""
}

// handleAcpSessionMessage unpacks a session/message notification. It accepts
// both the direct {type, content} shape and the nested
// {message:{content:[...]}} Claude-style envelope, since the on-the-wire
// ACP schema has not been confirmed against a real capture for most CLIs.
func handleAcpSessionMessage(params json.RawMessage, ch chan<- Message, output *strings.Builder, usageMap *map[string]TokenUsage, fallbackModel string, sessionID *string) {
	if len(params) == 0 {
		return
	}
	var p acpSessionMessageParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if p.SessionID != "" {
		*sessionID = p.SessionID
	}

	// Nested envelope: {message:{content:[{type,text,...}]}}.
	if len(p.Message) > 0 {
		var env acpMessageEnvelope
		if err := json.Unmarshal(p.Message, &env); err == nil && len(env.Content) > 0 {
			for _, block := range env.Content {
				handleAcpContentBlock(block, ch, output)
			}
			return
		}
	}

	// Direct shape.
	switch p.Type {
	case "text":
		if text := acpStringContent(p.Text, p.Content); text != "" {
			output.WriteString(text)
			trySend(ch, Message{Type: MessageText, Content: text})
		}
	case "thinking":
		if text := acpStringContent(p.Text, p.Content); text != "" {
			trySend(ch, Message{Type: MessageThinking, Content: text})
		}
	case "tool_use":
		var input map[string]any
		if len(p.Input) > 0 {
			_ = json.Unmarshal(p.Input, &input)
		}
		trySend(ch, Message{
			Type:   MessageToolUse,
			Tool:   p.Tool,
			CallID: p.ID,
			Input:  input,
		})
	case "tool_result":
		out := acpStringContent("", p.Output)
		trySend(ch, Message{
			Type:   MessageToolResult,
			CallID: p.ToolUseID,
			Output: out,
		})
	}
}

// handleAcpContentBlock unpacks one block of a nested message content array.
// Mirrors qwen.go's handleAssistant / handleUser so the daemon's event
// surface is uniform across the stream-json and ACP backends.
func handleAcpContentBlock(block acpContentBlock, ch chan<- Message, output *strings.Builder) {
	switch block.Type {
	case "text":
		if block.Text != "" {
			output.WriteString(block.Text)
			trySend(ch, Message{Type: MessageText, Content: block.Text})
		}
	case "thinking":
		if block.Text != "" {
			trySend(ch, Message{Type: MessageThinking, Content: block.Text})
		}
	case "tool_use":
		var input map[string]any
		if len(block.Input) > 0 {
			_ = json.Unmarshal(block.Input, &input)
		}
		trySend(ch, Message{
			Type:   MessageToolUse,
			Tool:   block.Name,
			CallID: block.ID,
			Input:  input,
		})
	case "tool_result":
		out := ""
		if len(block.Content) > 0 {
			// Keep the raw JSON bytes when the content is structured, mirroring
			// claude.go / qwen.go's handleUser behaviour.
			out = string(block.Content)
			var s string
			if json.Unmarshal(block.Content, &s) == nil {
				out = s
			}
		}
		trySend(ch, Message{
			Type:   MessageToolResult,
			CallID: block.ToolUseID,
			Output: out,
		})
	}
}

// handleAcpFinished captures the terminal session id and usage from a
// session/finished notification. The usage here is authoritative and
// overrides any incremental accumulation from session/message.
func handleAcpFinished(params json.RawMessage, sessionID *string, usageMap *map[string]TokenUsage, fallbackModel string) {
	if len(params) == 0 {
		return
	}
	var p acpSessionFinishedParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if p.SessionID != "" {
		*sessionID = p.SessionID
	}
	if u := acpExtractUsage(p.Usage, fallbackModel); u != nil {
		*usageMap = u
	}
}

// acpExtractUsage projects a session/finished usage block into the shared
// per-model TokenUsage map. Tries the per-model map shape first
// ({model:{inputTokens,...}}); when that yields no tokens, falls back to
// the flat shape keyed by fallbackModel. Returns nil when neither carries
// non-zero tokens, so the caller's incremental accumulation survives.
func acpExtractUsage(raw json.RawMessage, fallbackModel string) map[string]TokenUsage {
	if len(raw) == 0 {
		return nil
	}
	// Per-model map: {model-name: {inputTokens, outputTokens, ...}}.
	var perModel map[string]acpUsageBlock
	if err := json.Unmarshal(raw, &perModel); err == nil && len(perModel) > 0 {
		out := make(map[string]TokenUsage, len(perModel))
		for model, u := range perModel {
			if model == "" || !acpUsageHasTokens(u) {
				continue
			}
			out[model] = TokenUsage{
				InputTokens:      u.InputTokens,
				OutputTokens:      u.OutputTokens,
				CacheReadTokens:   u.CacheReadInputTokens,
				CacheWriteTokens:  u.CacheCreationInputTokens,
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	// Flat shape: {inputTokens, outputTokens, ...} keyed by fallbackModel.
	var flat acpUsageBlock
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil
	}
	if fallbackModel == "" || !acpUsageHasTokens(flat) {
		return nil
	}
	return map[string]TokenUsage{
		fallbackModel: {
			InputTokens:     flat.InputTokens,
			OutputTokens:    flat.OutputTokens,
			CacheReadTokens:  flat.CacheReadInputTokens,
			CacheWriteTokens: flat.CacheCreationInputTokens,
		},
	}
}

func acpUsageHasTokens(u acpUsageBlock) bool {
	return u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheReadInputTokens > 0 || u.CacheCreationInputTokens > 0
}

// acpStringContent resolves the textual content of a direct-shape
// session/message block. prefers an explicit text field; falls back to
// unmarshalling content as a JSON string. Returns "" when neither yields
// text.
func acpStringContent(text string, content json.RawMessage) string {
	if text != "" {
		return text
	}
	if len(content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	return ""
}

// extractAcpError pulls a human-readable message out of a session/error
// notification, tolerating both a top-level message and a nested error
// object (which may itself be a string or carry its own message field).
func extractAcpError(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var p acpSessionErrorParams
	if err := json.Unmarshal(params, &p); err == nil {
		if p.Message != "" {
			return p.Message
		}
		if len(p.Error) > 0 {
			if msg := acpNestedErrorMessage(p.Error); msg != "" {
				return msg
			}
			return strings.TrimSpace(string(p.Error))
		}
	}
	return ""
}

// acpNestedErrorMessage tries to extract a message string from a nested
// error object: either an {message:"..."} object or a bare JSON string.
func acpNestedErrorMessage(raw json.RawMessage) string {
	var obj struct {
		Message string `json:"message,omitempty"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Message != "" {
		return obj.Message
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// acpResponseErrorMessage formats a JSON-RPC error object carried on a
// handshake response into a single human-readable line.
func acpResponseErrorMessage(e *jsonrpcError) string {
	if e == nil {
		return "acp response carried an error"
	}
	if e.Message == "" {
		return fmt.Sprintf("acp response error (code %d)", e.Code)
	}
	return fmt.Sprintf("acp response error (code %d): %s", e.Code, e.Message)
}

// ── Args helper ──

// buildAcpArgs assembles the CLI argument vector for an ACP launch. The
// ACP entry args come from the per-CLI config; user-supplied extra/custom
// args are filtered against acpBlockedArgs so a misconfigured agent cannot
// silently outvote a flag the protocol depends on. The MCP flag is appended
// separately by Execute after the temp file is written, mirroring qwen /
// codebuddy.
//
// Model / system-prompt / max-turns / resume are intentionally NOT mapped
// to CLI flags: in ACP mode these are conveyed through the JSON-RPC
// protocol (session/prompt carries the prompt; model selection, when
// supported, is a session/new capability), not argv. Resume is conveyed
// via session/load, not a --resume flag.
func buildAcpArgs(bc acpBackendConfig, opts ExecOptions, logger *slog.Logger) []string {
	args := make([]string, 0, len(bc.acpArgs)+4)
	args = append(args, bc.acpArgs...)
	args = append(args, filterCustomArgs(opts.ExtraArgs, acpBlockedArgs, logger)...)
	args = append(args, filterCustomArgs(opts.CustomArgs, acpBlockedArgs, logger)...)
	return args
}

// withAcpStderr appends a stderr tail hint to an error message when
// non-empty, otherwise returns msg unchanged. Mirrors withClaudeStderr /
// withQwenStderr / withCodebuddyStderr; prefixed with the CLI name so a
// multi-backend failure report stays legible.
func withAcpStderr(cli, msg, tail string) string {
	if tail == "" {
		return msg
	}
	return msg + "; " + cli + " stderr: " + tail
}
