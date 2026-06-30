package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// opencodeBlockedArgs are flags hardcoded by the daemon that must not be
// overridden by user-configured custom_args. --format, --variant, and
// --dangerously-skip-permissions belong to the daemon-managed protocol
// contract (NDJSON output, ThinkingLevel picker, non-interactive mode);
// letting custom_args override them would break the opencode communication
// contract.
var opencodeBlockedArgs = map[string]blockedArgMode{
	"--format":                       blockedWithValue,  // json output format for daemon communication
	"--variant":                      blockedWithValue,  // owned by opts.ThinkingLevel
	"--dangerously-skip-permissions": blockedStandalone, // daemon manages non-interactive permission prompts
}

// opencodeBackend implements Backend by spawning `opencode run --format json`
// and reading streaming NDJSON events from stdout. Ported from
// multica/server/pkg/agent/opencode.go (line 43).
//
// Resume path: ResumeSessionID is passed as `--session <id>` to opencode.
// Verified in multica source at opencode.go:94-95:
//
//	if opts.ResumeSessionID != "" {
//		args = append(args, "--session", opts.ResumeSessionID)
//	}
//
// If a future opencode release drops --session, this is the only line that
// needs updating.

func (b *opencodeBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "opencode"
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, fmt.Errorf("opencode executable not found at %q: %w", execPath, err)
	}

	runCtx, cancel := runContext(ctx, opts.Timeout)

	args := []string{"run", "--format", "json", "--dangerously-skip-permissions"}
	if opts.Cwd != "" {
		// Anchor opencode's AGENTS.md / .opencode/skills discovery to the
		// task workdir (multica opencode.go:79-81).
		args = append(args, "--dir", opts.Cwd)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.ThinkingLevel != "" {
		// --variant is opencode's reasoning-effort surface; owned by the
		// daemon's ThinkingLevel picker (multica opencode.go:85-87).
		args = append(args, "--variant", opts.ThinkingLevel)
	}
	if opts.SystemPrompt != "" {
		args = append(args, "--prompt", opts.SystemPrompt)
	}
	if opts.ResumeSessionID != "" {
		// Resume path: --session <id>. multica opencode.go:94-95.
		args = append(args, "--session", opts.ResumeSessionID)
	}
	args = append(args, filterCustomArgs(opts.CustomArgs, opencodeBlockedArgs, b.cfg.Logger)...)
	args = append(args, prompt)

	cmd := exec.CommandContext(runCtx, execPath, args...)
	hideAgentWindow(cmd)
	configureProcessGroup(cmd)
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}

	env := buildEnv(b.cfg.Env)
	if opts.Cwd != "" {
		// Override PWD so the child opencode resolves its discovery root
		// to the task workdir (cmd.Dir alone is not enough; opencode reads
		// PWD before falling back to process.cwd()).
		env = append(env, "PWD="+opts.Cwd)
	}
	if mcp, ok := buildOpenCodeMCPConfigContent(opts.McpConfig); ok && mcp != "" {
		// MCP projection via OPENCODE_CONFIG_CONTENT (multica opencode.go:147-157).
		// OPENCODE_CONFIG_CONTENT is opencode's general inline-config injection
		// mechanism — it accepts any subset of opencode's schema and merges at
		// local scope, so daemon-injected entries take precedence over any
		// same-key user entry in <workdir>/opencode.json. We do NOT use
		// --mcp-config: opencode has no such flag; the env-var path is the
		// supported injection channel.
		env = append(env, "OPENCODE_CONFIG_CONTENT="+mcp)
	}
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("opencode stdout pipe: %w", err)
	}
	cmd.Stderr = newLogWriter(b.cfg.Logger, "[opencode:stderr] ")

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start opencode: %w", err)
	}

	b.cfg.Logger.Info("opencode started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)

	go func() {
		defer cancel()
		defer close(msgCh)
		defer close(resCh)

		startTime := time.Now()
		scanResult := b.processEvents(stdout, msgCh)
		exitErr := cmd.Wait()
		duration := time.Since(startTime)

		switch {
		case runCtx.Err() == context.DeadlineExceeded:
			scanResult.status = "timeout"
			scanResult.errMsg = fmt.Sprintf("opencode timed out after %s", opts.Timeout)
		case runCtx.Err() == context.Canceled:
			scanResult.status = "aborted"
			scanResult.errMsg = "execution cancelled"
		case exitErr != nil && scanResult.status == "completed":
			scanResult.status = "failed"
			scanResult.errMsg = fmt.Sprintf("opencode exited with error: %v", exitErr)
		}

		b.cfg.Logger.Info("opencode finished", "pid", cmd.Process.Pid, "status", scanResult.status, "duration", duration.Round(time.Millisecond).String())

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

// ── Event handlers ──

type opencodeEventResult struct {
	status    string
	errMsg    string
	output    string
	sessionID string
	usage     TokenUsage
}

// processEvents reads NDJSON lines from r, dispatches events to ch, and
// returns the accumulated result. Extracted from Execute for testability.
func (b *opencodeBackend) processEvents(r io.Reader, ch chan<- Message) opencodeEventResult {
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
			b.handleTextEvent(event, ch, &output)
		case "tool_use":
			b.handleToolUseEvent(event, ch)
		case "error":
			b.handleErrorEvent(event, ch, &finalStatus, &finalError)
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
		b.cfg.Logger.Warn("opencode stdout scanner error", "error", scanErr)
		if finalStatus == "completed" {
			finalStatus = "failed"
			finalError = fmt.Sprintf("stdout read error: %v", scanErr)
		}
	}

	return opencodeEventResult{
		status:    finalStatus,
		errMsg:    finalError,
		output:    output.String(),
		sessionID: sessionID,
		usage:     usage,
	}
}

func (b *opencodeBackend) handleTextEvent(event opencodeEvent, ch chan<- Message, output *strings.Builder) {
	text := event.Part.Text
	if text != "" {
		output.WriteString(text)
		trySend(ch, Message{Type: MessageText, Content: text})
	}
}

func (b *opencodeBackend) handleToolUseEvent(event opencodeEvent, ch chan<- Message) {
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
}

func (b *opencodeBackend) handleErrorEvent(event opencodeEvent, ch chan<- Message, finalStatus, finalError *string) {
	errMsg := ""
	if event.Error != nil {
		errMsg = event.Error.Message()
	}
	if errMsg == "" {
		errMsg = "unknown opencode error"
	}

	b.cfg.Logger.Warn("opencode error event", "error", errMsg)
	trySend(ch, Message{Type: MessageError, Content: errMsg})

	*finalStatus = "failed"
	*finalError = errMsg
}

// opencodeExtractToolOutput converts the tool state output (which may be a
// string or structured object) into a string.
func opencodeExtractToolOutput(output any) string {
	if output == nil {
		return ""
	}
	if s, ok := output.(string); ok {
		return s
	}
	data, _ := json.Marshal(output)
	return string(data)
}

// ── JSON types for `opencode run --format json` stdout events ──

type opencodeEvent struct {
	Type      string            `json:"type"`
	Timestamp int64             `json:"timestamp,omitempty"`
	SessionID string            `json:"sessionID,omitempty"`
	Part      opencodeEventPart `json:"part"`
	Error     *opencodeError    `json:"error,omitempty"`
}

type opencodeEventPart struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`

	Tool   string             `json:"tool,omitempty"`
	CallID string             `json:"callID,omitempty"`
	State  *opencodeToolState `json:"state,omitempty"`

	Tokens *opencodeTokens `json:"tokens,omitempty"`
}

type opencodeTokens struct {
	Input  int64                `json:"input"`
	Output int64                `json:"output"`
	Cache  *opencodeCacheTokens `json:"cache,omitempty"`
}

type opencodeCacheTokens struct {
	Read  int64 `json:"read"`
	Write int64 `json:"write"`
}

type opencodeToolState struct {
	Status string          `json:"status,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output any             `json:"output,omitempty"`
}

type opencodeError struct {
	Name string           `json:"name,omitempty"`
	Data *opencodeErrData `json:"data,omitempty"`
}

func (e *opencodeError) Message() string {
	if e.Data != nil && e.Data.Message != "" {
		return e.Data.Message
	}
	if e.Name != "" {
		return e.Name
	}
	return ""
}

type opencodeErrData struct {
	Message string `json:"message,omitempty"`
}

// ── MCP config content ──
//
// opencode has no --mcp-config flag. MCP is projected into the child via
// the OPENCODE_CONFIG_CONTENT env var, opencode's general inline-config
// injection mechanism. See multica opencode.go:147-157 and opencode_mcp.go
// for the full Claude→opencode translation. We keep a minimal translator
// (it has no UI surface in this layer) and add a small wrapper so the
// adapter can branch on a single boolean.

func buildOpenCodeMCPConfigContent(raw json.RawMessage) (string, bool) {
	s, err := translateMCPConfigForOpenCode(raw)
	if err != nil || len(s) == 0 {
		return "", err != nil
	}
	data, err := json.Marshal(map[string]any{"mcp": s})
	if err != nil {
		return "", false
	}
	return string(data), true
}

// translateMCPConfigForOpenCode is a minimal Claude-style → opencode-native
// MCP translator. Accepts {"mcpServers": {name: {url|command, ...}}} and
// emits the opencode `mcp` slice ({"name": {type, url, ...}}). Native
// opencode shape {"mcp": {...}} is passed through after basic structural
// validation. See multica opencode_mcp.go for the full schema-strict
// version.
func translateMCPConfigForOpenCode(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var payload struct {
		MCPServers map[string]map[string]any  `json:"mcpServers"`
		MCP        map[string]json.RawMessage `json:"mcp"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("opencode mcp_config: parse: %w", err)
	}
	if len(payload.MCPServers) == 0 && len(payload.MCP) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(payload.MCPServers)+len(payload.MCP))
	for name, rawEntry := range payload.MCP {
		var entry map[string]any
		if err := json.Unmarshal(rawEntry, &entry); err != nil {
			return nil, fmt.Errorf("opencode mcp_config: server %q: %w", name, err)
		}
		out[name] = entry
	}
	for name, server := range payload.MCPServers {
		if url, ok := server["url"].(string); ok && url != "" {
			entry := map[string]any{"type": "remote", "url": url}
			if h, ok := server["headers"]; ok {
				entry["headers"] = h
			}
			out[name] = entry
			continue
		}
		cmd, _ := server["command"].(string)
		entry := map[string]any{"type": "local", "command": []string{cmd}}
		if args, ok := server["args"].([]any); ok {
			full := make([]any, 0, 1+len(args))
			full = append(full, cmd)
			full = append(full, args...)
			entry["command"] = full
		}
		if env, ok := server["env"]; ok {
			entry["environment"] = env
		}
		out[name] = entry
	}
	return out, nil
}

// ── Model discovery ──
//
// Ported from multica models.go:401-434 (discoverOpenCodeModels), 441-478
// (parseOpenCodeModels), 481-498 (parseOpenCodeModelIDLine), 500-516
// (collectOpenCodeModelJSON), 549-562 (annotateOpenCodeModelMetadata), and
// 576-607 (openCodeThinkingLevelsFromVariants).
//
// On any failure (CLI missing, parse error, timeout) we return an empty
// list — the model picker renders gracefully and the runtime still
// appears online.

// opencodeModel is the rich model record used by opencode discovery. It
// extends the shared Model with per-model thinking-level data projected
// from opencode's variant catalog. The discovery layer exposes the rich
// form so callers can render the thinking-level picker; discoverOpenCodeModels
// flattens it to the shared Model.
type opencodeModel struct {
	ID       string                 `json:"id"`
	Label    string                 `json:"label"`
	Provider string                 `json:"provider,omitempty"`
	Thinking *opencodeModelThinking `json:"thinking,omitempty"`
}

type opencodeModelThinking struct {
	SupportedLevels []opencodeThinkingLevel `json:"supported_levels"`
}

type opencodeThinkingLevel struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

func discoverOpenCodeModels(ctx context.Context, executablePath string) ([]Model, error) {
	if executablePath == "" {
		executablePath = "opencode"
	}
	if _, err := exec.LookPath(executablePath); err != nil {
		return []Model{}, nil
	}
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Parse whatever --verbose printed, even on a non-zero exit; a stale
	// config entry can make `opencode models` exit non-zero while still
	// listing the resolvable catalog.
	cmd := exec.CommandContext(runCtx, executablePath, "models", "--verbose")
	hideAgentWindow(cmd)
	out, _ := cmd.Output()
	models := parseOpenCodeModels(string(out))
	if len(models) == 0 {
		cmd = exec.CommandContext(runCtx, executablePath, "models")
		hideAgentWindow(cmd)
		out, _ = cmd.Output()
		models = parseOpenCodeModels(string(out))
	}
	if len(models) == 0 {
		return []Model{}, nil
	}
	// Flatten opencodeModel to shared Model. Thinking data stays on the
	// rich form for callers that need it; the shared Model shape is what
	// the rest of the adapter layer consumes.
	flat := make([]Model, 0, len(models))
	for _, m := range models {
		flat = append(flat, Model{ID: m.ID, Label: m.Label, Provider: m.Provider})
	}
	return flat, nil
}

func parseOpenCodeModels(output string) []opencodeModel {
	lines := strings.Split(output, "\n")
	var models []opencodeModel
	indexByID := map[string]int{}
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		id := parseOpenCodeModelIDLine(line)
		if id == "" {
			continue
		}
		idx, seen := indexByID[id]
		if !seen {
			provider := ""
			if slash := strings.Index(id, "/"); slash > 0 {
				provider = id[:slash]
			}
			idx = len(models)
			indexByID[id] = idx
			models = append(models, opencodeModel{ID: id, Label: id, Provider: provider})
		}

		next := i + 1
		for next < len(lines) && strings.TrimSpace(lines[next]) == "" {
			next++
		}
		if next >= len(lines) || !strings.HasPrefix(strings.TrimSpace(lines[next]), "{") {
			continue
		}
		raw, resumeAt := collectOpenCodeModelJSON(lines, next)
		if json.Valid(raw) {
			annotateOpenCodeModelMetadata(&models[idx], raw)
		}
		i = resumeAt - 1
	}
	return models
}

func parseOpenCodeModelIDLine(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	id := fields[0]
	if strings.HasPrefix(id, `"`) || strings.HasPrefix(id, "{") || strings.HasPrefix(id, "[") {
		return ""
	}
	if !strings.Contains(id, "/") {
		return ""
	}
	if id == strings.ToUpper(id) {
		return ""
	}
	return id
}

func collectOpenCodeModelJSON(lines []string, start int) ([]byte, int) {
	var b strings.Builder
	for i := start; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if i > start && parseOpenCodeModelIDLine(line) != "" {
			return []byte(b.String()), i
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(lines[i])
		if json.Valid([]byte(b.String())) {
			return []byte(b.String()), i + 1
		}
	}
	return []byte(b.String()), len(lines)
}

type opencodeModelMetadata struct {
	Reasoning bool                            `json:"reasoning"`
	Variants  map[string]opencodeModelVariant `json:"variants"`
}

type opencodeModelVariant struct {
	Disabled        bool            `json:"disabled"`
	ReasoningEffort string          `json:"reasoningEffort"`
	Thinking        json.RawMessage `json:"thinking"`
}

var opencodeVariantLabel = map[string]string{
	"none":    "None",
	"minimal": "Minimal",
	"low":     "Low",
	"medium":  "Medium",
	"high":    "High",
	"xhigh":   "Extra high",
	"max":     "Max",
}

var opencodeVariantOrder = map[string]int{
	"none":    0,
	"minimal": 1,
	"low":     2,
	"medium":  3,
	"high":    4,
	"xhigh":   5,
	"max":     6,
}

func annotateOpenCodeModelMetadata(model *opencodeModel, raw []byte) {
	var meta opencodeModelMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return
	}
	if !meta.Reasoning && !openCodeVariantsLookReasoning(meta.Variants) {
		return
	}
	levels := openCodeThinkingLevelsFromVariants(meta.Variants)
	if len(levels) == 0 {
		return
	}
	model.Thinking = &opencodeModelThinking{SupportedLevels: levels}
}

func openCodeVariantsLookReasoning(variants map[string]opencodeModelVariant) bool {
	for name, variant := range variants {
		if _, known := opencodeVariantOrder[name]; known {
			return true
		}
		if variant.ReasoningEffort != "" || len(variant.Thinking) > 0 {
			return true
		}
	}
	return false
}

func openCodeThinkingLevelsFromVariants(variants map[string]opencodeModelVariant) []opencodeThinkingLevel {
	if len(variants) == 0 {
		return nil
	}
	values := make([]string, 0, len(variants))
	for value, variant := range variants {
		if value == "" || variant.Disabled {
			continue
		}
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool {
		left, leftKnown := opencodeVariantOrder[values[i]]
		right, rightKnown := opencodeVariantOrder[values[j]]
		if leftKnown && rightKnown {
			return left < right
		}
		if leftKnown != rightKnown {
			return leftKnown
		}
		return values[i] < values[j]
	})
	levels := make([]opencodeThinkingLevel, 0, len(values))
	for _, value := range values {
		label, ok := opencodeVariantLabel[value]
		if !ok {
			label = strings.Title(strings.ReplaceAll(value, "-", " ")) //nolint:staticcheck
		}
		levels = append(levels, opencodeThinkingLevel{Value: value, Label: label})
	}
	return levels
}
