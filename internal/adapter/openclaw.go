// Package adapter — openclaw backend. Ports the openclaw stream-json adapter
// from multica (multica/server/pkg/agent/openclaw.go + the discovery helpers
// in models.go) with one round of simplification:
//
//   - Model discovery is exposed as a top-level DiscoverOpenclawAgents function
//     instead of being inlined into a separate models file; the AICLIBridge
//     adapter package owns its own model catalog. The function still does the
//     same JSON-first, text-fallback, strict-identifier strategy as the source.
//
//   - Version gating is a soft preflight: if `--version` returns a parseable
//     version below the minimum, the adapter logs a warning with an actionable
//     upgrade hint but does NOT fail the run (the check is advisory). If
//     `--version` itself errors, the adapter proceeds — the runtime version
//     field is best-effort and shouldn't block runs on a working CLI that just
//     doesn't speak --version.
//
// The resume path lives at sessionID := opts.ResumeSessionID (mirrors
// multica/server/pkg/agent/openclaw.go:71): if the caller supplied
// ResumeSessionID, the daemon reuses the previous openclaw session; otherwise
// a fresh aiclibridge-prefixed id is generated and passed via --session-id.
package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// openclawNoParseableOutput is the canonical error string surfaced when the
// adapter cannot extract any usable JSON from a run's stdout. The exact phrase
// is depended on by external log-grep / dashboard alerts; do not change it
// without also updating those consumers.
const openclawNoParseableOutput = "openclaw returned no parseable output"

// minOpenclawVersion is the lowest openclaw version that emits its --json
// result on stdout. Older builds wrote JSON to stderr and now appear to
// silently produce no output; the version check in Execute fails fast with a
// hardcoded upgrade hint so users see an actionable message instead of the
// generic "no parseable output" failure.
const minOpenclawVersion = "2026.5.5"

// openclawVersionPattern extracts a three-segment dotted version from
// arbitrary `openclaw --version` output (e.g. "openclaw 2026.5.5",
// "openclaw v2026.5.5 c37871e").
var openclawVersionPattern = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// openclawBlockedArgs are flags hardcoded by the daemon that must not be
// overridden by user-configured custom_args. Adding a flag here is the
// single source of truth: mode routing, the JSON protocol, and the resume
// session id are all owned by the daemon, not by the user.
var openclawBlockedArgs = map[string]blockedArgMode{
	"--local":         blockedStandalone, // local mode for daemon execution
	"--json":          blockedStandalone, // JSON output for daemon communication
	"--session-id":    blockedWithValue,  // managed by daemon for session resumption
	"--message":       blockedWithValue,  // prompt is set by daemon
	"--model":         blockedWithValue,  // openclaw agent does not accept --model
	"--system-prompt": blockedWithValue,  // openclaw agent does not accept --system-prompt
}

// openclawBackend implements Backend by spawning `openclaw agent --message
// <prompt> --output-format stream-json --yes` and reading streaming NDJSON
// events from stdout — the same shape as the opencode backend.
type openclawBackend struct {
	cfg Config
}

// Execute runs an openclaw agent turn and returns a streaming Session.
// See backend.go for the Backend contract.
func (b *openclawBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "openclaw"
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, fmt.Errorf("openclaw executable not found at %q: %w", execPath, err)
	}

	// Advisory version gate: warn (do not hard-fail) when the installed
	// openclaw is below minOpenclawVersion. checkOpenclawVersion is
	// best-effort — it returns nil when --version is unsupported or
	// unparseable, so a warning only fires for a confirmed-too-old build.
	if err := checkOpenclawVersion(ctx, execPath); err != nil {
		b.cfg.Logger.Warn("openclaw version check failed", "error", err)
	}

	timeout := opts.Timeout
	runCtx, cancel := runContext(ctx, timeout)

	// Resume path (mirrors multica openclaw.go:71): if the caller provided
	// a ResumeSessionID we reuse it so openclaw dials the same session; if
	// not, we generate a fresh aiclibridge-prefixed id and pass it via
	// --session-id (the daemon-managed flag, see openclawBlockedArgs).
	sessionID := opts.ResumeSessionID
	if sessionID == "" {
		sessionID = fmt.Sprintf("aiclibridge-%d", time.Now().UnixNano())
	}
	args := buildOpenclawArgs(prompt, sessionID, opts, b.cfg.Logger)

	cmd := exec.CommandContext(runCtx, execPath, args...)
	hideAgentWindow(cmd)
	configureProcessGroup(cmd)
	b.cfg.Logger.Info("agent command", "exec", execPath, "args", args)
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Env = buildEnv(b.cfg.Env)

	// openclaw writes its --json output to stdout. Stderr carries log
	// overflow (security warnings, tool errors, etc.) — capture it via a
	// log writer so it surfaces in daemon logs without being fed into the
	// JSON parser.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("openclaw stdout pipe: %w", err)
	}
	cmd.Stderr = newLogWriter(b.cfg.Logger, "[openclaw:stderr] ")

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start openclaw: %w", err)
	}

	b.cfg.Logger.Info("openclaw started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)

	// Close stdout when the context is cancelled so the scanner unblocks.
	go func() {
		<-runCtx.Done()
		_ = stdout.Close()
	}()

	go func() {
		defer cancel()
		defer close(msgCh)
		defer close(resCh)

		startTime := time.Now()
		scanResult := b.processOutput(stdout, msgCh)

		// Wait for process exit.
		exitErr := cmd.Wait()
		duration := time.Since(startTime)

		if runCtx.Err() == context.DeadlineExceeded {
			scanResult.status = "timeout"
			scanResult.errMsg = fmt.Sprintf("openclaw timed out after %s", timeout)
		} else if runCtx.Err() == context.Canceled {
			scanResult.status = "aborted"
			scanResult.errMsg = "execution cancelled"
		} else if exitErr != nil && scanResult.status == "completed" {
			scanResult.status = "failed"
			scanResult.errMsg = fmt.Sprintf("openclaw exited with error: %v", exitErr)
		}

		b.cfg.Logger.Info("openclaw finished", "pid", cmd.Process.Pid, "status", scanResult.status, "duration", duration.Round(time.Millisecond).String())

		// Build usage map. Prefer the model openclaw reported in
		// `meta.agentMeta.model` (the actual LLM, e.g. `deepseek-chat`).
		// Fall back to opts.Model — which for openclaw is the agent name
		// passed via `--agent`, not a real model identifier — only when
		// the runtime didn't surface its own model. Last resort is the
		// daemon's `unknown` placeholder.
		var usage map[string]TokenUsage
		u := scanResult.usage
		if u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheReadTokens > 0 || u.CacheWriteTokens > 0 {
			model := scanResult.model
			if model == "" {
				model = opts.Model
			}
			if model == "" {
				model = "unknown"
			}
			usage = map[string]TokenUsage{model: u}
		}

		resCh <- Result{
			Status:     scanResult.status,
			Output:     scanResult.output,
			Error:      scanResult.errMsg,
			DurationMs: duration.Milliseconds(),
			SessionID:  scanResult.sessionID,
			Usage:      usage,
		}
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
}

// buildOpenclawArgs assembles the argv for a one-shot `openclaw agent`
// invocation.
//
// The CLI only accepts --local, --json, --session-id, --timeout, --message
// (and flags like --agent / --channel that users pass through CustomArgs).
// Notably it does NOT accept --model or --system-prompt — model is bound at
// agent registration time via `openclaw agents add/update --model`, and
// instructions must be injected inline into --message because openclaw loads
// AGENTS.md from its own workspace directory, not from cwd.
//
// Routing: `openclaw agent` defaults to Gateway routing; --local is the
// embedded-mode opt-in. The daemon historically forced --local so every
// run executed in-process on the daemon host. When opts.OpenclawMode ==
// "gateway" the daemon drops --local so openclaw dials its configured
// Gateway instead. --local stays in openclawBlockedArgs so users cannot
// smuggle it back in via custom_args under gateway mode.
func buildOpenclawArgs(prompt, sessionID string, opts ExecOptions, logger *slog.Logger) []string {
	args := []string{"agent"}
	if opts.OpenclawMode != "gateway" {
		args = append(args, "--local")
	}
	args = append(args, "--json", "--session-id", sessionID)
	if opts.Timeout > 0 {
		args = append(args, "--timeout", fmt.Sprintf("%d", int(opts.Timeout.Seconds())))
	}
	customArgs := filterCustomArgs(opts.CustomArgs, openclawBlockedArgs, logger)
	// OpenClaw binds models to pre-registered agents at `openclaw agents
	// add/update --model` time; the daemon selects one at runtime by
	// passing --agent <id>. Only inject when the user hasn't already set
	// --agent via custom_args — custom_args wins for backward
	// compatibility with existing configs.
	if opts.Model != "" && !customArgsContains(customArgs, "--agent") {
		args = append(args, "--agent", opts.Model)
	}
	args = append(args, customArgs...)

	if opts.SystemPrompt != "" {
		prompt = opts.SystemPrompt + "\n\n" + prompt
	}
	args = append(args, "--message", prompt)
	return args
}

// customArgsContains reports whether args contains the given flag
// (either as a standalone token "--flag" or in "--flag=value" form).
func customArgsContains(args []string, flag string) bool {
	prefix := flag + "="
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

// ── Event handlers ──

// openclawEventResult holds accumulated state from processing the event stream.
type openclawEventResult struct {
	status    string
	errMsg    string
	output    string
	sessionID string
	usage     TokenUsage
	// model is the LLM identifier reported by openclaw in its result blob
	// (`meta.agentMeta.model`). Empty when the run did not emit it.
	model string
}

// processOutput reads the JSON output from openclaw --json stdout and
// returns the parsed result. OpenClaw writes its JSON output to stdout;
// stderr carries log overflow and is captured separately by the caller.
//
// Two paths:
//
//   - A single pretty-printed JSON result blob (the format openclaw 2026.5.x
//     emits today).
//   - NDJSON streaming events (type: "text", "tool_use", "tool_result",
//     "error", "step_start", "step_finish", "lifecycle") — supported for
//     forward compatibility and shared with other backends.
//
// Implementation note: we read the full buffer first and try a single
// whole-buffer parse against the final-result schema, then fall through to
// the line-by-line NDJSON scanner. This makes the dominant happy path
// (one pretty-printed JSON blob) deterministic while keeping NDJSON event
// support intact.
func (b *openclawBackend) processOutput(r io.Reader, ch chan<- Message) openclawEventResult {
	buf, readErr := io.ReadAll(r)
	if readErr != nil {
		return openclawEventResult{status: "failed", errMsg: fmt.Sprintf("read stdout: %v", readErr)}
	}

	// Whole-buffer fast path: openclaw 2026.5.x emits a single pretty-printed
	// JSON result blob. Try parsing the entire buffer (after trimming
	// whitespace and any preceding non-JSON log lines) as the final-result
	// schema.
	if result, ok := parseWholeBufferOpenclawResult(buf); ok {
		var output strings.Builder
		return b.buildOpenclawEventResult(result, ch, &output)
	}

	// Fall-back path: NDJSON line scanner.
	scanner := bufio.NewScanner(bytes.NewReader(buf))
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	var output strings.Builder
	var sessionID string
	var model string
	var usage TokenUsage
	finalStatus := "completed"
	var finalError string
	gotEvents := false

	var rawLines []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// Try parsing as a streaming NDJSON event first.
		if event, ok := tryParseOpenclawEvent(line); ok {
			gotEvents = true
			if event.SessionID != "" {
				sessionID = event.SessionID
			}
			switch event.Type {
			case "text":
				if event.Text != "" {
					output.WriteString(event.Text)
					trySend(ch, Message{Type: MessageText, Content: event.Text})
				}
			case "tool_use":
				var input map[string]any
				if event.Input != nil {
					_ = json.Unmarshal(event.Input, &input)
				}
				trySend(ch, Message{
					Type:   MessageToolUse,
					Tool:   event.Tool,
					CallID: event.CallID,
					Input:  input,
				})
			case "tool_result":
				trySend(ch, Message{
					Type:   MessageToolResult,
					Tool:   event.Tool,
					CallID: event.CallID,
					Output: event.Text,
				})
			case "error":
				errMsg := event.errorMessage()
				b.cfg.Logger.Warn("openclaw error event", "error", errMsg)
				trySend(ch, Message{Type: MessageError, Content: errMsg})
				finalStatus = "failed"
				finalError = errMsg
			case "lifecycle":
				phase := event.Phase
				if phase == "error" || phase == "failed" || phase == "cancelled" {
					errMsg := event.errorMessage()
					b.cfg.Logger.Warn("openclaw lifecycle failure", "phase", phase, "error", errMsg)
					trySend(ch, Message{Type: MessageError, Content: errMsg})
					finalStatus = "failed"
					finalError = errMsg
				}
			case "step_start":
				trySend(ch, Message{Type: MessageStatus, Status: "running"})
			case "step_finish":
				if event.Usage != nil {
					u := parseOpenclawUsage(event.Usage)
					usage.InputTokens += u.InputTokens
					usage.OutputTokens += u.OutputTokens
					usage.CacheReadTokens += u.CacheReadTokens
					usage.CacheWriteTokens += u.CacheWriteTokens
				}
			}
			continue
		}

		// Try parsing as a final result blob (legacy format).
		if result, ok := tryParseOpenclawResult(line); ok {
			gotEvents = true
			res := b.buildOpenclawEventResult(result, ch, &output)
			if res.sessionID != "" {
				sessionID = res.sessionID
			}
			if res.model != "" {
				model = res.model
			}
			u := res.usage
			if u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheReadTokens > 0 || u.CacheWriteTokens > 0 {
				usage = u
			}
			continue
		}

		// Not JSON — treat as log line.
		b.cfg.Logger.Debug("[openclaw:stdout] " + line)
		rawLines = append(rawLines, line)
	}

	if err := scanner.Err(); err != nil {
		return openclawEventResult{status: "failed", errMsg: fmt.Sprintf("read stdout: %v", err)}
	}

	if !gotEvents {
		trimmed := strings.TrimSpace(strings.Join(rawLines, "\n"))
		if trimmed != "" {
			return openclawEventResult{status: "completed", output: trimmed}
		}
		return openclawEventResult{
			status: "failed",
			errMsg: openclawNoParseableOutput,
		}
	}

	return openclawEventResult{
		status:    finalStatus,
		errMsg:    finalError,
		output:    output.String(),
		sessionID: sessionID,
		usage:     usage,
		model:     model,
	}
}

// parseWholeBufferOpenclawResult attempts to parse the entire stdout buffer
// as a single openclaw final-result JSON blob.
func parseWholeBufferOpenclawResult(buf []byte) (openclawResult, bool) {
	trimmed := strings.TrimSpace(string(buf))
	if trimmed == "" {
		return openclawResult{}, false
	}
	if result, ok := tryParseOpenclawResult(trimmed); ok {
		return result, true
	}
	// Strip any leading log lines that precede the JSON blob.
	lines := strings.Split(trimmed, "\n")
	for i, line := range lines {
		if len(line) > 0 && line[0] == '{' {
			candidate := strings.TrimSpace(strings.Join(lines[i:], "\n"))
			if result, ok := tryParseOpenclawResult(candidate); ok {
				return result, true
			}
			return openclawResult{}, false
		}
	}
	return openclawResult{}, false
}

// tryParseOpenclawEvent attempts to parse a line as a streaming NDJSON event.
func tryParseOpenclawEvent(line string) (openclawEvent, bool) {
	if len(line) == 0 || line[0] != '{' {
		return openclawEvent{}, false
	}
	var event openclawEvent
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return openclawEvent{}, false
	}
	if event.Type == "" {
		return openclawEvent{}, false
	}
	return event, true
}

// tryParseOpenclawResult attempts to parse a line as a final result blob.
// Lines must start with '{' to be considered.
func tryParseOpenclawResult(raw string) (openclawResult, bool) {
	if len(raw) == 0 || raw[0] != '{' {
		return openclawResult{}, false
	}
	var result openclawResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return openclawResult{}, false
	}
	if result.Payloads == nil && result.Meta.DurationMs == 0 {
		return openclawResult{}, false
	}
	return result, true
}

// buildOpenclawEventResult extracts text and metadata from a final result
// blob. Text payloads are appended to the shared output builder and emitted
// to ch.
func (b *openclawBackend) buildOpenclawEventResult(result openclawResult, ch chan<- Message, output *strings.Builder) openclawEventResult {
	for _, p := range result.Payloads {
		if p.Text != "" {
			output.WriteString(p.Text)
			trySend(ch, Message{Type: MessageText, Content: p.Text})
		}
	}

	var sessionID string
	var model string
	var usage TokenUsage
	if result.Meta.AgentMeta != nil {
		if sid, ok := result.Meta.AgentMeta["sessionId"].(string); ok {
			sessionID = sid
		}
		// `meta.agentMeta.model` is openclaw's true LLM identifier
		// (e.g. "deepseek-chat", "claude-sonnet-4"). Take it as-is.
		if m, ok := result.Meta.AgentMeta["model"].(string); ok {
			model = strings.TrimSpace(m)
		}
		if u, ok := result.Meta.AgentMeta["usage"].(map[string]any); ok {
			usage = parseOpenclawUsage(u)
		}
	}

	return openclawEventResult{
		status:    "completed",
		output:    output.String(),
		sessionID: sessionID,
		usage:     usage,
		model:     model,
	}
}

// parseOpenclawUsage extracts token usage from a map, supporting multiple
// field name conventions used by different OpenClaw versions.
func parseOpenclawUsage(data map[string]any) TokenUsage {
	return TokenUsage{
		InputTokens:      openclawInt64FirstOf(data, "input", "inputTokens", "input_tokens"),
		OutputTokens:     openclawInt64FirstOf(data, "output", "outputTokens", "output_tokens"),
		CacheReadTokens:  openclawInt64FirstOf(data, "cacheRead", "cachedInputTokens", "cached_input_tokens", "cache_read", "cache_read_input_tokens"),
		CacheWriteTokens: openclawInt64FirstOf(data, "cacheWrite", "cacheCreationInputTokens", "cache_creation_input_tokens", "cache_write"),
	}
}

func openclawInt64FirstOf(data map[string]any, keys ...string) int64 {
	for _, key := range keys {
		if v := openclawInt64(data, key); v != 0 {
			return v
		}
	}
	return 0
}

func openclawInt64(data map[string]any, key string) int64 {
	v, ok := data[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	default:
		return 0
	}
}

// ── JSON types for `openclaw agent --json` output ──

// openclawEvent represents a single streaming NDJSON event from
// openclaw --json.
type openclawEvent struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId,omitempty"`
	Text      string          `json:"text,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	CallID    string          `json:"callId,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Usage     map[string]any  `json:"usage,omitempty"`
	Phase     string          `json:"phase,omitempty"`
	Error     *openclawError  `json:"error,omitempty"`
	Message   string          `json:"message,omitempty"`
}

func (e openclawEvent) errorMessage() string {
	if e.Error != nil {
		if msg := e.Error.message(); msg != "" {
			return msg
		}
	}
	if e.Text != "" {
		return e.Text
	}
	if e.Message != "" {
		return e.Message
	}
	return "unknown openclaw error"
}

type openclawError struct {
	Name    string             `json:"name,omitempty"`
	Data    *openclawErrorData `json:"data,omitempty"`
	Message string             `json:"message,omitempty"`
}

func (e *openclawError) message() string {
	if e.Data != nil && e.Data.Message != "" {
		return e.Data.Message
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Name != "" {
		return e.Name
	}
	return ""
}

type openclawErrorData struct {
	Message string `json:"message,omitempty"`
}

// openclawResult is the final JSON output from `openclaw agent --json`.
type openclawResult struct {
	Payloads []openclawPayload `json:"payloads"`
	Meta     openclawMeta      `json:"meta"`
}

type openclawPayload struct {
	Text string `json:"text"`
}

type openclawMeta struct {
	DurationMs int64          `json:"durationMs"`
	AgentMeta  map[string]any `json:"agentMeta"`
}

// ── Model discovery ──

// openclawModelProvider is the runtime-side provider name surfaced in the
// Model.Provider field for any openclaw-sourced entry. Keeps the dashboard
// grouping consistent with the multica reference.
const openclawModelProvider = "openclaw"

// DiscoverOpenclawAgents enumerates the pre-registered OpenClaw agents
// (where model selection actually lives — each agent is bound to a model
// at `agents add` time). It tries structured JSON output first, falling
// back to a conservative text parser that rejects TUI decoration and
// section headers. On any ambiguity the function returns an empty list
// and a nil error; a silently-wrong enumeration would be worse than none.
func DiscoverOpenclawAgents(ctx context.Context, executablePath string) ([]Model, error) {
	if executablePath == "" {
		executablePath = "openclaw"
	}
	if _, err := exec.LookPath(executablePath); err != nil {
		return []Model{}, nil
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Try JSON modes first. Different openclaw builds expose the flag
	// under different names; trying a couple is cheap.
	for _, jsonArgs := range [][]string{
		{"agents", "list", "--json"},
		{"agents", "list", "--output", "json"},
		{"agents", "list", "-o", "json"},
	} {
		cmd := exec.CommandContext(runCtx, executablePath, jsonArgs...)
		hideAgentWindow(cmd)
		out, err := cmd.Output()
		if err != nil && len(out) == 0 {
			continue
		}
		if models, ok := parseOpenclawAgentsJSON(out); ok {
			return models, nil
		}
	}

	// Text fallback. Be strict — the default output is a decorated
	// banner with box-drawing and section headers.
	cmd := exec.CommandContext(runCtx, executablePath, "agents", "list")
	hideAgentWindow(cmd)
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return []Model{}, nil
	}
	return parseOpenclawAgents(string(out)), nil
}

// Model describes a single LLM model exposed by an agent provider.
// Provider groups entries in the UI; Default badges the runtime-preferred pick.
type Model struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Provider string `json:"provider,omitempty"`
	Default  bool   `json:"default,omitempty"`
}

// openclawAgentEntry is the shape parseOpenclawAgentsJSON expects from
// `openclaw agents list --json`. `id` is the routing key passed to
// `openclaw agent --agent <id>`; `name` is the human display label. The
// two are not interchangeable — see openclawEntriesToModels for the
// mapping. Older openclaw versions may emit only `name`; in that case we
// fall back to using it as the id for backward compatibility. `model`
// is optional and only used to enrich the dropdown label.
type openclawAgentEntry struct {
	Name  string `json:"name"`
	ID    string `json:"id"`
	Model string `json:"model"`
}

// parseOpenclawAgentsJSON accepts `openclaw agents list --json`-style
// output. It handles two common shapes: a top-level array, or an object
// with an `agents` key whose value is an array. Returns ok=false if the
// input isn't valid JSON in either shape.
func parseOpenclawAgentsJSON(raw []byte) ([]Model, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, false
	}

	var flat []openclawAgentEntry
	if err := json.Unmarshal(raw, &flat); err == nil {
		return openclawEntriesToModels(flat), true
	}

	var wrapped struct {
		Agents []openclawAgentEntry `json:"agents"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Agents != nil {
		return openclawEntriesToModels(wrapped.Agents), true
	}

	return nil, false
}

func openclawEntriesToModels(entries []openclawAgentEntry) []Model {
	models := make([]Model, 0, len(entries))
	seen := map[string]bool{}
	for _, e := range entries {
		// Use ID as the model identifier because openclaw resolves
		// --agent by id, not by display name. Names may contain spaces
		// which openclaw's normalizeAgentId would mangle, causing
		// lookup misses.
		id := e.ID
		if id == "" {
			id = e.Name
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		displayName := e.Name
		if displayName == "" {
			displayName = id
		}
		label := displayName
		if e.Model != "" {
			label = displayName + " (" + e.Model + ")"
		}
		models = append(models, Model{ID: id, Label: label, Provider: openclawModelProvider})
	}
	return models
}

// parseOpenclawAgents extracts agent names from the text output of
// `openclaw agents list`. The default CLI output is a decorated banner —
// section headers ending in `:`, box-drawing characters, and
// single-character icons — so we only accept lines that look like a proper
// `<name> <model>` row: at least two whitespace-separated tokens, both
// made of safe identifier characters, and neither ending in `:`.
func parseOpenclawAgents(output string) []Model {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var models []Model
	seen := map[string]bool{}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name, model := fields[0], fields[1]
		if !isOpenclawIdentifier(name) || !isOpenclawIdentifier(model) {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		models = append(models, Model{
			ID:       name,
			Label:    name + " (" + model + ")",
			Provider: openclawModelProvider,
		})
	}
	return models
}

// isOpenclawIdentifier reports whether s looks like a valid agent-name or
// model-id token: starts with a letter, contains only identifier-safe
// characters, and isn't a section header (trailing colon). Rejects TUI
// decoration like `│`, `╭`, `◇`, `|`.
func isOpenclawIdentifier(s string) bool {
	if s == "" || strings.HasSuffix(s, ":") {
		return false
	}
	first := s[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z')) {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '/':
		default:
			return false
		}
	}
	return true
}

// ── Version gate ──

// checkOpenclawVersion runs `<execPath> --version` and returns a
// user-facing error when the installed openclaw is older than
// minOpenclawVersion. The check is best-effort: if --version itself
// errors, the adapter proceeds (a working openclaw that just doesn't
// speak --version shouldn't block the user's run).
func checkOpenclawVersion(ctx context.Context, execPath string) error {
	cmd := exec.CommandContext(ctx, execPath, "--version")
	hideAgentWindow(cmd)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil // best-effort: don't block the run
	}
	detected, ok := parseOpenclawVersion(string(out))
	if !ok {
		return nil
	}
	if compareOpenclawVersion(detected, minOpenclawVersion) < 0 {
		return fmt.Errorf("openclaw %s is below the minimum supported version %s. Run `openclaw update` to upgrade and try again.", detected, minOpenclawVersion)
	}
	return nil
}

// parseOpenclawVersion extracts the first three-segment dotted version
// from arbitrary `openclaw --version` output. Returns ok=false when no
// match is found.
func parseOpenclawVersion(raw string) (string, bool) {
	m := openclawVersionPattern.FindString(raw)
	if m == "" {
		return "", false
	}
	return m, true
}

// compareOpenclawVersion compares two three-segment dotted versions
// numerically. Returns -1, 0, or +1 like bytes.Compare.
func compareOpenclawVersion(a, b string) int {
	aParts := strings.SplitN(a, ".", 3)
	bParts := strings.SplitN(b, ".", 3)
	for i := 0; i < 3; i++ {
		ai, _ := strconv.Atoi(aParts[i])
		bi, _ := strconv.Atoi(bParts[i])
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
	}
	return 0
}
