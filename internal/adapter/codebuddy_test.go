package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// ── Test helpers ──

// newCodebuddyTestBackend returns a codebuddyBackend with a discarding
// logger, suitable for unit tests that exercise the pure parsing /
// arg-building paths without spawning a real codebuddy process.
func newCodebuddyTestBackend(t *testing.T) *codebuddyBackend {
	t.Helper()
	return &codebuddyBackend{cfg: Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
}

// noopCloseStdinCodebuddy is a no-op closeStdin for processCodebuddyEvents
// tests where the stdin lifecycle is not under test.
func noopCloseStdinCodebuddy() {}

// ── TestCodebuddyArgs ──
//
// Table-driven coverage of buildCodebuddyArgs across the flag combinations
// the daemon can inject. Pins the hardcoded protocol flags (--bare /
// --output-format stream-json / --input-format stream-json / --yolo) and
// every optional surface (-m, --max-session-turns, --append-system-prompt,
// --resume, custom_args passthrough). Mirrors TestQwenArgs.

func TestCodebuddyArgs(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	tests := []struct {
		name string
		opts ExecOptions
		// contains: every entry must appear as a contiguous sub-slice of args.
		contains []string
		// notContains: every entry must NOT appear anywhere in args.
		notContains []string
	}{
		{
			name: "bare minimum: stream-json + yolo + bare hardcoded",
			opts: ExecOptions{},
			contains: []string{
				"--bare",
				"--output-format", "stream-json",
				"--input-format", "stream-json",
				"--yolo",
			},
			notContains: []string{"-m", "--max-session-turns", "--append-system-prompt", "--resume", "--mcp-config"},
		},
		{
			name: "model injected via -m",
			opts: ExecOptions{Model: "codebuddy-pro"},
			contains: []string{
				"-m", "codebuddy-pro",
			},
		},
		{
			name: "max turns maps to --max-session-turns",
			opts: ExecOptions{MaxTurns: 7},
			contains: []string{
				"--max-session-turns", "7",
			},
		},
		{
			name: "system prompt uses --append-system-prompt (additive, not override)",
			opts: ExecOptions{SystemPrompt: "be terse"},
			contains: []string{
				"--append-system-prompt", "be terse",
			},
			notContains: []string{"--system-prompt"},
		},
		{
			name: "resume maps to --resume <id>",
			opts: ExecOptions{ResumeSessionID: "sess-abc-123"},
			contains: []string{
				"--resume", "sess-abc-123",
			},
		},
		{
			name: "thinking level is silently ignored (no codebuddy flag)",
			opts: ExecOptions{ThinkingLevel: "high"},
			// No flag should be injected; ThinkingLevel is a no-op for codebuddy.
			notContains: []string{"--thinking", "--effort", "--variant", "--reasoning-effort"},
		},
		{
			name: "safe custom_args pass through unfiltered",
			opts: ExecOptions{
				CustomArgs: []string{"--allowed-tools", "Read", "--debug"},
			},
			contains: []string{
				"--allowed-tools", "Read", "--debug",
			},
		},
		{
			name: "extra_args pass through before custom_args",
			opts: ExecOptions{
				ExtraArgs:  []string{"--include-directories", "/tmp"},
				CustomArgs: []string{"--debug"},
			},
			contains: []string{
				"--include-directories", "/tmp", "--debug",
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := buildCodebuddyArgs(tt.opts, logger)

			// Hardcoded protocol flags must always be present.
			for _, want := range []string{"--bare", "--output-format", "stream-json", "--input-format", "stream-json", "--yolo"} {
				if !slices.Contains(args, want) {
					t.Errorf("args %v: missing required %q", args, want)
				}
			}

			for _, want := range tt.contains {
				if !slices.Contains(args, want) {
					t.Errorf("args %v: missing expected %q", args, want)
				}
			}
			for _, bad := range tt.notContains {
				if slices.Contains(args, bad) {
					t.Errorf("args %v: unexpected %q present", args, bad)
				}
			}
		})
	}
}

// ── TestCodebuddyResume ──
//
// Pins the resume path in isolation: ResumeSessionID non-empty → exactly one
// "--resume <id>" pair in args, and no "--session-id" / "--continue" leak.
// Separate from TestCodebuddyArgs because resume is the single most
// load-bearing flag for the daemon's session-continuation contract.
// Mirrors TestQwenResume.

func TestCodebuddyResume(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("with resume id", func(t *testing.T) {
		t.Parallel()
		args := buildCodebuddyArgs(ExecOptions{ResumeSessionID: "sess-resume-xyz"}, logger)
		idx := slices.Index(args, "--resume")
		if idx < 0 {
			t.Fatalf("args %v: missing --resume", args)
		}
		if idx+1 >= len(args) || args[idx+1] != "sess-resume-xyz" {
			t.Fatalf("args %v: --resume not followed by the session id", args)
		}
		// --session-id is a different codebuddy flag (pin a NEW session); it
		// must not leak in when we are resuming an existing one.
		if slices.Contains(args, "--session-id") {
			t.Errorf("args %v: --session-id must not appear on resume", args)
		}
		if slices.Contains(args, "--continue") || slices.Contains(args, "-c") {
			t.Errorf("args %v: --continue must not appear on resume", args)
		}
	})

	t.Run("without resume id", func(t *testing.T) {
		t.Parallel()
		args := buildCodebuddyArgs(ExecOptions{}, logger)
		if slices.Contains(args, "--resume") {
			t.Errorf("args %v: --resume must not appear when ResumeSessionID is empty", args)
		}
	})
}

// ── TestCodebuddyBlockedArgs ──
//
// Verifies the daemon-managed flag set: protocol-critical flags
// (--output-format, --input-format, --yolo, --bare, -m/--model, --mcp-config,
// --session-id, --system-prompt, --append-system-prompt, --prompt/-p,
// --approval-mode) cannot be overridden by user-configured custom_args.
// Uses the same generic filterCustomArgs helper the other backends use;
// this test pins the codebuddy-specific set (identical to qwen's).

func TestCodebuddyBlockedArgs(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	in := []string{
		"--output-format", "text", // blocked (with value)
		"--input-format", "text", // blocked (with value)
		"--yolo",      // blocked (standalone)
		"--bare",      // blocked (standalone)
		"-m", "gpt-4", // blocked (with value)
		"--model", "gpt-4", // blocked (with value)
		"--mcp-config", "/etc/mcp.json", // blocked (with value)
		"--session-id", "hijack", // blocked (with value)
		"--system-prompt", "evil", // blocked (with value)
		"--append-system-prompt", "evil", // blocked (with value)
		"--prompt", "evil", // blocked (with value)
		"-p", "evil", // blocked (with value)
		"--approval-mode", "default", // blocked (with value)
		"--output-format=text", // blocked (inline value form)
		"--keep-me", "value",   // safe — passes through
		"--debug", // safe — passes through
	}
	got := filterCustomArgs(in, codebuddyBlockedArgs, logger)

	// Everything protocol-critical must be gone; only the safe flags survive.
	want := []string{"--keep-me", "value", "--debug"}
	if !slices.Equal(got, want) {
		t.Errorf("filterCustomArgs mismatch:\n got  %#v\n want %#v", got, want)
	}
}

// ── TestCodebuddyParseEvents ──
//
// Feeds a synthetic stream-json NDJSON stream (mirroring the schema
// ASSUMED for codebuddy — same Claude Code SDK contract as qwen) through
// processCodebuddyEvents and asserts the resulting Message sequence,
// accumulated output, session id, and token usage. No real codebuddy
// process is invoked. Mirrors TestQwenParseEvents.

func TestCodebuddyParseEvents(t *testing.T) {
	t.Parallel()

	b := newCodebuddyTestBackend(t)
	ch := make(chan Message, 256)

	// Captured-style NDJSON stream: system(init) → assistant(text+thinking+
	// tool_use, with usage) → user(tool_result) → result(success, with usage).
	// Field names match the assumed codebuddy schema (snake_case session_id,
	// is_error, duration_ms, num_turns, usage.input_tokens, etc.).
	ndjson := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"codebuddy-sess-1","uuid":"u1","parent_tool_use_id":null,"data":{"model":"codebuddy-pro"}}`,
		`{"type":"assistant","session_id":"codebuddy-sess-1","uuid":"u2","parent_tool_use_id":null,"message":{"id":"m1","type":"message","role":"assistant","model":"codebuddy-pro","content":[{"type":"text","text":"Analyzing "},{"type":"thinking","text":"let me think"},{"type":"tool_use","id":"call_1","name":"Read","input":{"file_path":"/tmp/foo"}}],"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":20}}}`,
		`{"type":"user","session_id":"codebuddy-sess-1","uuid":"u3","parent_tool_use_id":null,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"file contents here"}]}}`,
		`{"type":"assistant","session_id":"codebuddy-sess-1","uuid":"u4","parent_tool_use_id":null,"message":{"id":"m2","type":"message","role":"assistant","model":"codebuddy-pro","content":[{"type":"text","text":"Done."}],"stop_reason":"end_turn","usage":{"input_tokens":50,"output_tokens":10}}}`,
		`{"type":"result","subtype":"success","session_id":"codebuddy-sess-1","uuid":"u5","is_error":false,"duration_ms":1234.5,"duration_api_ms":1000,"num_turns":2,"result":"Done.","usage":{"input_tokens":150,"output_tokens":30},"permission_denials":[]}`,
	}, "\n")

	var stdin bytes.Buffer
	result := b.processCodebuddyEvents(strings.NewReader(ndjson), ch, &stdin, noopCloseStdinCodebuddy, "codebuddy-pro")
	close(ch)

	if result.status != "completed" {
		t.Errorf("status: got %q, want %q", result.status, "completed")
	}
	if result.sessionID != "codebuddy-sess-1" {
		t.Errorf("sessionID: got %q, want %q", result.sessionID, "codebuddy-sess-1")
	}
	// result frame's `result` field is authoritative — it replaces the
	// accumulated streaming text "Analyzing Done." with "Done.".
	if result.output != "Done." {
		t.Errorf("output: got %q, want %q", result.output, "Done.")
	}

	// Drain and verify the message sequence.
	var msgs []Message
	for m := range ch {
		msgs = append(msgs, m)
	}

	// Expected: status(running from system), text, thinking, tool-use,
	// tool-result, text = 6 msgs. (The final result frame carries no
	// streaming text — it is the terminal authoritative output.)
	if len(msgs) != 6 {
		t.Fatalf("expected 6 messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Type != MessageStatus || msgs[0].Status != "running" || msgs[0].SessionID != "codebuddy-sess-1" {
		t.Errorf("msg[0]: got %+v, want status=running session=codebuddy-sess-1", msgs[0])
	}
	if msgs[1].Type != MessageText || msgs[1].Content != "Analyzing " {
		t.Errorf("msg[1]: got %+v, want text 'Analyzing '", msgs[1])
	}
	if msgs[2].Type != MessageThinking || msgs[2].Content != "let me think" {
		t.Errorf("msg[2]: got %+v, want thinking 'let me think'", msgs[2])
	}
	if msgs[3].Type != MessageToolUse || msgs[3].Tool != "Read" || msgs[3].CallID != "call_1" {
		t.Errorf("msg[3]: got %+v, want tool-use Read/call_1", msgs[3])
	}
	if got := msgs[3].Input["file_path"]; got != "/tmp/foo" {
		t.Errorf("msg[3].Input.file_path: got %v, want /tmp/foo", got)
	}
	if msgs[4].Type != MessageToolResult || msgs[4].CallID != "call_1" {
		t.Errorf("msg[4]: got %+v, want tool-result call_1", msgs[4])
	}
	// block.Content is json.RawMessage, so string(block.Content) keeps the
	// surrounding JSON quotes — same behaviour as claude.go / qwen.go.
	// The raw bytes for "file contents here" are the 20-byte JSON string
	// literal including the surrounding quotes.
	if msgs[4].Output != `"file contents here"` {
		t.Errorf("msg[4].Output: got %q, want %q (raw JSON string)", msgs[4].Output, `"file contents here"`)
	}
	if msgs[5].Type != MessageText || msgs[5].Content != "Done." {
		t.Errorf("msg[5]: got %+v, want text 'Done.'", msgs[5])
	}

	// Usage: the result frame's `usage` block is authoritative and overrides
	// the incremental accumulation from assistant messages. codebuddy is
	// assumed not to emit modelUsage, so codebuddyResultUsage keys the flat
	// usage block by fallbackModel ("codebuddy-pro") and the result frame's
	// totals (150 input / 30 output) land in result.usage.
	if result.usage == nil {
		t.Fatalf("expected per-model usage map, got nil")
	}
	u, ok := result.usage["codebuddy-pro"]
	if !ok {
		t.Fatalf("expected usage for codebuddy-pro, got %#v", result.usage)
	}
	if u.InputTokens != 150 || u.OutputTokens != 30 {
		t.Errorf("usage[codebuddy-pro]: got input=%d output=%d, want 150/30", u.InputTokens, u.OutputTokens)
	}

	// No control_request in this stream → stdin must be empty.
	if stdin.Len() != 0 {
		t.Errorf("stdin: expected no control_response writes, got %q", stdin.String())
	}
}

// ── TestCodebuddyParseEvents_ErrorResult ──
//
// result frame with is_error:true surfaces as status=failed and carries the
// error.message into Result.Error. This is the path the daemon maps to a
// failed task (e.g. codebuddy exits with auth error, sandbox EPERM, etc.).
// Mirrors TestQwenParseEvents_ErrorResult.

func TestCodebuddyParseEvents_ErrorResult(t *testing.T) {
	t.Parallel()

	b := newCodebuddyTestBackend(t)
	ch := make(chan Message, 16)

	// Mirrors a real Claude Code SDK error result (same shape qwen emits):
	// `{"type":"result","subtype":"error_during_execution","session_id":"...","is_error":true,"duration_ms":0,"num_turns":0,"usage":{"input_tokens":0,"output_tokens":0},"error":{"message":"No auth type is selected."}}`
	ndjson := `{"type":"result","subtype":"error_during_execution","session_id":"codebuddy-err","uuid":"u1","is_error":true,"duration_ms":0,"num_turns":0,"usage":{"input_tokens":0,"output_tokens":0},"permission_denials":[],"error":{"message":"No auth type is selected. Please configure an auth type before running in non-interactive mode."}}`

	var stdin bytes.Buffer
	result := b.processCodebuddyEvents(strings.NewReader(ndjson), ch, &stdin, noopCloseStdinCodebuddy, "unknown")
	close(ch)

	if result.status != "failed" {
		t.Errorf("status: got %q, want %q", result.status, "failed")
	}
	if result.sessionID != "codebuddy-err" {
		t.Errorf("sessionID: got %q, want %q", result.sessionID, "codebuddy-err")
	}
	if !strings.Contains(result.errMsg, "No auth type is selected") {
		t.Errorf("errMsg: got %q, want it to contain 'No auth type is selected'", result.errMsg)
	}

	// An error result must emit a MessageError so streaming consumers see it.
	var sawErrorMsg bool
	for m := range ch {
		if m.Type == MessageError && strings.Contains(m.Content, "No auth type is selected") {
			sawErrorMsg = true
		}
	}
	if !sawErrorMsg {
		t.Errorf("expected a MessageError on the channel carrying the error message")
	}
}

// ── TestCodebuddyControlRequest ──
//
// A control_request event (codebuddy's permission prompt) must produce a
// control_response on stdin with behavior:"allow" and the request_id echoed
// back, so the daemon runs fully autonomously. Mirrors the claude / qwen
// backend's contract.

func TestCodebuddyControlRequest(t *testing.T) {
	t.Parallel()

	b := newCodebuddyTestBackend(t)
	ch := make(chan Message, 16)

	ndjson := `{"type":"control_request","request_id":"req-42","request":{"subtype":"can_use_tool","tool_name":"Write","tool_use_id":"call_1","input":{"file_path":"/tmp/out.txt","content":"hi"},"permission_suggestions":null,"blocked_path":null}}`

	var stdin bytes.Buffer
	b.processCodebuddyEvents(strings.NewReader(ndjson), ch, &stdin, noopCloseStdinCodebuddy, "unknown")
	close(ch)

	out := stdin.String()
	if out == "" {
		t.Fatal("expected a control_response written to stdin, got empty")
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("control_response must be newline-terminated; got %q", out)
	}

	var resp struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Response  struct {
				Behavior     string         `json:"behavior"`
				UpdatedInput map[string]any `json:"updatedInput"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal control_response: %v\nraw: %s", err, out)
	}
	if resp.Type != "control_response" {
		t.Errorf("type: got %q, want control_response", resp.Type)
	}
	if resp.Response.Subtype != "success" {
		t.Errorf("subtype: got %q, want success", resp.Response.Subtype)
	}
	if resp.Response.RequestID != "req-42" {
		t.Errorf("request_id: got %q, want req-42", resp.Response.RequestID)
	}
	if resp.Response.Response.Behavior != "allow" {
		t.Errorf("behavior: got %q, want allow", resp.Response.Response.Behavior)
	}
	if resp.Response.Response.UpdatedInput["file_path"] != "/tmp/out.txt" {
		t.Errorf("updatedInput.file_path: got %v, want /tmp/out.txt", resp.Response.Response.UpdatedInput["file_path"])
	}
}

// ── TestCodebuddyInput ──
//
// Verifies buildCodebuddyInput produces a single NDJSON frame with the
// Claude Code SDK / qwen-code input contract: a user turn wrapping a text
// content block carrying the prompt, terminated by a newline.

func TestCodebuddyInput(t *testing.T) {
	t.Parallel()

	data, err := buildCodebuddyInput("hello world")
	if err != nil {
		t.Fatalf("buildCodebuddyInput: unexpected error: %v", err)
	}

	// Exactly one trailing newline.
	if !strings.HasSuffix(string(data), "\n") {
		t.Errorf("input must end with newline; got %q", string(data))
	}
	if bytes.Count(data, []byte("\n")) != 1 {
		t.Errorf("input must be exactly one NDJSON line; got %q", string(data))
	}

	// Trim the trailing newline and unmarshal as JSON.
	var frame struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &frame); err != nil {
		t.Fatalf("unmarshal input frame: %v\nraw: %s", err, data)
	}
	if frame.Type != "user" {
		t.Errorf("type: got %q, want user", frame.Type)
	}
	if frame.Message.Role != "user" {
		t.Errorf("role: got %q, want user", frame.Message.Role)
	}
	if len(frame.Message.Content) != 1 {
		t.Fatalf("content: got %d blocks, want 1", len(frame.Message.Content))
	}
	if frame.Message.Content[0].Type != "text" {
		t.Errorf("content[0].type: got %q, want text", frame.Message.Content[0].Type)
	}
	if frame.Message.Content[0].Text != "hello world" {
		t.Errorf("content[0].text: got %q, want 'hello world'", frame.Message.Content[0].Text)
	}

	// writeCodebuddyInput must write the same bytes to the writer.
	var buf bytes.Buffer
	if err := writeCodebuddyInput(&buf, "hello world"); err != nil {
		t.Fatalf("writeCodebuddyInput: unexpected error: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Errorf("writeCodebuddyInput wrote %q, buildCodebuddyInput returned %q", buf.String(), string(data))
	}
}

// ── TestCodebuddyNewRegistration ──
//
// Verifies New("codebuddy") returns a non-nil *codebuddyBackend so the
// dispatcher can route to it. Pins the case label in the New() switch.
// Mirrors TestQwenNewRegistration.

func TestCodebuddyNewRegistration(t *testing.T) {
	b, err := New("codebuddy", Config{})
	if err != nil {
		t.Fatalf("New(codebuddy): unexpected error: %v", err)
	}
	if b == nil {
		t.Fatal("New(codebuddy): returned nil Backend")
	}
	if _, ok := b.(*codebuddyBackend); !ok {
		t.Errorf("New(codebuddy): expected *codebuddyBackend, got %T", b)
	}

	// Unknown agent type must still error.
	if _, err := New("nope", Config{}); err == nil {
		t.Error("New(nope): expected error for unknown agent type, got nil")
	}
}

// ── TestCodebuddyMissingExecutable ──
//
// A path that does not exist on PATH returns a clean error from Execute
// without panicking or hanging. No CLI is invoked — LookPath fails fast.
// Constructs the backend directly (rather than via New) because the New()
// switch is not yet wired for codebuddy; mirrors TestQwenMissingExecutable
// otherwise.

func TestCodebuddyMissingExecutable(t *testing.T) {
	t.Parallel()

	// Construct directly — New("codebuddy") is not wired yet.
	b := &codebuddyBackend{cfg: Config{
		ExecutablePath: "/nonexistent/codebuddy-binary-xyz",
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sess, execErr := b.Execute(ctx, "hello", ExecOptions{})
	if sess != nil {
		t.Errorf("Execute: expected nil Session on missing executable, got %+v", sess)
	}
	if execErr == nil {
		t.Fatal("Execute: expected error for missing executable, got nil")
	}
	msg := execErr.Error()
	if !strings.Contains(msg, "codebuddy executable not found") {
		t.Errorf("Execute error %q: missing %q", msg, "codebuddy executable not found")
	}
}

// ── TestCodebuddyParseEvents_EmptyAndMalformed ──
//
// Empty input, blank lines, and non-JSON lines (e.g. codebuddy's YOLO
// warning banner that leaks to stdout) must be skipped silently — the
// scanner must not error out and the result must default to
// status=completed. Mirrors TestQwenParseEvents_EmptyAndMalformed.

func TestCodebuddyParseEvents_EmptyAndMalformed(t *testing.T) {
	t.Parallel()

	b := newCodebuddyTestBackend(t)
	ch := make(chan Message, 16)

	// Mix of blank lines, a non-JSON banner, and no terminal result event.
	ndjson := strings.Join([]string{
		``,
		`Warning: running headless with --yolo and no sandbox.`,
		``,
		``,
	}, "\n")

	var stdin bytes.Buffer
	result := b.processCodebuddyEvents(strings.NewReader(ndjson), ch, &stdin, noopCloseStdinCodebuddy, "unknown")
	close(ch)

	if result.status != "completed" {
		t.Errorf("status: got %q, want completed (no error event seen)", result.status)
	}
	if result.output != "" {
		t.Errorf("output: got %q, want empty", result.output)
	}
	if result.sessionID != "" {
		t.Errorf("sessionID: got %q, want empty", result.sessionID)
	}

	// No events should have been emitted.
	msgs := drainCodebuddy(ch)
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages, got %d: %+v", len(msgs), msgs)
	}
}

// drainCodebuddy collects all messages currently buffered on ch. Used by
// tests that need to assert the message count after closing the channel.
func drainCodebuddy(ch <-chan Message) []Message {
	var out []Message
	for m := range ch {
		out = append(out, m)
	}
	return out
}

// ── TestCodebuddyExecuteSmoke ──
//
// End-to-end smoke test that spawns a real codebuddy CLI. Skipped by
// default — it requires (a) codebuddy installed on PATH, (b) auth
// configured (no auth results in an immediate error result which we DO
// assert on), and (c) a writable cwd.
//
// Enable with: CODEBUDDY_SMOKE=1 go test -run TestCodebuddyExecuteSmoke ./internal/adapter/...
//
// When auth is not configured, codebuddy is expected to emit a result
// event with is_error:true (matching the Claude Code SDK contract) — we
// accept that as a successful protocol round-trip (the stream-json
// contract works end-to-end) and only fail the test if codebuddy produces
// no result event at all. This is the test that will confirm or refute the
// schema assumption documented in codebuddy.go's package doc. Mirrors
// TestQwenExecuteSmoke.

func TestCodebuddyExecuteSmoke(t *testing.T) {
	if os.Getenv("CODEBUDDY_SMOKE") != "1" {
		t.Skip("skipping codebuddy smoke test; set CODEBUDDY_SMOKE=1 to enable")
	}

	if _, err := exec.LookPath("codebuddy"); err != nil {
		t.Skipf("codebuddy CLI not found on PATH: %v", err)
	}

	tmpDir, err := os.MkdirTemp("/tmp", "codebuddy-smoke-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Construct directly — New("codebuddy") is not wired yet.
	b := &codebuddyBackend{cfg: Config{Logger: slog.Default()}}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess, execErr := b.Execute(ctx, "say hi in 3 words", ExecOptions{Cwd: tmpDir, Timeout: 45 * time.Second})
	if execErr != nil {
		t.Fatalf("Execute: unexpected error: %v", execErr)
	}
	if sess == nil {
		t.Fatal("Execute: returned nil Session")
	}

	select {
	case res := <-sess.Result:
		// A real auth-configured run yields status=completed; an
		// unauthenticated run yields status=failed with an auth error
		// message. Both prove the stream-json protocol round-tripped.
		if res.Status != "completed" && res.Status != "failed" {
			t.Errorf("status: got %q, want completed or failed", res.Status)
		}
		if res.Status == "failed" && res.Error == "" {
			t.Errorf("failed result: expected non-empty Error, got empty")
		}
		t.Logf("smoke result: status=%s session=%s output=%q error=%q usage=%v",
			res.Status, res.SessionID, res.Output, res.Error, res.Usage)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for codebuddy result: %v", ctx.Err())
	}
}
