package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestClaudeParseAssistant feeds a captured assistant stream-json line
// (text + thinking + tool_use blocks) through handleAssistant and asserts
// each block is emitted as the correct MessageType with the right fields.
// This is the wire-format contract — if a field name or block type drifts,
// every streaming consumer (SSE, SQLite store) silently loses data.
func TestClaudeParseAssistant(t *testing.T) {
	t.Parallel()

	b := &claudeBackend{cfg: Config{Logger: slog.Default()}}
	ch := make(chan Message, 16)
	var output strings.Builder
	usage := make(map[string]TokenUsage)

	msg := claudeSDKMessage{
		Type: "assistant",
		Message: mustMarshalClaude(t, claudeMessageContent{
			Role:  "assistant",
			Model: "claude-sonnet-4-6",
			Content: []claudeContentBlock{
				{Type: "text", Text: "Hello "},
				{Type: "thinking", Text: "let me think"},
				{Type: "tool_use", ID: "call-1", Name: "Read", Input: mustMarshalClaude(t, map[string]any{"path": "/tmp/foo"})},
				{Type: "text", Text: "world"},
			},
		}),
	}

	b.handleAssistant(msg, ch, &output, usage)

	if got := output.String(); got != "Hello world" {
		t.Fatalf("output: want %q, got %q", "Hello world", got)
	}

	// Drain the channel — order matches the order of content blocks.
	want := []Message{
		{Type: MessageText, Content: "Hello "},
		{Type: MessageThinking, Content: "let me think"},
		{Type: MessageToolUse, Tool: "Read", CallID: "call-1", Input: map[string]any{"path": "/tmp/foo"}},
		{Type: MessageText, Content: "world"},
	}
	for i, w := range want {
		select {
		case got := <-ch:
			if got.Type != w.Type {
				t.Errorf("msg[%d] type: want %q, got %q", i, w.Type, got.Type)
			}
			if w.Type == MessageText || w.Type == MessageThinking {
				if got.Content != w.Content {
					t.Errorf("msg[%d] content: want %q, got %q", i, w.Content, got.Content)
				}
			}
			if w.Type == MessageToolUse {
				if got.Tool != w.Tool {
					t.Errorf("msg[%d] tool: want %q, got %q", i, w.Tool, got.Tool)
				}
				if got.CallID != w.CallID {
					t.Errorf("msg[%d] call_id: want %q, got %q", i, w.CallID, got.CallID)
				}
				if got.Input["path"] != "/tmp/foo" {
					t.Errorf("msg[%d] input.path: want %q, got %v", i, "/tmp/foo", got.Input["path"])
				}
			}
		default:
			t.Fatalf("msg[%d]: expected message on channel, got none", i)
		}
	}
}

// TestClaudeParseResult feeds a result line with is_error:false + modelUsage
// through claudeResultUsage and asserts the Result.Usage map carries per-
// model token counts. This is the only place the daemon learns about
// billing — a missing or zero field means a free ride for the user.
func TestClaudeParseResult(t *testing.T) {
	t.Parallel()

	// Build a result SDK message as if it came off stdout.
	raw := `{"type":"result","subtype":"success","is_error":false,"session_id":"sess-r","result":"done","modelUsage":{"zhipu/coding-plan":{"inputTokens":123,"outputTokens":45,"cacheReadInputTokens":7,"cacheCreationInputTokens":11,"costUSD":0.01}}}`
	var msg claudeSDKMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	usage := claudeResultUsage(msg, "fallback-model")
	u, ok := usage["zhipu/coding-plan"]
	if !ok {
		t.Fatalf("expected usage for zhipu/coding-plan, got %#v", usage)
	}
	if u.InputTokens != 123 || u.OutputTokens != 45 || u.CacheReadTokens != 7 || u.CacheWriteTokens != 11 {
		t.Fatalf("unexpected usage: %+v", u)
	}

	// Negative case: is_error=true must surface Result.Error from the
	// result_text field. This is the path the daemon maps to status:failed.
	errMsg := `{"type":"result","subtype":"error","is_error":true,"session_id":"sess-e","result":"something went wrong"}`
	var errMsg2 claudeSDKMessage
	if err := json.Unmarshal([]byte(errMsg), &errMsg2); err != nil {
		t.Fatalf("unmarshal error result: %v", err)
	}
	if !errMsg2.IsError {
		t.Fatal("expected is_error=true")
	}
	if errMsg2.ResultText != "something went wrong" {
		t.Fatalf("expected error text in result, got %q", errMsg2.ResultText)
	}
}

// TestClaudeControlRequest calls handleControlRequest with a control_request
// and asserts the response written to stdin is a control_response with
// behavior:allow and run_in_background:false (the daemon auto-approves all
// tool uses in autonomous mode, and refuses to let claude spawn background
// tasks that the daemon cannot supervise).
func TestClaudeControlRequest(t *testing.T) {
	t.Parallel()

	b := &claudeBackend{cfg: Config{Logger: slog.Default()}}

	var written bytes.Buffer

	msg := claudeSDKMessage{
		Type:      "control_request",
		RequestID: "req-42",
		Request: mustMarshalClaude(t, claudeControlRequestPayload{
			Subtype:  "tool_use",
			ToolName: "Bash",
			Input: mustMarshalClaude(t, map[string]any{
				"command":           "sleep 60",
				"run_in_background": true,
			}),
		}),
	}

	b.handleControlRequest(msg, &written)

	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(written.Bytes()), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp["type"] != "control_response" {
		t.Fatalf("expected type control_response, got %v", resp["type"])
	}
	respInner, ok := resp["response"].(map[string]any)
	if !ok {
		t.Fatalf("expected response object, got %T", resp["response"])
	}
	if respInner["request_id"] != "req-42" {
		t.Fatalf("expected request_id req-42, got %v", respInner["request_id"])
	}
	innerResp, ok := respInner["response"].(map[string]any)
	if !ok {
		t.Fatalf("expected inner response object, got %T", respInner["response"])
	}
	if innerResp["behavior"] != "allow" {
		t.Fatalf("expected behavior allow, got %v", innerResp["behavior"])
	}
	updatedInput, ok := innerResp["updatedInput"].(map[string]any)
	if !ok {
		t.Fatalf("expected updatedInput object, got %T", innerResp["updatedInput"])
	}
	if updatedInput["run_in_background"] != false {
		t.Fatalf("expected run_in_background=false, got %v", updatedInput["run_in_background"])
	}
	if updatedInput["command"] != "sleep 60" {
		t.Fatalf("expected original command preserved, got %v", updatedInput["command"])
	}
}

// TestClaudeResumeFallback exercises the resume-decision matrix. When the
// caller requested --resume but claude emitted a different session id AND
// the run failed, resume did not land (claude prints "No conversation
// found" and starts fresh); returning "" lets the daemon trigger its
// retry-with-fresh-session fallback instead of persisting a brand-new id
// as if resume had succeeded.
func TestClaudeResumeFallback(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		requested string
		emitted   string
		failed    bool
		want      string
	}{
		{
			name:      "resume did not land and run failed returns empty for fallback",
			requested: "req-1",
			emitted:   "emit-2",
			failed:    true,
			want:      "",
		},
		{
			name:      "resume succeeded keeps matching id",
			requested: "req-1",
			emitted:   "req-1",
			failed:    false,
			want:      "req-1",
		},
		{
			name:      "no resume requested propagates emitted",
			requested: "",
			emitted:   "fresh-abc",
			failed:    false,
			want:      "fresh-abc",
		},
		{
			name:      "resume did not land but run succeeded keeps fresh id (defensive)",
			requested: "sess-dead",
			emitted:   "fresh-new",
			failed:    false,
			want:      "fresh-new",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := resolveSessionID(tc.requested, tc.emitted, tc.failed)
			if got != tc.want {
				t.Fatalf("resolveSessionID(%q, %q, %v) = %q, want %q",
					tc.requested, tc.emitted, tc.failed, got, tc.want)
			}
		})
	}
}

// TestClaudeMissingExecutable ensures the preflight rejects an Execute call
// when the configured executable cannot be found, before any subprocess is
// spawned. This is the user-facing error path when claude is not installed
// or the configured ExecutablePath is wrong.
func TestClaudeMissingExecutable(t *testing.T) {
	t.Parallel()

	b := &claudeBackend{cfg: Config{
		ExecutablePath: "/nonexistent/claude",
		Logger:         slog.Default(),
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := b.Execute(ctx, "hello", ExecOptions{})
	if sess != nil {
		t.Errorf("expected nil Session on missing executable, got %+v", sess)
	}
	if err == nil {
		t.Fatal("expected error on missing executable, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "executable not found") {
		t.Errorf("error %q: missing %q", msg, "executable not found")
	}
}

// mustMarshalClaude is a tiny json.Marshal helper that fails the test on
// error. Kept local to claude_test.go because no other test file in this
// package needs it.
func mustMarshalClaude(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return data
}
