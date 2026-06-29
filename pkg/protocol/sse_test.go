package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestEventRoundTrip is the lossless guarantee test for the SSE event schema.
//
// multica's TaskMessagePayload (multica/server/pkg/protocol/messages.go:55-65)
// is LOSSY — it has no field for CallID, thinking, status, log, level, or
// session_id. This test asserts that aiclibridge's schema is LOSSLESS: every
// field on every event type round-trips byte-identical through Marshal →
// WriteSSEEvent → ParseSSEEvent. The `thinking` case is the Metis blocker —
// the old schema would have dropped it.
func TestEventRoundTrip(t *testing.T) {
	// Fixture: the `input` field on a tool_use. json.RawMessage round-trips
	// any JSON shape the CLI emits (object, array, scalar).
	toolInput := json.RawMessage(`{"path":"/tmp/x","args":["-l","-a"]}`)
	resultUsage := map[string]TokenUsagePayload{
		"claude-sonnet-4": {
			InputTokens:     1234,
			OutputTokens:    567,
			CacheReadTokens: 89,
		},
		"claude-haiku": {
			InputTokens:     11,
			OutputTokens:    22,
			CacheWriteTokens: 33,
		},
	}

	cases := []struct {
		name  string
		event Event
	}{
		{
			name: "text",
			event: Event{
				Type:      EventText,
				Seq:       0,
				Content:   "Hello, world.",
				SessionID: "sess-1",
			},
		},
		{
			// THIS CASE IS THE METIS BLOCKER.
			// TaskMessagePayload has no Type="thinking" path and no field
			// for thinking content. The old schema dropped it; this one
			// preserves it.
			name: "thinking",
			event: Event{
				Type:      EventThinking,
				Seq:       1,
				Content:   "Let me read the file first...",
				SessionID: "sess-1",
			},
		},
		{
			name: "tool_use",
			event: Event{
				Type:      EventToolUse,
				Seq:       2,
				Tool:      "Bash",
				CallID:    "call_abc123",
				Input:     toolInput,
				SessionID: "sess-1",
			},
		},
		{
			name: "tool_result",
			event: Event{
				Type:      EventToolResult,
				Seq:       3,
				Tool:      "Bash",
				CallID:    "call_abc123",
				Output:    "total 12\ndrwxr-xr-x  3 u u 4096 Jun 29 17:30 .\n",
				SessionID: "sess-1",
			},
		},
		{
			// Another Metis case: TaskMessagePayload has no `status` field.
			// aiclibridge preserves it for client-side progress UI.
			name: "status",
			event: Event{
				Type:      EventStatus,
				Seq:       4,
				Status:    "compacting",
				SessionID: "sess-1",
			},
		},
		{
			name: "error",
			event: Event{
				Type:      EventError,
				Seq:       5,
				Content:   "rate limit exceeded; retrying in 30s",
				SessionID: "sess-1",
			},
		},
		{
			// Another Metis case: TaskMessagePayload has no `log` type and
			// no `level` field. aiclibridge preserves both for the log
			// panel in clients.
			name: "log",
			event: Event{
				Type:      EventLog,
				Seq:       6,
				Content:   "stderr: npm warn deprecated foo@1.0.0",
				Level:     "warn",
				SessionID: "sess-1",
			},
		},
		{
			// Terminal result event — uses the embedded ResultPayload.
			// TaskMessagePayload has no terminal result shape at all; it
			// uses a separate TaskCompletedPayload with just PRURL/Output.
			name: "result",
			event: Event{
				Type:      EventResult,
				Seq:       7,
				SessionID: "sess-1",
				Result: &ResultPayload{
					Status:     "completed",
					Output:     "Final answer: 42.",
					Error:      "",
					DurationMs: 12345,
					SessionID:  "sess-1",
					Usage:      resultUsage,
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteSSEEvent(&buf, tc.event); err != nil {
				t.Fatalf("WriteSSEEvent: %v", err)
			}

			// SSE framing must be spec-compliant: `event: <type>\ndata: <json>\n\n`
			line := buf.String()
			if !strings.HasPrefix(line, "event: "+string(tc.event.Type)+"\n") {
				t.Fatalf("SSE frame missing event header, got: %q", line)
			}
			if !strings.HasSuffix(line, "\n\n") {
				t.Fatalf("SSE frame missing double-newline terminator, got: %q", line)
			}
			if !strings.Contains(line, "data: ") {
				t.Fatalf("SSE frame missing data line, got: %q", line)
			}

			// Extract the data payload.
			var dataLine string
			for _, l := range strings.Split(strings.TrimRight(line, "\n"), "\n") {
				if strings.HasPrefix(l, "data: ") {
					dataLine = strings.TrimPrefix(l, "data: ")
					break
				}
			}
			if dataLine == "" {
				t.Fatalf("SSE frame had no data line, got: %q", line)
			}

			parsed, err := ParseSSEEvent(string(tc.event.Type), []byte(dataLine))
			if err != nil {
				t.Fatalf("ParseSSEEvent: %v", err)
			}

			// Field-by-field assertions. We do NOT rely on reflect.DeepEqual
			// on the whole struct because the SSE wire shape drops
			// field-type erasure for empty fields. We assert EVERY populated
			// field survived the round-trip with its original value.
			if parsed.Type != tc.event.Type {
				t.Errorf("Type: got %q, want %q", parsed.Type, tc.event.Type)
			}
			if parsed.Seq != tc.event.Seq {
				t.Errorf("Seq: got %d, want %d", parsed.Seq, tc.event.Seq)
			}
			if parsed.Content != tc.event.Content {
				t.Errorf("Content: got %q, want %q", parsed.Content, tc.event.Content)
			}
			if parsed.Tool != tc.event.Tool {
				t.Errorf("Tool: got %q, want %q", parsed.Tool, tc.event.Tool)
			}
			if parsed.CallID != tc.event.CallID {
				t.Errorf("CallID: got %q, want %q", parsed.CallID, tc.event.CallID)
			}
			if !bytes.Equal(parsed.Input, tc.event.Input) {
				t.Errorf("Input: got %s, want %s", string(parsed.Input), string(tc.event.Input))
			}
			if parsed.Output != tc.event.Output {
				t.Errorf("Output: got %q, want %q", parsed.Output, tc.event.Output)
			}
			if parsed.Status != tc.event.Status {
				t.Errorf("Status: got %q, want %q", parsed.Status, tc.event.Status)
			}
			if parsed.Level != tc.event.Level {
				t.Errorf("Level: got %q, want %q", parsed.Level, tc.event.Level)
			}
			if parsed.SessionID != tc.event.SessionID {
				t.Errorf("SessionID: got %q, want %q", parsed.SessionID, tc.event.SessionID)
			}

			// Result sub-payload — only for the result event.
			if tc.event.Result != nil {
				if parsed.Result == nil {
					t.Fatalf("Result: got nil, want %+v", tc.event.Result)
				}
				if parsed.Result.Status != tc.event.Result.Status {
					t.Errorf("Result.Status: got %q, want %q", parsed.Result.Status, tc.event.Result.Status)
				}
				if parsed.Result.Output != tc.event.Result.Output {
					t.Errorf("Result.Output: got %q, want %q", parsed.Result.Output, tc.event.Result.Output)
				}
				if parsed.Result.Error != tc.event.Result.Error {
					t.Errorf("Result.Error: got %q, want %q", parsed.Result.Error, tc.event.Result.Error)
				}
				if parsed.Result.DurationMs != tc.event.Result.DurationMs {
					t.Errorf("Result.DurationMs: got %d, want %d", parsed.Result.DurationMs, tc.event.Result.DurationMs)
				}
				if parsed.Result.SessionID != tc.event.Result.SessionID {
					t.Errorf("Result.SessionID: got %q, want %q", parsed.Result.SessionID, tc.event.Result.SessionID)
				}
				if len(parsed.Result.Usage) != len(tc.event.Result.Usage) {
					t.Fatalf("Result.Usage length: got %d, want %d", len(parsed.Result.Usage), len(tc.event.Result.Usage))
				}
				for model, want := range tc.event.Result.Usage {
					got, ok := parsed.Result.Usage[model]
					if !ok {
						t.Errorf("Result.Usage missing model %q", model)
						continue
					}
					if got != want {
						t.Errorf("Result.Usage[%q]: got %+v, want %+v", model, got, want)
					}
				}
			}

			// Re-marshal the parsed event and assert the JSON is byte-equal
			// (modulo field-order, which encoding/json doesn't guarantee).
			// The wire data IS the canonical form, so we compare against it.
			var reCanonical map[string]any
			if err := json.Unmarshal([]byte(dataLine), &reCanonical); err != nil {
				t.Fatalf("re-unmarshal canonical: %v", err)
			}
			var reParsed map[string]any
			parsedJSON, _ := json.Marshal(parsed)
			if err := json.Unmarshal(parsedJSON, &reParsed); err != nil {
				t.Fatalf("re-unmarshal parsed: %v", err)
			}
			for k, v := range reCanonical {
				if got, ok := reParsed[k]; !ok {
					// `seq:0` is dropped by omitempty — that's fine, not a loss.
					if k == "seq" {
						continue
					}
					t.Errorf("round-trip lost field %q (canonical had %v)", k, v)
				} else {
					// Compare as JSON for nested structures.
					gj, _ := json.Marshal(got)
					vj, _ := json.Marshal(v)
					if !bytes.Equal(gj, vj) {
						t.Errorf("field %q differs: got %s, want %s", k, gj, vj)
					}
				}
			}
		})
	}
}

// TestWriteSSEEvent_Framing asserts the exact byte layout: `event: <type>\ndata: <json>\n\n`.
// SSE spec: https://html.spec.whatwg.org/multipage/server-sent-events.html
func TestWriteSSEEvent_Framing(t *testing.T) {
	e := Event{Type: EventText, Seq: 7, Content: "hi"}
	var buf bytes.Buffer
	if err := WriteSSEEvent(&buf, e); err != nil {
		t.Fatalf("WriteSSEEvent: %v", err)
	}
	got := buf.String()
	want := "event: text\ndata: {\"type\":\"text\",\"seq\":7,\"content\":\"hi\"}\n\n"
	if got != want {
		t.Errorf("SSE framing:\n got  %q\n want %q", got, want)
	}
}

// TestParseSSEEvent_BadJSON ensures ParseSSEEvent returns a non-nil error for
// malformed input — callers (the SSE handler) rely on this to surface a 400.
func TestParseSSEEvent_BadJSON(t *testing.T) {
	_, err := ParseSSEEvent(string(EventText), []byte(`{not json`))
	if err == nil {
		t.Fatal("ParseSSEEvent accepted malformed JSON")
	}
}
