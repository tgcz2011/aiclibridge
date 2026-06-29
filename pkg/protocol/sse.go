// Package protocol hosts the wire types and constants shared between
// aiclibridge and its clients/adapters. Anything in pkg/ is importable
// by external consumers; keep it small and stable.
//
// sse.go defines the lossless SSE event schema for aiclibridge's
// /v1/runs endpoint. The schema deliberately preserves fields that
// multica's TaskMessagePayload (multica/server/pkg/protocol/messages.go:55-65)
// drops — CallID, thinking content, status, log level, session_id —
// so clients can reconstruct the full run timeline for replay/resume.
package protocol

import (
	"encoding/json"
	"fmt"
	"io"
)

// EventType is the discriminator on a single SSE event frame. Eight
// values: the seven Message types from internal/adapter plus the
// terminal result event that signals run completion.
type EventType string

const (
	EventText       EventType = "text"
	EventThinking   EventType = "thinking"
	EventToolUse    EventType = "tool_use"
	EventToolResult EventType = "tool_result"
	EventStatus     EventType = "status"
	EventError      EventType = "error"
	EventLog        EventType = "log"
	EventResult     EventType = "result"
)

// TokenUsagePayload is the per-model token accounting for a run. Cache
// hit/miss split is preserved so clients can surface cache savings.
type TokenUsagePayload struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	CacheReadTokens int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// ResultPayload is the terminal event for a run. It is embedded in an
// Event with Type==EventResult.
type ResultPayload struct {
	Status     string                     `json:"status"` // "completed" | "failed" | "cancelled" | "timeout"
	Output     string                     `json:"output,omitempty"`
	Error      string                     `json:"error,omitempty"`
	DurationMs int64                      `json:"duration_ms"`
	SessionID  string                     `json:"session_id,omitempty"`
	Usage      map[string]TokenUsagePayload `json:"usage,omitempty"`
}

// Event is the single SSE frame. Every field is preserved end-to-end —
// the schema is deliberately fat so client replay matches the original
// adapter output exactly. zero values are omitted from the wire form
// so the SSE stream is compact for the common case (a text frame is
// just `{"type":"text","seq":N,"content":"..."}`).
type Event struct {
	Type      EventType       `json:"type"`
	Seq       int             `json:"seq,omitempty"`
	Content   string          `json:"content,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Output    string          `json:"output,omitempty"`
	Status    string          `json:"status,omitempty"`
	Level     string          `json:"level,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Result    *ResultPayload  `json:"result,omitempty"`
}

// WriteSSEEvent writes a single spec-compliant SSE frame to w:
// `event: <type>\ndata: <json>\n\n`.
// See https://html.spec.whatwg.org/multipage/server-sent-events.html.
//
// The JSON is compact (no newlines) so a client can split on \n\n
// and re-parse the data line verbatim. Use a *bufio.Writer on w if
// you want WriteSSEEvent to flush — this function does not Flush.
func WriteSSEEvent(w io.Writer, e Event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal SSE event: %w", err)
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, payload); err != nil {
		return fmt.Errorf("write SSE frame: %w", err)
	}
	return nil
}

// ParseSSEEvent is the inverse of WriteSSEEvent: it takes the `event:`
// header and the `data:` payload (with the `data: ` prefix already
// stripped) and reconstructs the Event. The eventType argument must
// match what was in the `event:` header — it is NOT validated against
// e.Type to allow callers to detect drift between header and body.
func ParseSSEEvent(eventType string, data []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(data, &e); err != nil {
		return Event{}, fmt.Errorf("unmarshal SSE data: %w", err)
	}
	// Force the type from the header so a malformed body that omits
	// `type` still classifies correctly. This matches the SSE spec
	// where the `event:` field is the canonical type signal.
	if e.Type == "" {
		e.Type = EventType(eventType)
	}
	return e, nil
}
