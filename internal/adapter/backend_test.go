package adapter

import (
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestMessageRoundTrip verifies the Message struct survives a full JSON
// encode/decode cycle for every MessageType. This is the lossless wire
// contract — if a field is dropped or its value is mangled, downstream
// consumers (SSE replay, store) silently lose data. The protocol layer
// (pkg/protocol/sse.go) has its own Event type with explicit json tags;
// this test guards the in-process Message form, which adapters marshal
// internally before handing events to the protocol layer.
func TestMessageRoundTrip(t *testing.T) {
	cases := []Message{
		{Type: MessageText, Content: "hello world"},
		{Type: MessageThinking, Content: "hmm, what if..."},
		{
			Type:   MessageToolUse,
			Tool:   "Bash",
			CallID: "call_abc",
			Input:  map[string]any{"cmd": "ls -la", "flag": "verbose"},
		},
		{
			Type:   MessageToolResult,
			Tool:   "Bash",
			CallID: "call_abc",
			Output: "total 4\n",
		},
		{Type: MessageStatus, Status: "running", SessionID: "sess-1"},
		{Type: MessageError, Content: "oops"},
		{Type: MessageLog, Content: "spawning subprocess", Level: "debug"},
	}

	for _, want := range cases {
		t.Run(string(want.Type), func(t *testing.T) {
			data, err := json.Marshal(want)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got Message
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("round-trip mismatch:\n got  %+v\n want %+v\n json %s", got, want, data)
			}
		})
	}
}

// TestNewFactory verifies the v1 factory contract: the five supported
// agent types return a non-nil Backend (the stub for now), and unknown
// types return an error whose message tells the caller which types v1
// does support. The "not supported in v1" wording is the user-facing
// signal that the agent type may be added later — we don't want
// callers to mis-parse a hard "unknown" as a typo they can fix.
func TestNewFactory(t *testing.T) {
	supported := []string{"claude", "codex", "opencode", "openclaw", "gemini"}
	for _, name := range supported {
		t.Run("supported/"+name, func(t *testing.T) {
			b, err := New(name, Config{})
			if err != nil {
				t.Fatalf("New(%q): unexpected error: %v", name, err)
			}
			if b == nil {
				t.Fatalf("New(%q): returned nil Backend", name)
			}
		})
	}

	t.Run("unsupported/copilot", func(t *testing.T) {
		b, err := New("copilot", Config{})
		if err == nil {
			t.Fatalf("New(copilot): expected error, got nil")
		}
		if b != nil {
			t.Errorf("New(copilot): expected nil Backend on error, got %T", b)
		}
		if !strings.Contains(err.Error(), "not supported in v1") {
			t.Errorf("New(copilot): error should mention \"not supported in v1\", got %q", err.Error())
		}
	})
}

// TestFilterCustomArgs exercises the protocol-critical-flag filter: a
// blocked flag AND its value (when the flag takes a value and the value
// is a separate arg) must both be dropped, while unrelated safe flags
// survive untouched. This is the user-config safety net — without it a
// custom_args value could override --output-format and break the
// stream-json contract.
func TestFilterCustomArgs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	in := []string{"--output-format", "json", "--model", "sonnet"}
	got := filterCustomArgs(in, claudeBlockedArgs, logger)
	want := []string{"--model", "sonnet"}
	if !slices.Equal(got, want) {
		t.Errorf("filterCustomArgs mismatch:\n got  %#v\n want %#v", got, want)
	}
}

// TestDeferredGemini verifies the geminiBackend stub returns a clear
// deferred error citing the evidence file. Path 2B of todo 8: gemini
// is deferred because no `gemini` binary on host + no ACP launch code
// in AionUi + no gemini adapter in multica = no ACP evidence (Metis
// rule: do not assume ACP without evidence). The stub is kept in the
// factory so New("gemini", ...) still returns a non-nil Backend —
// only Execute() errors with the deferral message.
func TestDeferredGemini(t *testing.T) {
	b, err := New("gemini", Config{})
	if err != nil {
		t.Fatalf("New(gemini): unexpected error: %v", err)
	}
	if b == nil {
		t.Fatalf("New(gemini): returned nil Backend")
	}
	sess, execErr := b.Execute(t.Context(), "hello", ExecOptions{})
	if sess != nil {
		t.Errorf("Execute: expected nil Session, got %+v", sess)
	}
	if execErr == nil {
		t.Fatal("Execute: expected deferred error, got nil")
	}
	msg := execErr.Error()
	for _, want := range []string{"deferred", "ACP-capable", "task-8-aiclibridge-mvp.txt", "4 CLIs"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Execute error %q: missing %q", msg, want)
		}
	}
}
