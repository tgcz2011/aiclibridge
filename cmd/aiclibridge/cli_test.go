// Package main hosts the unit tests for aiclibridge's CLI helpers.
//
// cli_test.go covers the pure functions only — collectPrompt,
// exitCodeForStatus, printEvent, printVersion, plus the streamEvents /
// drainAndSummarize consumers driven by a hand-built RunHandle. The
// end-to-end subcommand paths (runServe / runRun / runCancel / runGet)
// are intentionally NOT exercised here: they spawn subprocesses or
// open HTTP listeners, which belong in an integration suite rather than
// `go test ./...`. Every test runs without network or filesystem
// fixtures beyond a single temp file for the stdin-redirect case.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/tgcz2011/aiclibridge/internal/facade"
	"github.com/tgcz2011/aiclibridge/pkg/protocol"
)

// ── collectPrompt ──

// TestCollectPromptPositional verifies that positional args are joined
// with a single space. This is the path `aiclibridge run hello world`
// hits — no stdin interaction, so the test never touches os.Stdin.
func TestCollectPromptPositional(t *testing.T) {
	got, err := collectPrompt([]string{"hello", "world"})
	if err != nil {
		t.Fatalf("collectPrompt: %v", err)
	}
	if want := "hello world"; got != want {
		t.Errorf("collectPrompt positional: got %q, want %q", got, want)
	}
}

// TestCollectPromptSinglePositional verifies that a single positional
// arg round-trips unchanged (no trailing space, no joining).
func TestCollectPromptSinglePositional(t *testing.T) {
	got, err := collectPrompt([]string{"just-one-arg"})
	if err != nil {
		t.Fatalf("collectPrompt: %v", err)
	}
	if want := "just-one-arg"; got != want {
		t.Errorf("collectPrompt single: got %q, want %q", got, want)
	}
}

// TestCollectPromptStdin verifies that when no positional args are
// present and stdin is a regular file (simulating a pipe), the file's
// content is read and trimmed. The temp file is NOT a char device, so
// collectPrompt's TTY guard falls through to the io.ReadAll path.
func TestCollectPromptStdin(t *testing.T) {
	// Write the prompt to a temp file, then redirect os.Stdin to it.
	tmp, err := os.CreateTemp("", "aiclibridge-prompt-*")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString("  hello from stdin  \n"); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("close temp: %v", err)
	}

	f, err := os.Open(tmp.Name())
	if err != nil {
		t.Fatalf("open temp: %v", err)
	}
	defer f.Close()

	origStdin := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = origStdin })

	got, err := collectPrompt(nil)
	if err != nil {
		t.Fatalf("collectPrompt: %v", err)
	}
	if want := "hello from stdin"; got != want {
		t.Errorf("collectPrompt stdin: got %q, want %q", got, want)
	}
}

// TestCollectPromptCharDevice verifies that when no positional args are
// present and stdin is a char device (simulating a TTY), collectPrompt
// returns the empty string rather than blocking on a read. os.DevNull
// is a char device on every supported platform, so it stands in for a
// terminal here. This is the path `aiclibridge run` with no args and no
// pipe hits — runRun then surfaces the "prompt required" error.
func TestCollectPromptCharDevice(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("open %s: %v", os.DevNull, err)
	}
	defer devNull.Close()

	origStdin := os.Stdin
	os.Stdin = devNull
	t.Cleanup(func() { os.Stdin = origStdin })

	got, err := collectPrompt(nil)
	if err != nil {
		t.Fatalf("collectPrompt: %v", err)
	}
	if got != "" {
		t.Errorf("collectPrompt char device: got %q, want empty", got)
	}
}

// ── exitCodeForStatus ──

// TestExitCodeForStatus is a table test covering every documented
// status plus the unknown fallback. The expected codes follow the
// shell conventions documented on exitCodeForStatus: 0/1/130/124.
func TestExitCodeForStatus(t *testing.T) {
	tests := []struct {
		status string
		want   int
	}{
		{"completed", 0},
		{"failed", 1},
		{"cancelled", 130},
		{"timeout", 124},
		{"", 1},        // unknown — falls through to default
		{"unknown", 1}, // unknown — falls through to default
		{"running", 1}, // a non-terminal status, treated as failure
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			if got := exitCodeForStatus(tt.status); got != tt.want {
				t.Errorf("exitCodeForStatus(%q) = %d, want %d", tt.status, got, tt.want)
			}
		})
	}
}

// ── printEvent ──

// TestPrintEvent is a table test covering every protocol.EventType the
// formatter handles. Each case asserts the output contains the
// type-specific prefix and the load-bearing fields, without pinning the
// exact byte layout (so a future tweak to spacing doesn't break tests).
func TestPrintEvent(t *testing.T) {
	tests := []struct {
		name     string
		event    protocol.Event
		wantHas  []string // substrings that MUST appear
		wantNot  []string // substrings that must NOT appear
	}{
		{
			name:    "text",
			event:   protocol.Event{Type: protocol.EventText, Content: "hello world"},
			wantHas: []string{"hello world"},
			wantNot: []string{"[text]"}, // text is raw, no prefix
		},
		{
			name:    "thinking",
			event:   protocol.Event{Type: protocol.EventThinking, Content: "reasoning here"},
			wantHas: []string{"reasoning here"},
			wantNot: []string{"[thinking]"}, // thinking is raw, no prefix
		},
		{
			name: "tool_use",
			event: protocol.Event{
				Type:   protocol.EventToolUse,
				Tool:   "bash",
				CallID: "call-1",
				Input:  json.RawMessage(`{"cmd":"ls"}`),
			},
			wantHas: []string{"[tool_use]", "bash", "call-1", `"cmd":"ls"`},
		},
		{
			name: "tool_result",
			event: protocol.Event{
				Type:   protocol.EventToolResult,
				Tool:   "bash",
				CallID: "call-1",
				Output: "file.txt",
			},
			wantHas: []string{"[tool_result]", "bash", "call-1", "file.txt"},
		},
		{
			name: "status",
			event: protocol.Event{
				Type:      protocol.EventStatus,
				Status:    "running",
				SessionID: "sess-1",
			},
			wantHas: []string{"[status]", "running", "session_id=sess-1"},
		},
		{
			name: "status_no_session",
			event: protocol.Event{
				Type:   protocol.EventStatus,
				Status: "idle",
			},
			wantHas:  []string{"[status]", "idle"},
			wantNot:  []string{"session_id"},
		},
		{
			name:    "error",
			event:   protocol.Event{Type: protocol.EventError, Content: "kaboom"},
			wantHas: []string{"[error]", "kaboom"},
		},
		{
			name: "log",
			event: protocol.Event{
				Type:    protocol.EventLog,
				Level:   "info",
				Content: "progress update",
			},
			wantHas: []string{"[log:info]", "progress update"},
		},
		{
			name: "result_completed",
			event: protocol.Event{
				Type: protocol.EventResult,
				Result: &protocol.ResultPayload{
					Status:     "completed",
					DurationMs: 1234,
					SessionID:  "sess-1",
				},
			},
			wantHas: []string{"[result]", "completed", "duration_ms=1234", "session_id=sess-1"},
		},
		{
			name: "result_failed_with_error",
			event: protocol.Event{
				Type: protocol.EventResult,
				Result: &protocol.ResultPayload{
					Status: "failed",
					Error:  "adapter crashed",
				},
			},
			wantHas: []string{"[result]", "failed", "error=", "adapter crashed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			printEvent(tt.event, &buf)
			got := buf.String()
			for _, want := range tt.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("printEvent %s: output %q missing substring %q", tt.name, got, want)
				}
			}
			for _, notWant := range tt.wantNot {
				if strings.Contains(got, notWant) {
					t.Errorf("printEvent %s: output %q contains unwanted substring %q", tt.name, got, notWant)
				}
			}
		})
	}
}

// ── printVersion ──

// TestPrintVersion verifies that the version banner contains the
// canonical version string and the binary name. runVersion delegates
// to printVersion, so this also covers the `aiclibridge version`
// output (minus the os.Stdout redirection).
func TestPrintVersion(t *testing.T) {
	var buf bytes.Buffer
	printVersion(&buf)
	out := buf.String()
	if !strings.Contains(out, Version) {
		t.Errorf("printVersion: output %q missing version %q", out, Version)
	}
	if !strings.Contains(out, "aiclibridge") {
		t.Errorf("printVersion: output %q missing binary name", out)
	}
	// Without ldflags stamping, Build/Commit are empty and the output
	// should be a single line.
	if Build == "" && Commit == "" {
		lines := strings.Count(out, "\n")
		if lines != 1 {
			t.Errorf("printVersion: expected 1 line, got %d (output %q)", lines, out)
		}
	}
}

// ── streamEvents ──

// TestStreamEvents verifies the live consumer's routing: text goes to
// stdout, tool_use/result go to stderr, and the terminal status is
// extracted from the EventResult. The RunHandle is hand-built with a
// buffered, pre-populated Events channel so no facade is needed.
func TestStreamEvents(t *testing.T) {
	ch := make(chan protocol.Event, 4)
	ch <- protocol.Event{Type: protocol.EventText, Content: "hello "}
	ch <- protocol.Event{Type: protocol.EventThinking, Content: "thinking..."}
	ch <- protocol.Event{Type: protocol.EventToolUse, Tool: "bash", CallID: "c1"}
	ch <- protocol.Event{Type: protocol.EventResult, Result: &protocol.ResultPayload{
		Status: "completed", SessionID: "sess-1",
	}}
	close(ch)

	handle := &facade.RunHandle{Events: (<-chan protocol.Event)(ch)}
	var stdout, stderr bytes.Buffer
	status := streamEvents(handle, &stdout, &stderr)

	if status != "completed" {
		t.Errorf("streamEvents status: got %q, want completed", status)
	}
	stdoutStr := stdout.String()
	if !strings.Contains(stdoutStr, "hello ") {
		t.Errorf("streamEvents stdout: %q missing text content", stdoutStr)
	}
	if !strings.Contains(stdoutStr, "thinking...") {
		t.Errorf("streamEvents stdout: %q missing thinking content", stdoutStr)
	}
	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, "[tool_use]") {
		t.Errorf("streamEvents stderr: %q missing tool_use line", stderrStr)
	}
	if !strings.Contains(stderrStr, "[result]") {
		t.Errorf("streamEvents stderr: %q missing result line", stderrStr)
	}
	if !strings.Contains(stderrStr, "session_id=sess-1") {
		t.Errorf("streamEvents stderr: %q missing session_id", stderrStr)
	}
}

// TestStreamEventsNoResult verifies the defensive default: if the
// Events channel closes without a terminal EventResult, streamEvents
// falls back to "completed" so the process still exits 0. This mirrors
// buildRunResult's default in the API layer.
func TestStreamEventsNoResult(t *testing.T) {
	ch := make(chan protocol.Event, 1)
	ch <- protocol.Event{Type: protocol.EventText, Content: "orphan text"}
	close(ch)

	handle := &facade.RunHandle{Events: (<-chan protocol.Event)(ch)}
	var stdout, stderr bytes.Buffer
	status := streamEvents(handle, &stdout, &stderr)

	if status != "completed" {
		t.Errorf("streamEvents no-result: got %q, want completed", status)
	}
}

// ── drainAndSummarize ──

// TestDrainAndSummarize verifies the --no-stream consumer: events are
// collected silently, the terminal Result.Output is printed to stdout,
// and the summary line goes to stderr.
func TestDrainAndSummarize(t *testing.T) {
	ch := make(chan protocol.Event, 3)
	ch <- protocol.Event{Type: protocol.EventText, Content: "partial text"}
	ch <- protocol.Event{Type: protocol.EventToolUse, Tool: "bash", CallID: "c1"}
	ch <- protocol.Event{Type: protocol.EventResult, Result: &protocol.ResultPayload{
		Status: "completed", Output: "final answer", SessionID: "sess-1",
	}}
	close(ch)

	handle := &facade.RunHandle{Events: (<-chan protocol.Event)(ch)}
	var stdout, stderr bytes.Buffer
	status := drainAndSummarize(handle, &stdout, &stderr)

	if status != "completed" {
		t.Errorf("drainAndSummarize status: got %q, want completed", status)
	}
	stdoutStr := stdout.String()
	if !strings.Contains(stdoutStr, "final answer") {
		t.Errorf("drainAndSummarize stdout: %q missing terminal output", stdoutStr)
	}
	// The partial text should NOT appear on stdout because the terminal
	// Result.Output takes precedence.
	if strings.Contains(stdoutStr, "partial text") {
		t.Errorf("drainAndSummarize stdout: %q leaked fallback text", stdoutStr)
	}
	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, "[result]") {
		t.Errorf("drainAndSummarize stderr: %q missing result summary", stderrStr)
	}
}

// TestDrainAndSummarizeFallbackOutput verifies that when the terminal
// result has no Output, text events are concatenated as the fallback
// content — mirroring aggregateText in the API layer so a run that
// emitted text but no terminal Output still yields its content.
func TestDrainAndSummarizeFallbackOutput(t *testing.T) {
	ch := make(chan protocol.Event, 3)
	ch <- protocol.Event{Type: protocol.EventText, Content: "chunk1 "}
	ch <- protocol.Event{Type: protocol.EventText, Content: "chunk2"}
	ch <- protocol.Event{Type: protocol.EventResult, Result: &protocol.ResultPayload{
		Status: "completed", // no Output field
	}}
	close(ch)

	handle := &facade.RunHandle{Events: (<-chan protocol.Event)(ch)}
	var stdout, stderr bytes.Buffer
	status := drainAndSummarize(handle, &stdout, &stderr)

	if status != "completed" {
		t.Errorf("drainAndSummarize fallback status: got %q, want completed", status)
	}
	// drainAndSummarize appends a trailing newline when the output does
	// not end with one, so the expected stdout is the concatenated text
	// plus a single \n.
	if want := "chunk1 chunk2\n"; stdout.String() != want {
		t.Errorf("drainAndSummarize fallback stdout: got %q, want %q", stdout.String(), want)
	}
}

// ── runVersion ──

// TestRunVersion verifies that runVersion returns 0. The version string
// itself is validated by TestPrintVersion; this test only asserts the
// exit code and that something was written to stdout. stdout is
// redirected to a pipe because runVersion writes to os.Stdout directly.
func TestRunVersion(t *testing.T) {
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = origStdout
		r.Close()
		w.Close()
	})

	code := runVersion(nil)
	if code != 0 {
		t.Errorf("runVersion code: got %d, want 0", code)
	}
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	if !strings.Contains(string(out), Version) {
		t.Errorf("runVersion output: %q missing %q", string(out), Version)
	}
}

// ── printUsage ──

// TestPrintUsageSmoke is a smoke test: printUsage must mention every
// subcommand so a user scanning the help sees the full surface. It
// does NOT pin the exact wording — the banner evolves and the test
// should not become a maintenance burden.
func TestPrintUsageSmoke(t *testing.T) {
	var buf bytes.Buffer
	printUsage(&buf)
	out := buf.String()
	for _, cmd := range []string{"serve", "run", "agents", "models", "cancel", "get", "version"} {
		if !strings.Contains(out, cmd) {
			t.Errorf("printUsage: output missing subcommand %q", cmd)
		}
	}
}


