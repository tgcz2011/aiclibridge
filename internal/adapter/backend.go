// Package adapter is part of aiclibridge.
//
// It hosts the per-CLI adapters that translate aiclibridge's internal
// request model into the wire protocol of each supported coding CLI
// (Claude Code, Codex, OpenCode, OpenClaw, Qwen Code, Gemini CLI). Each
// backend lives in its own file (claude.go, codex.go, opencode.go,
// openclaw.go, qwen.go, gemini.go) and shares process-supervision
// helpers from helpers.go.
package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Backend is the unified interface for executing prompts via coding agents.
type Backend interface {
	// Execute runs a prompt and returns a Session for streaming results.
	// The caller should read from Session.Messages (optional) and wait on
	// Session.Result for the final outcome.
	Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error)
}

// ExecOptions configures a single execution.
type ExecOptions struct {
	Cwd   string
	Model string
	// SystemPrompt is consumed only by providers that can pass or safely inline
	// developer/system instructions. Adapters that cannot (e.g. process-stdin
	// CLIs) intentionally ignore it and rely on cwd-scoped context files such
	// as AGENTS.md instead.
	SystemPrompt              string
	ThreadName                string
	MaxTurns                  int
	Timeout                   time.Duration
	SemanticInactivityTimeout time.Duration
	ResumeSessionID           string          // if non-empty, resume a previous agent session
	ExtraArgs                 []string        // daemon-wide default CLI arguments appended before CustomArgs; currently read by claude and codex backends only
	CustomArgs                []string        // per-agent CLI arguments appended after ExtraArgs
	McpConfig                 json.RawMessage // if non-nil, MCP server config to pass via --mcp-config
	// ThinkingLevel is the runtime-native reasoning/effort value (e.g.
	// Claude's "low|medium|high|xhigh|max", Codex's "none|minimal|low|
	// medium|high|xhigh", OpenCode's model variant names). Empty means
	// "use the runtime/model default" — every backend that consumes this
	// skips its --effort / reasoning_effort injection so the upstream CLI's
	// own default applies. Currently honoured by the claude, codex, and
	// opencode backends; other backends ignore the field rather than fail
	// so runtime support can grow incrementally without breaking unrelated
	// agents.
	ThinkingLevel string
	// OpenclawMode chooses between local (embedded) and gateway routing for
	// the openclaw backend. "" or "local" keeps the historical behaviour —
	// the daemon spawns `openclaw agent --local …` and the agent loop runs
	// in-process on the daemon host. "gateway" instructs the daemon to drop
	// the --local flag and let openclaw route the turn through a configured
	// gateway. Other backends ignore this field, mirroring ThinkingLevel's
	// renderer-side fall-through pattern.
	OpenclawMode string
}

// runContext derives the execution context for an agent subprocess from the
// configured per-run timeout. A positive timeout imposes a hard wall-clock
// deadline; a zero (or negative) timeout imposes NO deadline, leaving liveness
// entirely to the daemon's inactivity watchdog so a session that keeps emitting
// events is never killed merely for running long. The caller owns the
// returned CancelFunc and must call it to release resources.
func runContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}

// trySend performs a non-blocking send on ch. If the channel is full the
// message is dropped — final output is accumulated separately in
// Result.Output, so only streaming consumers are affected by the drop.
func trySend(ch chan<- Message, msg Message) {
	select {
	case ch <- msg:
	default:
		// Channel full — drop message. Final output is accumulated separately
		// in Result.Output, so only streaming consumers are affected.
	}
}

// Session represents a running agent execution.
type Session struct {
	// Messages streams events as the agent works. The channel is closed
	// when the agent finishes (before Result is sent).
	Messages <-chan Message
	// Result receives exactly one value — the final outcome — then closes.
	Result <-chan Result
}

// MessageType identifies the kind of Message.
type MessageType string

const (
	MessageText       MessageType = "text"
	MessageThinking   MessageType = "thinking"
	MessageToolUse    MessageType = "tool-use"
	MessageToolResult MessageType = "tool-result"
	MessageStatus     MessageType = "status"
	MessageError      MessageType = "error"
	MessageLog        MessageType = "log"
)

// Message is a unified event emitted by an agent during execution.
type Message struct {
	Type      MessageType
	Content   string         // text content (Text, Error, Log)
	Tool      string         // tool name (ToolUse, ToolResult)
	CallID    string         // tool call ID (ToolUse, ToolResult)
	Input     map[string]any // tool input (ToolUse)
	Output    string         // tool output (ToolResult)
	Status    string         // agent status string (Status)
	Level     string         // log level (Log)
	SessionID string         // backend session id (Status), for early resume-pointer pinning
}

// TokenUsage tracks token consumption for a single model.
type TokenUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// Result is the final outcome after an agent session completes.
type Result struct {
	Status     string // "completed", "failed", "aborted", "timeout", "cancelled"
	Output     string // accumulated text output
	Error      string // error message if failed
	DurationMs int64
	SessionID  string
	Usage      map[string]TokenUsage // keyed by model name
}

// Config configures a Backend instance.
type Config struct {
	ExecutablePath string            // path to CLI binary (claude, codex, opencode, openclaw, gemini)
	Env            map[string]string // extra environment variables
	Logger         *slog.Logger
}

// New creates a Backend for the given agent type.
//
// Supported types (v0.2):
//   - stream-json / NDJSON / app-server: claude, codex, opencode, openclaw, qwen, gemini
//   - stream-json (Claude SDK schema, same flags as qwen): codebuddy
//   - ACP JSON-RPC (generic adapter in acp.go): copilot, goose, cursor, kimi, kiro, qoder, hermes, auggie
//   - stubs (ErrNotImplemented): droid, snow, vibe, aion
//
// Each adapter's Execute method is implemented in its own file; stub
// adapters return ErrNotImplemented so the catalog can list them
// honestly as known-but-unavailable.
func New(agentType string, cfg Config) (Backend, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	switch agentType {
	case "claude":
		return &claudeBackend{cfg: cfg}, nil
	case "codex":
		return &codexBackend{cfg: cfg}, nil
	case "opencode":
		return &opencodeBackend{cfg: cfg}, nil
	case "openclaw":
		return &openclawBackend{cfg: cfg}, nil
	case "qwen":
		return &qwenBackend{cfg: cfg}, nil
	case "gemini":
		return &geminiBackend{cfg: cfg}, nil
	case "codebuddy":
		return &codebuddyBackend{cfg: cfg}, nil
	case "copilot", "goose", "cursor", "kimi", "kiro", "qoder", "hermes", "auggie":
		return newAcpBackend(agentType, cfg)
	case "droid", "snow", "vibe", "aion":
		return &stubBackend{name: agentType}, nil
	default:
		return nil, fmt.Errorf("unknown agent type %q (supported: claude, codex, opencode, openclaw, qwen, gemini, codebuddy, copilot, goose, cursor, kimi, kiro, qoder, hermes, auggie, droid, snow, vibe, aion)", agentType)
	}
}

// ErrNotImplemented is returned by stub adapters whose upstream CLI
// protocol has not yet been documented. The bridge catalog still lists
// these agents (as available:false) so clients can see what the bridge
// would route to once the protocol is known.
var ErrNotImplemented = fmt.Errorf("adapter not implemented for this CLI (protocol unknown; awaiting upstream documentation)")

// stubBackend satisfies Backend for CLIs whose wire protocol is not yet
// known (droid, snow, vibe, aion). Execute always returns ErrNotImplemented
// so a run routed to one of these fails fast with an honest error rather
// than silently hanging or spawning a process with guessed args.
type stubBackend struct {
	name string
}

func (s *stubBackend) Execute(_ context.Context, _ string, _ ExecOptions) (*Session, error) {
	return nil, fmt.Errorf("%w: %s", ErrNotImplemented, s.name)
}

// ── Stub adapters ──
//
// These satisfy the Backend interface so New() can return non-nil pointers
// for the supported agent types. The real Execute implementation lands in
// a later milestone (one per agent type); for now each stub returns a
// "not yet implemented" error so the rest of the system can wire up
// dispatch and tests without depending on the real CLI logic.

type claudeBackend struct {
	cfg Config
}

// claudeBackend.Execute lives in internal/adapter/claude.go.

type codexBackend struct {
	cfg Config
}

// codexBackend.Execute lives in internal/adapter/codex.go.

type opencodeBackend struct {
	cfg Config
}

// openclawBackend lives in internal/adapter/openclaw.go.

type geminiBackend struct {
	cfg Config
}

// geminiBackend.Execute lives in internal/adapter/gemini.go.
