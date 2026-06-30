// Package adapter — codebuddy backend.
//
// codebuddyBackend implements Backend by spawning
// `codebuddy --bare --output-format stream-json --input-format stream-json --yolo`
// and driving a stream-json conversation over stdin/stdout. The prompt is
// delivered as a single user-turn NDJSON frame on stdin; codebuddy emits
// assistant / user / system / result / control_request events on stdout,
// one JSON object per line.
//
// codebuddy (Tencent CodeBuddy CLI) and qwen-code share the same Claude
// Code SDK lineage, and codebuddy --help (v2.113.0) exposes the identical
// stream-json flag set as qwen (--output-format stream-json,
// --input-format stream-json, --include-partial-messages, --yolo,
// -c/--continue, -r/--resume [id], --session-id, --fork-session,
// --mcp-config <file>, --strict-mcp-config, --model, --permission-mode).
//
// Schema note: ASSUMPTION, not yet verified against codebuddy source. The
// stream-json output schema is assumed to be the same Claude Code SDK
// contract as qwen — events carry `type`, `subtype`, `session_id`
// (snake_case), `is_error`, `duration_ms`, `num_turns`, `usage`, `result`
// or `error`, and assistant messages wrap a `message.{content[],usage}`
// sub-object — the same shape claude.go / qwen.go parse. We therefore mirror
// the qwen backend's parsing structure (handleAssistant/handleUser/
// handleResult/handleSystem) and keep the types and handlers
// codebuddy-prefixed and self-contained in this file so a future codebuddy
// schema drift can be fixed without touching qwen.go or claude.go. The
// stream-json output has NOT been exercised against a real codebuddy
// process in this task (the binary is present on the dev machine but no
// smoke round-trip was captured); correctness rests on the shared-SDK
// assumption above until a smoke test confirms it.
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

// ── Blocked args ──
//
// codebuddyBlockedArgs are flags hardcoded by the daemon that must not be
// overridden by user-configured custom_args. Overriding these would break
// the daemon↔codebuddy stream-json protocol contract (NDJSON I/O, YOLO
// autonomous mode, MCP injection, session id pinning, model selection,
// system-prompt injection). Mirrors claudeBlockedArgs / qwenBlockedArgs —
// same generic filterCustomArgs helper, codebuddy-specific set. The flag
// set is identical to qwenBlockedArgs because codebuddy exposes the same
// Claude Code SDK flag surface as qwen-code.
var codebuddyBlockedArgs = map[string]blockedArgMode{
	"--bare":                 blockedStandalone, // minimal mode, daemon-managed
	"-p":                     blockedWithValue,  // deprecated non-interactive prompt flag (-p <text>)
	"--prompt":               blockedWithValue,  // deprecated non-interactive prompt flag
	"--output-format":        blockedWithValue,  // stream-json protocol
	"--input-format":         blockedWithValue,  // stream-json protocol
	"--yolo":                 blockedStandalone, // daemon manages autonomous approval
	"--approval-mode":        blockedWithValue,  // owned by --yolo above
	"--mcp-config":           blockedWithValue,  // set by daemon from agent.mcp_config
	"--session-id":           blockedWithValue,  // daemon-managed session pinning
	"-m":                     blockedWithValue,  // model is daemon-managed via opts.Model
	"--model":                blockedWithValue,  // long form of -m
	"--system-prompt":        blockedWithValue,  // daemon-managed system prompt injection
	"--append-system-prompt": blockedWithValue,  // daemon-managed system prompt injection
}

// codebuddyBackend implements Backend by spawning the codebuddy CLI in
// stream-json headless mode. See the package doc above for the wire-format
// rationale.
type codebuddyBackend struct {
	cfg Config
}

// Execute runs a single codebuddy turn. The prompt is written to stdin as one
// user-turn NDJSON frame; stdout is scanned line-by-line for stream-json
// events. The function returns immediately with a Session whose channels
// are drained by a background goroutine that lives until codebuddy exits.
func (b *codebuddyBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "codebuddy"
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, fmt.Errorf("codebuddy executable not found at %q: %w", execPath, err)
	}

	timeout := opts.Timeout
	runCtx, cancel := runContext(ctx, timeout)

	args := buildCodebuddyArgs(opts, b.cfg.Logger)

	// MCP config: codebuddy supports `--mcp-config <path|inline-json>`
	// natively (same as qwen / claude). We reuse the shared
	// writeMcpConfigToTemp helper from claude.go so the temp-file lifecycle
	// and cleanup contract are identical across the three backends.
	var mcpConfigPath string
	var mcpFileCleanup func() // non-nil while this function owns the temp file
	if len(opts.McpConfig) > 0 {
		path, err := writeMcpConfigToTemp(opts.McpConfig)
		if err != nil {
			cancel()
			return nil, err
		}
		mcpConfigPath = path
		mcpFileCleanup = func() { os.Remove(mcpConfigPath) }
		args = append(args, "--mcp-config", mcpConfigPath)
	}
	// Clean up the temp file if we return before the goroutine takes ownership.
	defer func() {
		if mcpFileCleanup != nil {
			mcpFileCleanup()
		}
	}()

	cmd := exec.CommandContext(runCtx, execPath, args...)
	hideAgentWindow(cmd)
	configureProcessGroup(cmd)
	b.cfg.Logger.Info("agent command", "exec", execPath, "args", args)
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	env := buildEnv(b.cfg.Env)
	if opts.Cwd != "" {
		// Override PWD so codebuddy resolves its discovery root to the task
		// workdir (cmd.Dir alone is not enough; codebuddy reads PWD before
		// falling back to process.cwd()).
		env = append(env, "PWD="+opts.Cwd)
	}
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("codebuddy stdout pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("codebuddy stdin pipe: %w", err)
	}
	var closeStdinOnce sync.Once
	closeStdin := func() { closeStdinOnce.Do(func() { _ = stdin.Close() }) }
	// Capture stderr into both the daemon log and a bounded tail buffer so
	// we can include the last few KB in Result.Error when codebuddy exits
	// unexpectedly (e.g. node panic, auth failure, EPERM). Without the tail
	// an exit-code-only failure looks like "codebuddy exited with error:
	// exit status 1" — useless for root-causing.
	stderrBuf := newStderrTail(newLogWriter(b.cfg.Logger, "[codebuddy:stderr] "), 0)
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		closeStdin()
		cancel()
		return nil, fmt.Errorf("start codebuddy: %w", err)
	}

	b.cfg.Logger.Info("codebuddy started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

	// cmd.Start() succeeded — transfer temp file ownership to the goroutine.
	mcpFileCleanup = nil

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)

	// writeCodebuddyInput runs in its own goroutine so it cannot deadlock
	// against the stdout reader. codebuddy's stream-json mode emits a
	// system/init event before reading its first stdin frame; if nothing is
	// draining stdout while we write the prompt, codebuddy blocks writing
	// stdout, never reads stdin, and our Write blocks until runCtx fires —
	// same failure mode claude / qwen have. Keep stdin open after the
	// initial user message so codebuddy's control_request frames can be
	// answered on the same input stream.
	writeDone := make(chan error, 1)
	go func() {
		err := writeCodebuddyInput(stdin, prompt)
		if err != nil {
			closeStdin()
		}
		writeDone <- err
	}()

	go func() {
		defer cancel()
		defer close(msgCh)
		defer close(resCh)
		if mcpConfigPath != "" {
			defer os.Remove(mcpConfigPath)
		}

		startTime := time.Now()
		fallbackModel := opts.Model
		if fallbackModel == "" {
			fallbackModel = "unknown"
		}
		scanResult := b.processCodebuddyEvents(stdout, msgCh, stdin, closeStdin, fallbackModel)
		exitErr := cmd.Wait()
		duration := time.Since(startTime)
		writeErr := <-writeDone

		status := scanResult.status
		errMsg := scanResult.errMsg

		switch {
		case runCtx.Err() == context.DeadlineExceeded:
			status = "timeout"
			errMsg = fmt.Sprintf("codebuddy timed out after %s", timeout)
		case runCtx.Err() == context.Canceled:
			status = "aborted"
			errMsg = "execution cancelled"
		case writeErr != nil && status == "completed" && scanResult.sessionID == "":
			// No result event landed and the prompt write failed — codebuddy
			// died before reading the prompt. Surface the write error; the
			// stderr tail attached below carries the real reason.
			status = "failed"
			errMsg = fmt.Sprintf("write codebuddy input: %v", writeErr)
		case exitErr != nil && status == "completed":
			status = "failed"
			errMsg = fmt.Sprintf("codebuddy exited with error: %v", exitErr)
		}

		// cmd.Wait() has returned — os/exec's stderr copy goroutine has
		// observed every byte codebuddy wrote to stderr before exiting, so
		// stderrBuf.Tail() is safe to sample now. Attach the tail to any
		// non-empty failure message so callers see the real reason instead
		// of a bare exit code.
		if errMsg != "" {
			errMsg = withCodebuddyStderr(errMsg, stderrBuf.Tail())
		}

		b.cfg.Logger.Info("codebuddy finished", "pid", cmd.Process.Pid, "status", status, "duration", duration.Round(time.Millisecond).String())

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

// ── Event handlers ──

// codebuddyEventResult accumulates the terminal state extracted from a
// stream-json stdout scan: final status, error message, accumulated text
// output, session id, and token usage (per-model map). When the result
// frame carries authoritative usage it overrides the incremental
// accumulation from assistant messages; when no result frame arrives
// (e.g. codebuddy crashes first) the incremental map is the only usage
// source.
type codebuddyEventResult struct {
	status    string
	errMsg    string
	output    string
	sessionID string
	usage     map[string]TokenUsage
}

// processCodebuddyEvents reads NDJSON lines from r, dispatches events to ch,
// answers control_request frames on stdin, and returns the accumulated
// terminal result. Extracted from Execute for testability — the pure
// parsing path can be exercised without spawning a real codebuddy process.
//
// fallbackModel is the model name used to key the result frame's usage
// when codebuddy does not emit a per-model modelUsage map (the common case).
// Usually opts.Model; "unknown" when the caller did not pin a model.
//
// closeStdin is invoked when a result event arrives so codebuddy can drain
// its shutdown path promptly; the caller still owns the final cmd.Wait.
func (b *codebuddyBackend) processCodebuddyEvents(r io.Reader, ch chan<- Message, stdin io.Writer, closeStdin func(), fallbackModel string) codebuddyEventResult {
	var output strings.Builder
	var sessionID string
	var usageMap map[string]TokenUsage
	finalStatus := "completed"
	var finalError string

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var event codebuddySDKMessage
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			// Not a stream-json line — codebuddy may print a YOLO warning or
			// other banner to stdout before the first event. Silently
			// skip; the stderr tail captures diagnostics for failures.
			continue
		}

		if event.SessionID != "" {
			sessionID = event.SessionID
		}

		switch event.Type {
		case "assistant":
			b.handleCodebuddyAssistant(event, ch, &output, &usageMap)
		case "user":
			b.handleCodebuddyUser(event, ch)
		case "system":
			// system messages carry session_id at top level (already
			// captured above); subtype init/session_start/session_end
			// are informational. Emit a running status so streaming
			// consumers see activity.
			trySend(ch, Message{Type: MessageStatus, Status: "running", SessionID: sessionID})
		case "result":
			sessionID = event.SessionID
			if event.ResultText != "" {
				// Replace accumulated streaming text with the final
				// result text — codebuddy's result frame is authoritative.
				output.Reset()
				output.WriteString(event.ResultText)
			}
			// The result frame's usage is authoritative and overrides
			// the incremental accumulation from assistant messages
			// (mirrors claude.go's claudeResultUsage / qwen.go's
			// qwenResultUsage contract). When codebuddy emits modelUsage
			// (per-model), that wins; otherwise the flat usage block is
			// keyed by fallbackModel. When neither is present, the
			// incremental map survives.
			if u := codebuddyResultUsage(event, fallbackModel); u != nil {
				usageMap = u
			}
			if event.IsError {
				finalStatus = "failed"
				if event.Error != nil {
					finalError = event.Error.Message
				}
				if finalError == "" && event.ResultText != "" {
					finalError = event.ResultText
				}
				if finalError == "" {
					finalError = "codebuddy reported error result"
				}
				trySend(ch, Message{Type: MessageError, Content: finalError})
			}
			// Result is terminal — close stdin so codebuddy can exit cleanly.
			closeStdin()
		case "control_request":
			b.handleCodebuddyControlRequest(event, stdin)
		case "stream_event":
			// Partial-message deltas — only emitted when codebuddy is launched
			// with --include-partial-messages, which the daemon does not
			// set. Ignore them defensively so a future flag flip does not
			// break the parser.
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		b.cfg.Logger.Warn("codebuddy stdout scanner error", "error", scanErr)
		if finalStatus == "completed" {
			finalStatus = "failed"
			finalError = fmt.Sprintf("stdout read error: %v", scanErr)
		}
	}

	return codebuddyEventResult{
		status:    finalStatus,
		errMsg:    finalError,
		output:    output.String(),
		sessionID: sessionID,
		usage:     usageMap,
	}
}

// handleCodebuddyAssistant unpacks an assistant event's message.content
// blocks and emits one Message per block. Token usage on the assistant
// message is accumulated into usageMap (per-model, Claude-style); the
// result frame's authoritative usage later overrides these incrementals.
func (b *codebuddyBackend) handleCodebuddyAssistant(event codebuddySDKMessage, ch chan<- Message, output *strings.Builder, usageMap *map[string]TokenUsage) {
	if len(event.Message) == 0 {
		return
	}
	var content codebuddyMessageContent
	if err := json.Unmarshal(event.Message, &content); err != nil {
		return
	}

	// Per-model usage accumulation (Claude-style). codebuddy emits usage on
	// every assistant message; the final result frame carries the
	// authoritative totals, which override these incrementals.
	if content.Usage != nil && content.Model != "" {
		if *usageMap == nil {
			*usageMap = make(map[string]TokenUsage)
		}
		u := (*usageMap)[content.Model]
		u.InputTokens += content.Usage.InputTokens
		u.OutputTokens += content.Usage.OutputTokens
		u.CacheReadTokens += content.Usage.CacheReadInputTokens
		u.CacheWriteTokens += content.Usage.CacheCreationInputTokens
		(*usageMap)[content.Model] = u
	}

	for _, block := range content.Content {
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
			if block.Input != nil {
				_ = json.Unmarshal(block.Input, &input)
			}
			trySend(ch, Message{
				Type:   MessageToolUse,
				Tool:   block.Name,
				CallID: block.ID,
				Input:  input,
			})
		}
	}
}

// handleCodebuddyUser unpacks a user event's message.content blocks and
// emits tool_result blocks as MessageToolResult. codebuddy emits user
// events for tool results (the agent's own tool outputs echoed back as
// user turns), mirroring Claude's protocol.
func (b *codebuddyBackend) handleCodebuddyUser(event codebuddySDKMessage, ch chan<- Message) {
	if len(event.Message) == 0 {
		return
	}
	var content codebuddyMessageContent
	if err := json.Unmarshal(event.Message, &content); err != nil {
		return
	}
	for _, block := range content.Content {
		if block.Type != "tool_result" {
			continue
		}
		resultStr := ""
		if block.Content != nil {
			resultStr = string(block.Content)
		}
		trySend(ch, Message{
			Type:   MessageToolResult,
			CallID: block.ToolUseID,
			Output: resultStr,
		})
	}
}

// handleCodebuddyControlRequest auto-approves every tool use so the daemon
// runs fully autonomously. codebuddy emits control_request frames for
// permission prompts; the daemon answers with a control_response carrying
// behavior:"allow". This mirrors the claude / qwen backend's contract.
func (b *codebuddyBackend) handleCodebuddyControlRequest(event codebuddySDKMessage, stdin io.Writer) {
	if len(event.Request) == 0 {
		return
	}
	var req codebuddyControlRequestPayload
	if err := json.Unmarshal(event.Request, &req); err != nil {
		return
	}

	var inputMap map[string]any
	if req.Input != nil {
		_ = json.Unmarshal(req.Input, &inputMap)
	}
	if inputMap == nil {
		inputMap = map[string]any{}
	}

	response := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": event.RequestID,
			"response": map[string]any{
				"behavior":     "allow",
				"updatedInput": inputMap,
			},
		},
	}

	data, err := json.Marshal(response)
	if err != nil {
		b.cfg.Logger.Warn("codebuddy: failed to marshal control response", "error", err)
		return
	}
	data = append(data, '\n')
	if _, err := stdin.Write(data); err != nil {
		b.cfg.Logger.Warn("codebuddy: failed to write control response", "error", err)
	}
}

// ── JSON types for codebuddy stream-json stdout events ──
//
// These mirror the Claude Code SDK schema (which qwen-code / codebuddy
// reimplement) but live in this file as codebuddy-prefixed types so a future
// codebuddy schema drift can be fixed without touching claude.go or qwen.go.
// Extra codebuddy-specific fields (parent_tool_use_id, duration_api_ms,
// permission_denials, structured_result) are tolerated via json.RawMessage /
// omitempty and ignored unless the daemon needs them.

type codebuddySDKMessage struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	Request   json.RawMessage `json:"request,omitempty"`

	// result fields
	ResultText string                               `json:"result,omitempty"`
	IsError    bool                                 `json:"is_error,omitempty"`
	DurationMs float64                              `json:"duration_ms,omitempty"`
	NumTurns   int                                  `json:"num_turns,omitempty"`
	Usage      *codebuddyUsage                      `json:"usage,omitempty"`
	ModelUsage map[string]codebuddyResultModelUsage `json:"modelUsage,omitempty"`
	Error      *codebuddyError                      `json:"error,omitempty"`
}

// codebuddyError is the error object carried by result events with
// is_error:true. codebuddy is assumed to emit {message: "..."} (matching
// the Claude Code SDK contract qwen reimplements); the handler reads the
// Message field directly.
type codebuddyError struct {
	Message string `json:"message,omitempty"`
}

type codebuddyMessageContent struct {
	Role    string                  `json:"role"`
	Model   string                  `json:"model,omitempty"`
	Content []codebuddyContentBlock `json:"content"`
	Usage   *codebuddyUsage         `json:"usage,omitempty"`
}

// codebuddyUsage is the token-usage shape carried on assistant messages and
// the result frame. Field names match the Claude Code SDK contract that
// codebuddy is assumed to reimplement (same as qwen).
type codebuddyUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
}

// codebuddyResultModelUsage mirrors qwenResultModelUsage /
// claudeResultModelUsage — the per-model usage map on the result frame
// (camelCase keys, distinct from the snake_case codebuddyUsage on
// assistant messages).
type codebuddyResultModelUsage struct {
	InputTokens              int64 `json:"inputTokens"`
	OutputTokens             int64 `json:"outputTokens"`
	CacheReadInputTokens     int64 `json:"cacheReadInputTokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cacheCreationInputTokens,omitempty"`
}

type codebuddyContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

type codebuddyControlRequestPayload struct {
	Subtype  string          `json:"subtype"`
	ToolName string          `json:"tool_name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
}

// codebuddyResultUsage projects the result frame's usage into the shared
// per-model TokenUsage map. Mirrors qwenResultUsage / claudeResultUsage so
// the daemon's billing path is identical across the backends.
//
// Precedence (matches claude.go / qwen.go):
//  1. msg.ModelUsage (per-model map) wins when present and non-empty.
//  2. Otherwise the flat msg.Usage block is keyed by fallbackModel.
//  3. Returns nil when neither carries non-zero tokens, so the caller's
//     incremental accumulation from assistant messages survives.
func codebuddyResultUsage(msg codebuddySDKMessage, fallbackModel string) map[string]TokenUsage {
	if len(msg.ModelUsage) > 0 {
		usage := make(map[string]TokenUsage, len(msg.ModelUsage))
		for model, u := range msg.ModelUsage {
			if model == "" || !codebuddyUsageHasTokens(u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens) {
				continue
			}
			usage[model] = TokenUsage{
				InputTokens:      u.InputTokens,
				OutputTokens:     u.OutputTokens,
				CacheReadTokens:  u.CacheReadInputTokens,
				CacheWriteTokens: u.CacheCreationInputTokens,
			}
		}
		if len(usage) > 0 {
			return usage
		}
	}
	// Flat usage block on the result frame, keyed by fallbackModel.
	// codebuddy is assumed not to currently emit modelUsage (matching
	// qwen), so this is the common path; the result frame is authoritative
	// and overrides the incremental map from assistant messages. Returning
	// nil here preserves the incremental map when the result frame carries
	// no usage (e.g. crash before result).
	if msg.Usage == nil || fallbackModel == "" || !codebuddyUsageHasTokens(
		msg.Usage.InputTokens,
		msg.Usage.OutputTokens,
		msg.Usage.CacheReadInputTokens,
		msg.Usage.CacheCreationInputTokens,
	) {
		return nil
	}
	return map[string]TokenUsage{
		fallbackModel: {
			InputTokens:      msg.Usage.InputTokens,
			OutputTokens:     msg.Usage.OutputTokens,
			CacheReadTokens:  msg.Usage.CacheReadInputTokens,
			CacheWriteTokens: msg.Usage.CacheCreationInputTokens,
		},
	}
}

func codebuddyUsageHasTokens(input, output, cacheRead, cacheWrite int64) bool {
	return input > 0 || output > 0 || cacheRead > 0 || cacheWrite > 0
}

// ── Args + I/O helpers ──

// buildCodebuddyArgs assembles the CLI argument vector for
// `codebuddy --bare` in stream-json mode. The hardcoded flags establish the
// daemon↔codebuddy protocol contract; user-supplied extra/custom args are
// filtered against codebuddyBlockedArgs so a misconfigured agent cannot
// silently outvote a flag the protocol depends on.
//
// ThinkingLevel is intentionally not mapped: codebuddy --help (v2.113.0)
// exposes no reasoning-effort flag (same as qwen). opts.ThinkingLevel is
// silently ignored rather than failing, mirroring the qwen / openclaw
// backend's handling of unsupported fields, so runtime support can grow
// incrementally.
func buildCodebuddyArgs(opts ExecOptions, logger *slog.Logger) []string {
	args := []string{
		"--bare",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--yolo",
	}
	if opts.Model != "" {
		args = append(args, "-m", opts.Model)
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-session-turns", fmt.Sprintf("%d", opts.MaxTurns))
	}
	if opts.SystemPrompt != "" {
		// Use --append-system-prompt (additive) rather than --system-prompt
		// (override) so we do not blow away codebuddy's default system
		// prompt; daemon instructions should compose with the CLI's
		// built-in scaffolding, not replace it. Matches the claude / qwen
		// backend's choice.
		args = append(args, "--append-system-prompt", opts.SystemPrompt)
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	args = append(args, filterCustomArgs(opts.ExtraArgs, codebuddyBlockedArgs, logger)...)
	args = append(args, filterCustomArgs(opts.CustomArgs, codebuddyBlockedArgs, logger)...)
	return args
}

// writeCodebuddyInput writes the prompt as a single user-turn NDJSON frame
// to w. The frame schema matches the Claude Code SDK / qwen-code input
// contract:
//
//	{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<prompt>"}]}}
//
// One trailing newline terminates the frame; stdin is left open so
// codebuddy can emit control_request frames mid-run and read matching
// control_response frames on the same stream.
func writeCodebuddyInput(w io.Writer, prompt string) error {
	data, err := buildCodebuddyInput(prompt)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return nil
}

func buildCodebuddyInput(prompt string) ([]byte, error) {
	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]string{
				{
					"type": "text",
					"text": prompt,
				},
			},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal codebuddy input: %w", err)
	}
	return append(data, '\n'), nil
}

// withCodebuddyStderr appends a stderr tail hint to an error message when
// non-empty, otherwise returns msg unchanged. Mirrors withClaudeStderr /
// withQwenStderr; kept local to codebuddy.go so the shared helpers module
// does not need to grow a generic equivalent.
func withCodebuddyStderr(msg, tail string) string {
	if tail == "" {
		return msg
	}
	return msg + "; codebuddy stderr: " + tail
}
