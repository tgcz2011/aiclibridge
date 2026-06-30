package adapter

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestOpencodeParseEvents feeds captured NDJSON through processEvents and
// verifies the resulting Message sequence. Mirrors multica's
// TestOpencodeProcessEventsHappyPath (opencode_test.go:497) with fixtures
// captured from real `opencode run --format json` output. No CLI is invoked.
func TestOpencodeParseEvents(t *testing.T) {
	b := &opencodeBackend{cfg: Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	ch := make(chan Message, 256)

	// Captured NDJSON stream: step_start → text → tool_use (completed) →
	// text → step_finish.
	ndjson := strings.Join([]string{
		`{"type":"step_start","timestamp":1000,"sessionID":"ses_happy","part":{"type":"step-start"}}`,
		`{"type":"text","timestamp":1001,"sessionID":"ses_happy","part":{"type":"text","text":"Analyzing the issue..."}}`,
		`{"type":"tool_use","timestamp":1002,"sessionID":"ses_happy","part":{"tool":"bash","callID":"call_1","state":{"status":"completed","input":{"command":"ls"},"output":"file1.go\nfile2.go\n"}}}`,
		`{"type":"text","timestamp":1003,"sessionID":"ses_happy","part":{"type":"text","text":" Done."}}`,
		`{"type":"step_finish","timestamp":1004,"sessionID":"ses_happy","part":{"type":"step-finish"}}`,
	}, "\n")

	result := b.processEvents(strings.NewReader(ndjson), ch)
	close(ch)

	if result.status != "completed" {
		t.Errorf("status: got %q, want %q", result.status, "completed")
	}
	if result.sessionID != "ses_happy" {
		t.Errorf("sessionID: got %q, want %q", result.sessionID, "ses_happy")
	}
	if result.output != "Analyzing the issue... Done." {
		t.Errorf("output: got %q, want %q", result.output, "Analyzing the issue... Done.")
	}

	// Drain and verify message sequence.
	var msgs []Message
	for m := range ch {
		msgs = append(msgs, m)
	}

	// Expected: status(running), text, tool-use, tool-result, text = 5 msgs.
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Type != MessageStatus || msgs[0].Status != "running" {
		t.Errorf("msg[0]: got %+v, want status=running", msgs[0])
	}
	if msgs[1].Type != MessageText || msgs[1].Content != "Analyzing the issue..." {
		t.Errorf("msg[1]: got %+v", msgs[1])
	}
	if msgs[2].Type != MessageToolUse || msgs[2].Tool != "bash" || msgs[2].CallID != "call_1" {
		t.Errorf("msg[2]: got %+v, want tool-use(bash, call_1)", msgs[2])
	}
	if msgs[3].Type != MessageToolResult || msgs[3].Output != "file1.go\nfile2.go\n" {
		t.Errorf("msg[3]: got %+v, want tool-result", msgs[3])
	}
	if msgs[4].Type != MessageText || msgs[4].Content != " Done." {
		t.Errorf("msg[4]: got %+v", msgs[4])
	}
}

// TestOpencodeModelDiscovery feeds captured `opencode models --verbose` output
// through parseOpenCodeModels and verifies per-model thinking-level
// derivation from the variant catalog. No CLI is invoked.
func TestOpencodeModelDiscovery(t *testing.T) {
	// Captured `opencode models --verbose` output: one reasoning-capable
	// model (with a multi-level variant catalog) and one non-reasoning
	// model (no variants). The reasoning model must surface
	// Thinking.SupportedLevels in known order; the non-reasoning model
	// must NOT (its Thinking stays nil so the UI hides the picker).
	verboseOutput := `openai/gpt-5
{
  "id": "gpt-5",
  "reasoning": true,
  "variants": {
    "low":    {"reasoningEffort": "low"},
    "medium": {"reasoningEffort": "medium"},
    "high":   {"reasoningEffort": "high"},
    "max":    {"reasoningEffort": "max"},
    "minimal": {"disabled": true}
  }
}
openai/gpt-4o
{
  "id": "gpt-4o",
  "variants": {}
}
`
	models := parseOpenCodeModels(verboseOutput)
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d: %+v", len(models), models)
	}

	// First model: reasoning + variants → Thinking populated.
	gpt5 := models[0]
	if gpt5.ID != "openai/gpt-5" {
		t.Errorf("model[0].ID: got %q, want openai/gpt-5", gpt5.ID)
	}
	if gpt5.Provider != "openai" {
		t.Errorf("model[0].Provider: got %q, want openai", gpt5.Provider)
	}
	if gpt5.Thinking == nil {
		t.Fatal("model[0].Thinking is nil; expected non-nil for reasoning model")
	}
	wantValues := []string{"low", "medium", "high", "max"}
	var gotValues []string
	for _, lvl := range gpt5.Thinking.SupportedLevels {
		gotValues = append(gotValues, lvl.Value)
	}
	if !slices.Equal(gotValues, wantValues) {
		t.Errorf("Thinking.SupportedLevels values = %v, want %v (minimal should be excluded: disabled)", gotValues, wantValues)
	}
	// Spot-check one label.
	if gpt5.Thinking.SupportedLevels[0].Label != "Low" {
		t.Errorf("first level label: got %q, want Low", gpt5.Thinking.SupportedLevels[0].Label)
	}

	// Second model: no reasoning flag, no reasoning variants → no Thinking.
	gpt4o := models[1]
	if gpt4o.ID != "openai/gpt-4o" {
		t.Errorf("model[1].ID: got %q, want openai/gpt-4o", gpt4o.ID)
	}
	if gpt4o.Thinking != nil {
		t.Errorf("model[1].Thinking: got %+v, want nil (non-reasoning model)", gpt4o.Thinking)
	}
}

// TestOpencodeBlockedArgs verifies the daemon-managed flag set:
// --format, --variant, --dangerously-skip-permissions cannot be overridden
// by user-configured custom_args. The filter is the same generic helper
// the claude backend uses; this test pins the opencode-specific set.
func TestOpencodeBlockedArgs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	in := []string{
		"--format", "json",                    // blocked (with value)
		"--variant", "low",                    // blocked (with value)
		"--dangerously-skip-permissions",      // blocked (standalone)
		"--format=text",                       // blocked (inline value form)
		"--keep-me", "value",                  // safe — passes through
	}
	got := filterCustomArgs(in, opencodeBlockedArgs, logger)
	want := []string{"--keep-me", "value"}
	if !slices.Equal(got, want) {
		t.Errorf("filterCustomArgs mismatch:\n got  %#v\n want %#v", got, want)
	}
}

// TestOpencodeMissingExecutable verifies that a path that does not exist on
// PATH returns a clean error from Execute without panicking or hanging.
// No CLI is invoked — LookPath fails fast.
func TestOpencodeMissingExecutable(t *testing.T) {
	b, err := New("opencode", Config{ExecutablePath: "/nonexistent/opencode-binary-xyz"})
	if err != nil {
		t.Fatalf("New(opencode): unexpected error: %v", err)
	}
	if b == nil {
		t.Fatal("New(opencode): returned nil Backend")
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
	if !strings.Contains(msg, "opencode executable not found") {
		t.Errorf("Execute error %q: missing %q", msg, "opencode executable not found")
	}
}
