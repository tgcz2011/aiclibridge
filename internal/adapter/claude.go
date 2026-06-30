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

// ── Execute ──

func (b *claudeBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "claude"
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, fmt.Errorf("claude executable not found at %q: %w", execPath, err)
	}

	timeout := opts.Timeout
	runCtx, cancel := runContext(ctx, timeout)

	args := buildClaudeArgs(opts, b.cfg.Logger)

	// If the caller provided an MCP config, write it to a temp file and pass
	// --mcp-config <path> so the agent uses a controlled set of MCP servers
	// instead of inheriting from the outer Claude Code session.
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
	b.cfg.Logger.Info("agent command", "exec", execPath, "args", args)
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Env = buildEnv(b.cfg.Env)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claude stdout pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claude stdin pipe: %w", err)
	}
	var closeStdinOnce sync.Once
	closeStdin := func() { closeStdinOnce.Do(func() { _ = stdin.Close() }) }
	// Capture stderr into both the daemon log (as before) and a bounded tail
	// buffer so we can include the last few KB in Result.Error when claude
	// exits unexpectedly. Without the tail, an exit-code-only failure looks
	// like "claude exited with error: exit status 3" — which is useless for
	// root-causing V8 aborts, Bun panics, or any other CLI-side crash.
	stderrBuf := newStderrTail(newLogWriter(b.cfg.Logger, "[claude:stderr] "), 0)
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		closeStdin()
		cancel()
		return nil, fmt.Errorf("start claude: %w", err)
	}

	b.cfg.Logger.Info("claude started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

	// cmd.Start() succeeded — transfer temp file ownership to the goroutine.
	mcpFileCleanup = nil

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)

	// writeClaudeInput runs in its own goroutine so it cannot deadlock
	// against the stdout reader. With --verbose --output-format stream-json
	// the CLI emits a startup banner before reading its first stdin frame;
	// if nothing is draining stdout while we write the prompt, claude blocks
	// writing stdout, never reads stdin, and our Write blocks until runCtx
	// fires. The field symptom is "write |1: The pipe has been ended."
	// surfacing exactly at the per-task timeout when the kill invalidates
	// the still-blocked pipe.
	//
	// Keep stdin open after the initial user message. Claude's stream-json
	// protocol can emit control_request events mid-run and expects matching
	// control_response frames on the same input stream; closing stdin here
	// leaves the child stuck waiting for a response until its own fallback
	// timeout.
	writeDone := make(chan error, 1)
	go func() {
		err := writeClaudeInput(stdin, prompt)
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
		var output strings.Builder
		var sessionID string
		finalStatus := "completed"
		var finalError string
		sawAsyncLaunch := false
		usage := make(map[string]TokenUsage)

		// Close stdout when the context is cancelled so scanner.Scan() unblocks.
		go func() {
			<-runCtx.Done()
			closeStdin()
			_ = stdout.Close()
		}()

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			var msg claudeSDKMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}

			switch msg.Type {
			case "assistant":
				b.handleAssistant(msg, msgCh, &output, usage)
			case "user":
				if b.handleUser(msg, msgCh) {
					sawAsyncLaunch = true
				}
			case "system":
				if msg.SessionID != "" {
					sessionID = msg.SessionID
				}
				trySend(msgCh, Message{Type: MessageStatus, Status: "running", SessionID: sessionID})
			case "result":
				sessionID = msg.SessionID
				if msg.ResultText != "" {
					output.Reset()
					output.WriteString(msg.ResultText)
				}
				if resultUsage := claudeResultUsage(msg, opts.Model); len(resultUsage) > 0 {
					usage = resultUsage
				}
				if msg.IsError {
					finalStatus = "failed"
					finalError = msg.ResultText
				}
				closeStdin()
			case "log":
				if msg.Log != nil {
					trySend(msgCh, Message{
						Type:    MessageLog,
						Level:   msg.Log.Level,
						Content: msg.Log.Message,
					})
				}
			case "control_request":
				b.handleControlRequest(msg, stdin)
			}
		}

		closeStdin()

		// Wait for process exit
		exitErr := cmd.Wait()
		duration := time.Since(startTime)
		// writeDone is buffered (cap 1) and the writer always sends — by the
		// time cmd has exited, the prompt write has either succeeded, hit a
		// broken pipe, or been unblocked by the kill that ended cmd.
		writeErr := <-writeDone

		switch {
		case runCtx.Err() == context.DeadlineExceeded:
			finalStatus = "timeout"
			finalError = fmt.Sprintf("claude timed out after %s", timeout)
		case runCtx.Err() == context.Canceled:
			finalStatus = "aborted"
			finalError = "execution cancelled"
		case writeErr != nil && finalStatus == "completed" && sessionID == "":
			// No result event landed and the prompt write failed — claude
			// died before reading the prompt. Surface the write error; the
			// stderr tail attached below carries the real reason.
			finalStatus = "failed"
			finalError = fmt.Sprintf("write claude input: %v", writeErr)
		case exitErr != nil && finalStatus == "completed":
			finalStatus = "failed"
			finalError = fmt.Sprintf("claude exited with error: %v", exitErr)
		}
		if finalStatus == "completed" && sawAsyncLaunch {
			finalStatus = "failed"
			finalError = "claude launched an async background task; aiclibridge-managed runs require foreground execution"
		}

		// cmd.Wait() has returned — os/exec's stderr copy goroutine has
		// observed every byte claude wrote to stderr before exiting, so
		// stderrBuf.Tail() is safe to sample now. Attach the tail to any
		// non-empty failure message; callers upstream surface this as the
		// task's error field, which is the only place users see it.
		if finalError != "" {
			finalError = withClaudeStderr(finalError, stderrBuf.Tail())
		}

		b.cfg.Logger.Info("claude finished", "pid", cmd.Process.Pid, "status", finalStatus, "duration", duration.Round(time.Millisecond).String())

		reportedSessionID := resolveSessionID(opts.ResumeSessionID, sessionID, finalStatus == "failed")
		if reportedSessionID != sessionID {
			b.cfg.Logger.Info("claude resume did not land; clearing fresh session id for daemon fallback",
				"requested_resume", opts.ResumeSessionID,
				"emitted_session", sessionID,
			)
		}

		resCh <- Result{
			Status:     finalStatus,
			Output:     output.String(),
			Error:      finalError,
			DurationMs: duration.Milliseconds(),
			SessionID:  reportedSessionID,
			Usage:      usage,
		}
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
}

// ── Message handlers ──

func (b *claudeBackend) handleAssistant(msg claudeSDKMessage, ch chan<- Message, output *strings.Builder, usage map[string]TokenUsage) {
	var content claudeMessageContent
	if err := json.Unmarshal(msg.Message, &content); err != nil {
		return
	}

	// Accumulate token usage per model.
	if content.Usage != nil && content.Model != "" {
		u := usage[content.Model]
		u.InputTokens += content.Usage.InputTokens
		u.OutputTokens += content.Usage.OutputTokens
		u.CacheReadTokens += content.Usage.CacheReadInputTokens
		u.CacheWriteTokens += content.Usage.CacheCreationInputTokens
		usage[content.Model] = u
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

func (b *claudeBackend) handleUser(msg claudeSDKMessage, ch chan<- Message) bool {
	var content claudeMessageContent
	if err := json.Unmarshal(msg.Message, &content); err != nil {
		return false
	}

	sawAsyncLaunch := false
	for _, block := range content.Content {
		if block.Type == "tool_result" {
			resultStr := ""
			if block.Content != nil {
				resultStr = string(block.Content)
				if claudeToolResultHasAsyncLaunch(block.Content) {
					sawAsyncLaunch = true
				}
			}
			trySend(ch, Message{
				Type:   MessageToolResult,
				CallID: block.ToolUseID,
				Output: resultStr,
			})
		}
	}
	return sawAsyncLaunch
}

func (b *claudeBackend) handleControlRequest(msg claudeSDKMessage, stdin interface{ Write([]byte) (int, error) }) {
	// Auto-approve all tool uses in autonomous/daemon mode.
	var req claudeControlRequestPayload
	if err := json.Unmarshal(msg.Request, &req); err != nil {
		return
	}

	var inputMap map[string]any
	if req.Input != nil {
		_ = json.Unmarshal(req.Input, &inputMap)
	}
	if inputMap == nil {
		inputMap = map[string]any{}
	}
	if forceClaudeToolInputForeground(inputMap) {
		b.cfg.Logger.Info("claude: forced foreground tool execution",
			"request_id", msg.RequestID,
			"tool", req.ToolName,
		)
	}

	response := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": msg.RequestID,
			"response": map[string]any{
				"behavior":     "allow",
				"updatedInput": inputMap,
			},
		},
	}

	data, err := json.Marshal(response)
	if err != nil {
		b.cfg.Logger.Warn("claude: failed to marshal control response", "error", err)
		return
	}
	data = append(data, '\n')
	if _, err := stdin.Write(data); err != nil {
		b.cfg.Logger.Warn("claude: failed to write control response", "error", err)
	}
}

func forceClaudeToolInputForeground(input map[string]any) bool {
	if runInBackground, ok := input["run_in_background"].(bool); ok && runInBackground {
		input["run_in_background"] = false
		return true
	}
	return false
}

func claudeToolResultHasAsyncLaunch(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	switch v := value.(type) {
	case map[string]any:
		if claudeMapHasAsyncLaunchStatus(v) {
			return true
		}
		if content, ok := v["content"].([]any); ok {
			return claudeArrayHasAsyncLaunchStatus(content)
		}
	case []any:
		return claudeArrayHasAsyncLaunchStatus(v)
	}
	return false
}

func claudeArrayHasAsyncLaunchStatus(values []any) bool {
	for _, value := range values {
		if item, ok := value.(map[string]any); ok && claudeMapHasAsyncLaunchStatus(item) {
			return true
		}
	}
	return false
}

func claudeMapHasAsyncLaunchStatus(value map[string]any) bool {
	status, ok := value["status"].(string)
	return ok && status == "async_launched"
}

// ── Claude SDK JSON types ──

type claudeSDKMessage struct {
	Type      string          `json:"type"`
	Message   json.RawMessage `json:"message,omitempty"`
	Subtype   string          `json:"subtype,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Model     string          `json:"model,omitempty"`

	// result fields
	ResultText string                            `json:"result,omitempty"`
	IsError    bool                              `json:"is_error,omitempty"`
	DurationMs float64                           `json:"duration_ms,omitempty"`
	NumTurns   int                               `json:"num_turns,omitempty"`
	Usage      *claudeUsage                      `json:"usage,omitempty"`
	ModelUsage map[string]claudeResultModelUsage `json:"modelUsage,omitempty"`

	// log fields
	Log *claudeLogEntry `json:"log,omitempty"`

	// control request fields
	RequestID string          `json:"request_id,omitempty"`
	Request   json.RawMessage `json:"request,omitempty"`
}

type claudeLogEntry struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

type claudeMessageContent struct {
	Role    string               `json:"role"`
	Model   string               `json:"model"`
	Content []claudeContentBlock `json:"content"`
	Usage   *claudeUsage         `json:"usage,omitempty"`
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

type claudeResultModelUsage struct {
	InputTokens              int64 `json:"inputTokens"`
	OutputTokens             int64 `json:"outputTokens"`
	CacheReadInputTokens     int64 `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64 `json:"cacheCreationInputTokens"`
}

func claudeResultUsage(msg claudeSDKMessage, fallbackModel string) map[string]TokenUsage {
	if len(msg.ModelUsage) > 0 {
		usage := make(map[string]TokenUsage, len(msg.ModelUsage))
		for model, u := range msg.ModelUsage {
			if model == "" || !claudeUsageHasTokens(u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens) {
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

	model := msg.Model
	if model == "" {
		model = fallbackModel
	}
	if msg.Usage == nil || model == "" || !claudeUsageHasTokens(
		msg.Usage.InputTokens,
		msg.Usage.OutputTokens,
		msg.Usage.CacheReadInputTokens,
		msg.Usage.CacheCreationInputTokens,
	) {
		return nil
	}
	return map[string]TokenUsage{
		model: {
			InputTokens:      msg.Usage.InputTokens,
			OutputTokens:     msg.Usage.OutputTokens,
			CacheReadTokens:  msg.Usage.CacheReadInputTokens,
			CacheWriteTokens: msg.Usage.CacheCreationInputTokens,
		},
	}
}

func claudeUsageHasTokens(input, output, cacheRead, cacheWrite int64) bool {
	return input > 0 || output > 0 || cacheRead > 0 || cacheWrite > 0
}

type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

type claudeControlRequestPayload struct {
	Subtype  string          `json:"subtype"`
	ToolName string          `json:"tool_name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
}

// ── Args + I/O helpers ──

// buildClaudeArgs assembles the CLI argument vector for `claude -p` in
// stream-json mode. The hardcoded flags establish the daemon↔Claude protocol
// contract; user-supplied extra/custom args are filtered against the same
// claudeBlockedArgs set defined in helpers.go so a misconfigured agent
// cannot silently outvote a flag the protocol depends on (e.g. replacing
// stream-json with text and breaking the parser).
func buildClaudeArgs(opts ExecOptions, logger *slog.Logger) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--strict-mcp-config",
		"--permission-mode", "bypassPermissions",
		// AskUserQuestion is Claude Code's built-in interactive question tool.
		// The daemon runs Claude in non-interactive stream-json mode and has
		// no UI for the prompt to render in, so a call returns an empty
		// answer and the agent ends up "inferring" silently — the user
		// never sees the question (see GitHub #2588). User-facing
		// clarification belongs in an issue comment instead.
		"--disallowedTools", "AskUserQuestion",
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.ThinkingLevel != "" {
		// Slotted right after --model so the per-session effort runs
		// against the same model selection the args advertise; the CLI
		// itself accepts the flag in any order but this ordering makes
		// the launch line readable in `agent command` logs.
		args = append(args, "--effort", opts.ThinkingLevel)
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", opts.MaxTurns))
	}
	if opts.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", opts.SystemPrompt)
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	args = append(args, filterCustomArgs(opts.ExtraArgs, claudeBlockedArgs, logger)...)
	args = append(args, filterCustomArgs(opts.CustomArgs, claudeBlockedArgs, logger)...)
	return args
}

func writeClaudeInput(w io.Writer, prompt string) error {
	data, err := buildClaudeInput(prompt)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return nil
}

func buildClaudeInput(prompt string) ([]byte, error) {
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
		return nil, fmt.Errorf("marshal claude input: %w", err)
	}
	return append(data, '\n'), nil
}

// resolveSessionID decides which session id to report on the Result. When the
// caller requested --resume but claude emitted a fresh, different session id
// AND the run failed, the resume did not land (claude prints
// "No conversation found with session ID: ..." to stderr, generates a fresh
// session, and exits). Returning "" in that case keeps the daemon's
// retry-with-fresh-session fallback able to trigger, instead of silently
// persisting a brand-new id as if resume had succeeded.
func resolveSessionID(requestedResume, emitted string, failed bool) string {
	if failed && requestedResume != "" && emitted != "" && emitted != requestedResume {
		return ""
	}
	return emitted
}

// writeMcpConfigToTemp writes raw MCP config JSON to a temporary file and
// returns its path. The caller is responsible for removing the file when
// done.
func writeMcpConfigToTemp(raw json.RawMessage) (string, error) {
	f, err := os.CreateTemp("", "aiclibridge-mcp-*.json")
	if err != nil {
		return "", fmt.Errorf("create mcp config temp file: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("write mcp config temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("close mcp config temp file: %w", err)
	}
	return f.Name(), nil
}

// withClaudeStderr appends a stderr tail hint to an error message when
// non-empty, otherwise returns msg unchanged. The tail is prefixed with a
// short label so the composed string stays readable even when the original
// msg is already verbose. Kept local to claude.go because the shared
// helpers module does not (yet) export a generic equivalent — other
// backends inline their own when they need it.
func withClaudeStderr(msg, tail string) string {
	if tail == "" {
		return msg
	}
	return msg + "; claude stderr: " + tail
}
