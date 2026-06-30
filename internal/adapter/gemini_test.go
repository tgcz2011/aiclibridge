package adapter

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

// These tests exercise the gemini adapter's pure logic (args builder,
// event parser, blocked-arg filter, input frame marshalling) and the
// Execute-not-deferred regression. No real gemini-cli binary is invoked
// (none is installed on this host); processGeminiEvents is fed stubbed
// NDJSON and Execute is exercised only via its LookPath preflight, which
// fails fast on a non-existent executable path. See the EXPERIMENTAL
// note at the top of gemini.go.

func newGeminiTestBackend() *geminiBackend {
	return &geminiBackend{cfg: Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
}

// TestGeminiArgs verifies buildGeminiArgs assembles the stream-json flag
// set and projects opts fields onto the right gemini-cli flags. Table-
// driven so a future flag rename is one row away from a failing test.
func TestGeminiArgs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	base := []string{
		"--bare",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--yolo",
	}

	cases := []struct {
		name string
		opts ExecOptions
		want []string
	}{
		{
			name: "bare no opts",
			opts: ExecOptions{},
			want: base,
		},
		{
			name: "model projected onto --model",
			opts: ExecOptions{Model: "gemini-2.5-pro"},
			want: append(append([]string{}, base...), "--model", "gemini-2.5-pro"),
		},
		{
			name: "system prompt projected onto --append-system-prompt (not --system-prompt)",
			opts: ExecOptions{SystemPrompt: "be terse"},
			want: append(append([]string{}, base...), "--append-system-prompt", "be terse"),
		},
		{
			name: "max turns projected onto --max-turns",
			opts: ExecOptions{MaxTurns: 5},
			want: append(append([]string{}, base...), "--max-turns", "5"),
		},
		{
			name: "full projection",
			opts: ExecOptions{
				Model:        "gemini-2.5-flash",
				SystemPrompt: "you are a router",
				MaxTurns:     3,
			},
			want: append(append([]string{}, base...),
				"--model", "gemini-2.5-flash",
				"--append-system-prompt", "you are a router",
				"--max-turns", "3",
			),
		},
		{
			name: "thinking level ignored (no stable gemini flag)",
			opts: ExecOptions{ThinkingLevel: "high"},
			want: base,
		},
		{
			name: "safe custom args pass through",
			opts: ExecOptions{
				CustomArgs: []string{"--foo", "bar", "--flag"},
			},
			want: append(append([]string{}, base...), "--foo", "bar", "--flag"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGeminiArgs(tc.opts, logger)
			if !slices.Equal(got, tc.want) {
				t.Errorf("buildGeminiArgs mismatch:\n got  %#v\n want %#v", got, tc.want)
			}
		})
	}
}

// TestGeminiResume pins the resume path: a non-empty ResumeSessionID is
// projected onto `--resume <id>` (not --session-id). Mirrors the claude
// backend's resume wiring; gemini-cli exposes both flags but --resume is
// the continue-a-prior-session form.
func TestGeminiResume(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	args := buildGeminiArgs(ExecOptions{ResumeSessionID: "sess-123"}, logger)

	idx := slices.Index(args, "--resume")
	if idx < 0 {
		t.Fatalf("--resume flag missing from args: %#v", args)
	}
	if idx+1 >= len(args) || args[idx+1] != "sess-123" {
		t.Fatalf("--resume value: got %#v, want sess-123", args[idx+1:])
	}
	if slices.Index(args, "--session-id") >= 0 {
		t.Errorf("--session-id should NOT be injected for resume; --resume is the resume path")
	}
}

// TestGeminiBlockedArgs verifies the daemon-managed flag set: the
// stream-json protocol flags, auto-approve flags, mcp-config, resume,
// model, system-prompt, and max-turns cannot be overridden by user-
// configured custom_args. The filter is the same generic helper every
// backend uses; this test pins the gemini-specific set.
func TestGeminiBlockedArgs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	in := []string{
		"--bare",                            // blocked (standalone)
		"--output-format", "text",           // blocked (with value)
		"--input-format", "text",            // blocked (with value)
		"--yolo",                            // blocked (standalone)
		"--approval-mode", "default",       // blocked (with value)
		"--mcp-config", "/etc/mcp.json",     // blocked (with value)
		"--session-id", "hijack",           // blocked (with value)
		"--resume", "other-sess",            // blocked (with value)
		"-p",                                // blocked (standalone)
		"--prompt", "injected",             // blocked (with value)
		"--model", "overridden",            // blocked (with value)
		"--system-prompt", "wipe",          // blocked (with value)
		"--append-system-prompt", "wipe",    // blocked (with value)
		"--max-turns", "99",                // blocked (with value)
		"--output-format=json",             // blocked (inline value form)
		"--keep-me", "value",               // safe — passes through
		"--also-keep",                      // safe — passes through
	}
	got := filterCustomArgs(in, geminiBlockedArgs, logger)
	want := []string{"--keep-me", "value", "--also-keep"}
	if !slices.Equal(got, want) {
		t.Errorf("filterCustomArgs mismatch:\n got  %#v\n want %#v", got, want)
	}
}

// TestGeminiParseEvents feeds stubbed NDJSON (mirroring the opencode
// stream-json schema, which gemini-cli is assumed to share) through
// processGeminiEvents and verifies the resulting Message sequence and
// accumulated Result fields. No CLI is invoked.
func TestGeminiParseEvents(t *testing.T) {
	b := newGeminiTestBackend()
	ch := make(chan Message, 256)

	// Stubbed NDJSON stream: step_start → text → tool_use (completed) →
	// text → step_finish (with tokens). Schema mirrors opencode's
	// {type, sessionID, part:{...}} because gemini-cli shares lineage.
	ndjson := strings.Join([]string{
		`{"type":"step_start","timestamp":1000,"sessionID":"ses_gem","part":{"type":"step-start"}}`,
		`{"type":"text","timestamp":1001,"sessionID":"ses_gem","part":{"type":"text","text":"Hello, "}}`,
		`{"type":"tool_use","timestamp":1002,"sessionID":"ses_gem","part":{"tool":"read_file","callID":"call_1","state":{"status":"completed","input":{"path":"a.go"},"output":"package a\n"}}}`,
		`{"type":"text","timestamp":1003,"sessionID":"ses_gem","part":{"type":"text","text":"world."}}`,
		`{"type":"step_finish","timestamp":1004,"sessionID":"ses_gem","part":{"type":"step-finish","tokens":{"input":120,"output":30,"cache":{"read":10,"write":5}}}}`,
	}, "\n")

	result := b.processGeminiEvents(strings.NewReader(ndjson), ch)
	close(ch)

	if result.status != "completed" {
		t.Errorf("status: got %q, want %q", result.status, "completed")
	}
	if result.sessionID != "ses_gem" {
		t.Errorf("sessionID: got %q, want %q", result.sessionID, "ses_gem")
	}
	if result.output != "Hello, world." {
		t.Errorf("output: got %q, want %q", result.output, "Hello, world.")
	}
	if result.usage.InputTokens != 120 {
		t.Errorf("usage.InputTokens: got %d, want 120", result.usage.InputTokens)
	}
	if result.usage.OutputTokens != 30 {
		t.Errorf("usage.OutputTokens: got %d, want 30", result.usage.OutputTokens)
	}
	if result.usage.CacheReadTokens != 10 {
		t.Errorf("usage.CacheReadTokens: got %d, want 10", result.usage.CacheReadTokens)
	}
	if result.usage.CacheWriteTokens != 5 {
		t.Errorf("usage.CacheWriteTokens: got %d, want 5", result.usage.CacheWriteTokens)
	}

	var msgs []Message
	for m := range ch {
		msgs = append(msgs, m)
	}

	// Expected: status(running), text, tool-use, tool-result, text = 5.
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Type != MessageStatus || msgs[0].Status != "running" {
		t.Errorf("msg[0]: got %+v, want status=running", msgs[0])
	}
	if msgs[1].Type != MessageText || msgs[1].Content != "Hello, " {
		t.Errorf("msg[1]: got %+v", msgs[1])
	}
	if msgs[2].Type != MessageToolUse || msgs[2].Tool != "read_file" || msgs[2].CallID != "call_1" {
		t.Errorf("msg[2]: got %+v, want tool-use(read_file, call_1)", msgs[2])
	}
	if msgs[2].Input == nil || msgs[2].Input["path"] != "a.go" {
		t.Errorf("msg[2].Input: got %+v, want path=a.go", msgs[2].Input)
	}
	if msgs[3].Type != MessageToolResult || msgs[3].Output != "package a\n" {
		t.Errorf("msg[3]: got %+v, want tool-result", msgs[3])
	}
	if msgs[4].Type != MessageText || msgs[4].Content != "world." {
		t.Errorf("msg[4]: got %+v", msgs[4])
	}
}

// TestGeminiParseErrorEvent verifies an error event flips the result to
// failed and emits a MessageError carrying the error message.
func TestGeminiParseErrorEvent(t *testing.T) {
	b := newGeminiTestBackend()
	ch := make(chan Message, 256)

	ndjson := `{"type":"error","sessionID":"ses_e","error":{"name":"model_error","data":{"message":"rate limited"}}}`
	result := b.processGeminiEvents(strings.NewReader(ndjson), ch)
	close(ch)

	if result.status != "failed" {
		t.Errorf("status: got %q, want failed", result.status)
	}
	if result.errMsg != "rate limited" {
		t.Errorf("errMsg: got %q, want %q", result.errMsg, "rate limited")
	}

	var msgs []Message
	for m := range ch {
		msgs = append(msgs, m)
	}
	if len(msgs) != 1 || msgs[0].Type != MessageError || msgs[0].Content != "rate limited" {
		t.Fatalf("messages: got %+v, want one MessageError(rate limited)", msgs)
	}
}

// TestGeminiInput verifies writeGeminiInput emits a newline-terminated
// JSON frame in the assumed stream-json user-turn shape and that the
// prompt text round-trips through a JSON unmarshal.
func TestGeminiInput(t *testing.T) {
	var sb strings.Builder
	if err := writeGeminiInput(&sb, "hello, gemini"); err != nil {
		t.Fatalf("writeGeminiInput: %v", err)
	}
	raw := sb.String()

	// NDJSON frames are newline-delimited; the single frame must end in \n.
	if !strings.HasSuffix(raw, "\n") {
		t.Errorf("frame missing trailing newline: %q", raw)
	}

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
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		t.Fatalf("unmarshal frame: %v\nraw: %s", err, raw)
	}
	if frame.Type != "user" {
		t.Errorf("frame.type: got %q, want user", frame.Type)
	}
	if frame.Message.Role != "user" {
		t.Errorf("frame.message.role: got %q, want user", frame.Message.Role)
	}
	if len(frame.Message.Content) != 1 {
		t.Fatalf("content blocks: got %d, want 1", len(frame.Message.Content))
	}
	if frame.Message.Content[0].Type != "text" {
		t.Errorf("content[0].type: got %q, want text", frame.Message.Content[0].Type)
	}
	if frame.Message.Content[0].Text != "hello, gemini" {
		t.Errorf("content[0].text: got %q, want %q", frame.Message.Content[0].Text, "hello, gemini")
	}

	// buildGeminiInput must return the same bytes (incl. trailing newline).
	data, err := buildGeminiInput("hello, gemini")
	if err != nil {
		t.Fatalf("buildGeminiInput: %v", err)
	}
	if string(data) != raw {
		t.Errorf("buildGeminiInput vs writeGeminiInput mismatch:\n build %q\n write %q", string(data), raw)
	}
}

// TestGeminiNotDeferred is the key regression: Execute must NOT return
// the historical "gemini adapter deferred" error. With an executable
// path that does not exist on PATH, Execute proceeds to LookPath and
// returns a "gemini executable not found" error instead — proving the
// real spawn path is wired up (the deferred stub is gone). No CLI is
// invoked; LookPath fails fast.
func TestGeminiNotDeferred(t *testing.T) {
	b, err := New("gemini", Config{ExecutablePath: "/nonexistent/gemini-binary-xyz"})
	if err != nil {
		t.Fatalf("New(gemini): unexpected error: %v", err)
	}
	if b == nil {
		t.Fatal("New(gemini): returned nil Backend")
	}

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

	if strings.Contains(msg, "deferred") {
		t.Errorf("Execute error must NOT be the deferred stub error, got %q", msg)
	}
	if strings.Contains(msg, "ACP-capable") {
		t.Errorf("Execute error must NOT mention ACP-capable (legacy deferred wording), got %q", msg)
	}
	if !strings.Contains(msg, "gemini executable not found") {
		t.Errorf("Execute error %q: missing %q", msg, "gemini executable not found")
	}
}
