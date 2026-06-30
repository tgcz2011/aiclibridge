// Package adapter — gemini backend (EXPERIMENTAL).
//
// EXPERIMENTAL: not validated against a real gemini-cli install; schema
// assumed to match opencode/qwen stream-json since gemini-cli shares
// lineage. Tests use stubbed stdout.
//
// This adapter spawns `gemini --bare --output-format stream-json
// --input-format stream-json --yolo` and drives a single user turn over
// stdin (stream-json) while reading NDJSON events from stdout. The flag
// set mirrors qwen's (same fork lineage). The stdout event schema is
// assumed to match opencode's {type, sessionID, part:{type, text, tool,
// callID, state, tokens}, error} shape — gemini-cli, opencode, and qwen
// share stream-json lineage, so the opencode event parser is ported here
// verbatim (see processGeminiEvents). This assumption has NOT been
// validated against a real @google/gemini-cli install on this host; if the
// real schema diverges, only this file needs updating.
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

// geminiBlockedArgs are flags hardcoded by the daemon that must not be
// overridden by user-configured custom_args. These flags establish the
// daemon↔gemini stream-json protocol contract (NDJSON over stdio,
// auto-approve, daemon-managed model / system-prompt / resume / mcp
// wiring); letting custom_args shadow any of them would either break the
// NDJSON parser or duplicate a flag the daemon already injected. Mirrors
// the qwen/opencode blocked-arg policy (same fork lineage).
var geminiBlockedArgs = map[string]blockedArgMode{
	"--bare":                blockedStandalone, // raw stream-json (no banner)
	"--output-format":       blockedWithValue,  // stream-json protocol
	"--input-format":        blockedWithValue,  // stream-json protocol
	"--yolo":                blockedStandalone, // auto-approve (shorthand)
	"--approval-mode":       blockedWithValue,  // auto-approve (long form: --approval-mode yolo)
	"--mcp-config":          blockedWithValue,  // set by daemon from agent.mcp_config
	"--session-id":          blockedWithValue,  // owned by daemon resume / session pinning
	"--resume":              blockedWithValue,  // owned by daemon resume path
	"-p":                    blockedStandalone, // non-interactive one-shot mode (conflicts with stream-json stdin)
	"--prompt":              blockedWithValue,  // alternate prompt flag (conflicts with stream-json stdin)
	"--model":               blockedWithValue,  // owned by opts.Model
	"--system-prompt":       blockedWithValue,  // owned by opts.SystemPrompt
	"--append-system-prompt": blockedWithValue, // owned by opts.SystemPrompt
	"--max-turns":           blockedWithValue, // owned by opts.MaxTurns
}

// ── Execute ──

// Execute spawns gemini-cli in stream-json mode and drives a single user
// turn. EXPERIMENTAL — see the file-level comment.
func (b *geminiBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "gemini"
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, fmt.Errorf("gemini executable not found at %q: %w", execPath, err)
	}

	runCtx, cancel := runContext(ctx, opts.Timeout)

	args := buildGeminiArgs(opts, b.cfg.Logger)

	// MCP config: gemini-cli supports --mcp-config <path>. Write the
	// agent's mcp_config JSON to a temp file and pass its path so the
	// child uses a controlled server set instead of inheriting the
	// ambient gemini config. Mirrors the claude backend's approach.
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
	cmd.Env = buildEnv(b.cfg.Env)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("gemini stdout pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("gemini stdin pipe: %w", err)
	}
	var closeStdinOnce sync.Once
	closeStdin := func() { closeStdinOnce.Do(func() { _ = stdin.Close() }) }
	// Capture stderr into both the daemon log and a bounded tail buffer so
	// we can include the last few KB in Result.Error when gemini exits
	// unexpectedly — without it a crash looks like "exit status N" only.
	stderrBuf := newStderrTail(newLogWriter(b.cfg.Logger, "[gemini:stderr] "), 0)
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		closeStdin()
		cancel()
		return nil, fmt.Errorf("start gemini: %w", err)
	}

	b.cfg.Logger.Info("gemini started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

	// cmd.Start() succeeded — transfer temp file ownership to the goroutine.
	mcpFileCleanup = nil

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)

	// Write the user-turn stream-json frame to stdin in its own goroutine
	// so it cannot deadlock against the stdout reader. With
	// --input-format stream-json the CLI reads frames from stdin while
	// emitting stdout; if nothing drains stdout while we write, gemini
	// blocks writing stdout, never reads stdin, and our Write blocks
	// until runCtx fires — the same deadlock the claude backend guards
	// against. After the write completes (success or broken-pipe), close
	// stdin to signal EOF: gemini-cli finishes the single turn and exits.
	// Mid-run control_request frames are NOT handled in this experimental
	// adapter; if a future gemini-cli build emits them, the run will
	// surface as a failure (child waits on a response that never comes)
	// and the schema assumption at the top of this file should be
	// revisited.
	writeDone := make(chan error, 1)
	go func() {
		err := writeGeminiInput(stdin, prompt)
		closeStdin()
		writeDone <- err
	}()

	go func() {
		defer cancel()
		defer close(msgCh)
		defer close(resCh)
		if mcpConfigPath != "" {
			defer os.Remove(mcpConfigPath)
		}

		// Close stdin/stdout when the context fires so a stuck scanner
		// unblocks and the goroutine can exit on cancel/timeout.
		go func() {
			<-runCtx.Done()
			closeStdin()
			_ = stdout.Close()
		}()

		startTime := time.Now()
		scanResult := b.processGeminiEvents(stdout, msgCh)
		exitErr := cmd.Wait()
		duration := time.Since(startTime)
		// writeDone is buffered (cap 1) and the writer always sends — by
		// the time cmd has exited, the prompt write has either succeeded,
		// hit a broken pipe, or been unblocked by the kill that ended cmd.
		writeErr := <-writeDone

		finalStatus := scanResult.status
		finalError := scanResult.errMsg

		switch {
		case runCtx.Err() == context.DeadlineExceeded:
			finalStatus = "timeout"
			finalError = fmt.Sprintf("gemini timed out after %s", opts.Timeout)
		case runCtx.Err() == context.Canceled:
			finalStatus = "aborted"
			finalError = "execution cancelled"
		case writeErr != nil && finalStatus == "completed" && scanResult.sessionID == "":
			// No session id landed and the prompt write failed — gemini
			// died before reading the prompt. Surface the write error;
			// the stderr tail attached below carries the real reason.
			finalStatus = "failed"
			finalError = fmt.Sprintf("write gemini input: %v", writeErr)
		case exitErr != nil && finalStatus == "completed":
			finalStatus = "failed"
			finalError = fmt.Sprintf("gemini exited with error: %v", exitErr)
		}
		if finalError != "" {
			finalError = withGeminiStderr(finalError, stderrBuf.Tail())
		}

		b.cfg.Logger.Info("gemini finished", "pid", cmd.Process.Pid, "status", finalStatus, "duration", duration.Round(time.Millisecond).String())

		var usage map[string]TokenUsage
		u := scanResult.usage
		if u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheReadTokens > 0 || u.CacheWriteTokens > 0 {
			model := opts.Model
			if model == "" {
				model = "unknown"
			}
			usage = map[string]TokenUsage{model: u}
		}

		resCh <- Result{
			Status:     finalStatus,
			Output:     scanResult.output,
			Error:      finalError,
			DurationMs: duration.Milliseconds(),
			SessionID:  scanResult.sessionID,
			Usage:      usage,
		}
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
}

// ── Event handlers ──

// geminiEventResult is the accumulated outcome of processGeminiEvents.
// Structurally identical to opencodeEventResult — kept gemini-named so
// the experimental adapter stays self-contained and the logging/path can
// diverge without touching opencode.
type geminiEventResult struct {
	status    string
	errMsg    string
	output    string
	sessionID string
	usage     TokenUsage
}

// processGeminiEvents reads NDJSON lines from r, dispatches events to ch,
// and returns the accumulated result. Ported verbatim from opencode's
// processEvents (opencode.go) and reuses the opencode event types
// (opencodeEvent / opencodeEventPart / opencodeTokens / opencodeError)
// because gemini-cli shares stream-json lineage with opencode/qwen and
// the schema is assumed to match. If the real gemini-cli schema
// diverges, fork the types here. See the EXPERIMENTAL note at the top
// of this file.
func (b *geminiBackend) processGeminiEvents(r io.Reader, ch chan<- Message) geminiEventResult {
	var output strings.Builder
	var sessionID string
	var usage TokenUsage
	finalStatus := "completed"
	var finalError string

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var event opencodeEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		if event.SessionID != "" {
			sessionID = event.SessionID
		}

		switch event.Type {
		case "text":
			text := event.Part.Text
			if text != "" {
				output.WriteString(text)
				trySend(ch, Message{Type: MessageText, Content: text})
			}
		case "tool_use":
			var input map[string]any
			if event.Part.State != nil && event.Part.State.Input != nil {
				_ = json.Unmarshal(event.Part.State.Input, &input)
			}
			trySend(ch, Message{
				Type:   MessageToolUse,
				Tool:   event.Part.Tool,
				CallID: event.Part.CallID,
				Input:  input,
			})
			if event.Part.State != nil && event.Part.State.Status == "completed" {
				trySend(ch, Message{
					Type:   MessageToolResult,
					Tool:   event.Part.Tool,
					CallID: event.Part.CallID,
					Output: opencodeExtractToolOutput(event.Part.State.Output),
				})
			}
		case "error":
			errMsg := ""
			if event.Error != nil {
				errMsg = event.Error.Message()
			}
			if errMsg == "" {
				errMsg = "unknown gemini error"
			}
			b.cfg.Logger.Warn("gemini error event", "error", errMsg)
			trySend(ch, Message{Type: MessageError, Content: errMsg})
			finalStatus = "failed"
			finalError = errMsg
		case "step_start":
			trySend(ch, Message{Type: MessageStatus, Status: "running"})
		case "step_finish":
			if t := event.Part.Tokens; t != nil {
				usage.InputTokens += t.Input
				usage.OutputTokens += t.Output
				if t.Cache != nil {
					usage.CacheReadTokens += t.Cache.Read
					usage.CacheWriteTokens += t.Cache.Write
				}
			}
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		b.cfg.Logger.Warn("gemini stdout scanner error", "error", scanErr)
		if finalStatus == "completed" {
			finalStatus = "failed"
			finalError = fmt.Sprintf("stdout read error: %v", scanErr)
		}
	}

	return geminiEventResult{
		status:    finalStatus,
		errMsg:    finalError,
		output:    output.String(),
		sessionID: sessionID,
		usage:     usage,
	}
}

// ── Args builder ──

// buildGeminiArgs assembles the CLI argument vector for `gemini` in
// stream-json mode. The hardcoded flags establish the daemon↔gemini
// protocol contract (NDJSON over stdio, auto-approve); user-supplied
// extra/custom args are filtered against geminiBlockedArgs so a
// misconfigured agent cannot silently outvote a protocol flag.
//
// ThinkingLevel is intentionally ignored: gemini-cli has no stable
// reasoning-effort flag in the assumed schema, and the daemon's picker
// fall-through leaves the model default in place (mirrors the
// opencode/qwen policy for unsupported effort surfaces).
func buildGeminiArgs(opts ExecOptions, logger *slog.Logger) []string {
	args := []string{
		"--bare",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--yolo",
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.SystemPrompt != "" {
		// --append-system-prompt (not --system-prompt) so we don't wipe
		// gemini-cli's base instructions; mirrors the claude backend's
		// choice for the same reason.
		args = append(args, "--append-system-prompt", opts.SystemPrompt)
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", opts.MaxTurns))
	}
	if opts.ResumeSessionID != "" {
		// Resume path: --resume <id>. Mirrors claude; gemini-cli exposes
		// both --session-id (pin a new session's id) and --resume
		// (continue a prior one). We use --resume for the resume path.
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	args = append(args, filterCustomArgs(opts.ExtraArgs, geminiBlockedArgs, logger)...)
	args = append(args, filterCustomArgs(opts.CustomArgs, geminiBlockedArgs, logger)...)
	return args
}

// ── Input frame ──

// writeGeminiInput writes a single user-turn stream-json frame to the
// child's stdin. The frame shape mirrors claude's stream-json input
// ({"type":"user","message":{"role":"user","content":[{"type":"text",
// "text":<prompt>}]}}) because gemini-cli shares stream-json lineage
// with claude/qwen; the exact field set has NOT been validated against a
// real install.
func writeGeminiInput(w io.Writer, prompt string) error {
	data, err := buildGeminiInput(prompt)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return nil
}

// buildGeminiInput marshals the user-turn frame and appends the
// newline delimiter that stream-json (NDJSON) requires between frames.
func buildGeminiInput(prompt string) ([]byte, error) {
	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]string{
				{"type": "text", "text": prompt},
			},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal gemini input: %w", err)
	}
	return append(data, '\n'), nil
}

// ── stderr helper ──

// withGeminiStderr appends a stderr tail hint to an error message when
// non-empty, otherwise returns msg unchanged. Mirrors the claude/codex
// equivalents.
func withGeminiStderr(msg, tail string) string {
	if tail == "" {
		return msg
	}
	return msg + "; gemini stderr: " + tail
}
