package api

import (
	"encoding/json"
	"net/http"
	"strings"

	facadepkg "github.com/tgcz2011/aiclibridge/internal/facade"
	"github.com/tgcz2011/aiclibridge/pkg/protocol"
)

// ── Anthropic-compatible Messages API ──
//
// POST /v1/messages mirrors Anthropic's Messages API. Like the OpenAI
// compat layer, one run backs one message (run id == message id). v1
// simplifications:
//   - The system field (string or array of blocks) is collapsed to a single
//     SystemPrompt string passed to the facade.
//   - Only text content blocks are emitted in the stream; thinking and
//     tool_use events are dropped from the Anthropic event stream (the
//     native /v1/runs stream preserves them losslessly). This matches the
//     basic event sequence in the spec.
//   - max_tokens, temperature, top_p are accepted but ignored.

// anthropicRequest is the wire shape for POST /v1/messages. Messages reuse
// the OpenAI message type (role + RawMessage content) since Anthropic and
// OpenAI use the same role/content structure.
type anthropicRequest struct {
	Model       string           `json:"model"`
	Messages    []openaiMessage  `json:"messages"`
	System      json.RawMessage  `json:"system"`
	MaxTokens   int              `json:"max_tokens"`
	Stream      bool             `json:"stream"`
	Temperature float64          `json:"temperature"`
	TopP        float64          `json:"top_p"`
}

// anthropicSystemText extracts the textual system prompt. Anthropic allows
// system as a string or as an array of content blocks; both are collapsed
// to a single string so the facade's SystemPrompt field carries it whole.
func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" || p.Type == "" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// handleAnthropicMessages translates an Anthropic Messages request into a
// facade run. The model is resolved the same way as OpenAI (bare names are
// catalog-looked-up). stream=true emits the Anthropic event sequence;
// stream=false returns a full message object.
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	var req anthropicRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages must not be empty", "invalid_request_error", nil)
		return
	}

	model, err := s.resolveModel(req.Model)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), "model_not_found_error", err)
		return
	}

	system := anthropicSystemText(req.System)
	prompt, sysPrompt := buildOpenAIPrompt(req.Messages, system)
	handle, err := s.fc.StartRun(r.Context(), facadepkg.RunRequest{
		Model:        model,
		Prompt:       prompt,
		SystemPrompt: sysPrompt,
		Stream:       true,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway,
			"facade failed to start run: "+err.Error(), "upstream_error", err)
		return
	}

	if req.Stream {
		s.streamAnthropicMessages(w, r, handle, model)
		return
	}
	events, complete := collectEvents(r, handle)
	if !complete {
		return
	}
	writeJSON(w, http.StatusOK, buildAnthropicMessage(handle, model, events))
}

// handleAnthropicCancel cancels an Anthropic message by id. The id is the
// run id; this delegates to the native cancel logic.
func (s *Server) handleAnthropicCancel(w http.ResponseWriter, r *http.Request) {
	s.handleCancelRun(w, r)
}

// ── Anthropic streaming ──
//
// The Anthropic event stream is a sequence of typed SSE frames:
// message_start → content_block_start → content_block_delta* →
// content_block_stop → message_delta → message_stop. v1 emits a single
// text content block at index 0; non-text events (thinking, tool_use) are
// skipped to keep the stream shape simple and spec-aligned.

// streamAnthropicMessages pipes a run's events as Anthropic message
// events. It emits the fixed start sequence, one content_block_delta per
// text event, and the fixed stop sequence when the run finishes. Client
// disconnect cancels the run.
func (s *Server) streamAnthropicMessages(w http.ResponseWriter, r *http.Request, handle *facadepkg.RunHandle, model string) {
	if !writeSSEHeaders(w) {
		writeError(w, http.StatusInternalServerError, "streaming not supported", "server_error", nil)
		return
	}

	// message_start: the message envelope with empty content and null
	// stop_reason. usage is zeroed; a future revision can populate from
	// the terminal result, but Anthropic clients tolerate zeros.
	msgStart := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":           handle.ID,
			"type":         "message",
			"role":         "assistant",
			"model":        model,
			"content":      []any{},
			"stop_reason":  nil,
			"stop_sequence": nil,
			"usage":        map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}
	if err := writeSSENamedEvent(w, "message_start", mustJSON(msgStart)); err != nil {
		handle.Cancel()
		return
	}

	// content_block_start: open a single text block at index 0.
	blockStart := map[string]any{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]any{"type": "text", "text": ""},
	}
	if err := writeSSENamedEvent(w, "content_block_start", mustJSON(blockStart)); err != nil {
		handle.Cancel()
		return
	}

	gone := r.Context().Done()
	streamDone := false
	for {
		select {
		case ev, ok := <-handle.Events:
			if !ok {
				streamDone = true
				break
			}
			if ev.Type != protocol.EventText {
				// v1 streams only text deltas; thinking/tool_use are
				// preserved in the native /v1/runs stream and the store.
				continue
			}
			delta := map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "text_delta", "text": ev.Content},
			}
			if err := writeSSENamedEvent(w, "content_block_delta", mustJSON(delta)); err != nil {
				handle.Cancel()
				return
			}
		case <-gone:
			handle.Cancel()
			return
		}
		if streamDone {
			break
		}
	}

	// content_block_stop: close the text block.
	_ = writeSSENamedEvent(w, "content_block_stop", mustJSON(map[string]any{
		"type":  "content_block_stop",
		"index":  0,
	}))

	// message_delta: carry the stop_reason. v1 always reports end_turn; a
	// failed/cancelled run still closes the stream with end_turn because
	// Anthropic clients treat any other value as a hard error and the
	// native stream already carried the real status.
	_ = writeSSENamedEvent(w, "message_delta", mustJSON(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 0},
	}))

	// message_stop: terminal sentinel.
	_ = writeSSENamedEvent(w, "message_stop", mustJSON(map[string]any{
		"type": "message_stop",
	}))
}

// ── Anthropic non-streaming ──

// anthropicMessage is the full response for stream=false. content is an
// array of blocks; v1 emits a single text block.
type anthropicMessage struct {
	ID          string                `json:"id"`
	Type        string                `json:"type"`
	Role        string                `json:"role"`
	Model       string                `json:"model"`
	Content     []anthropicContentBlock `json:"content"`
	StopReason  string                `json:"stop_reason"`
	StopSequence *string              `json:"stop_sequence"`
	Usage       anthropicUsage        `json:"usage"`
}

// anthropicContentBlock is one element of content. v1 emits type "text"
// only; tool_use blocks are out of scope.
type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// anthropicUsage is the token accounting block. input_tokens and
// output_tokens are summed across models when the adapter reported usage.
type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// buildAnthropicMessage assembles the non-streaming message from the
// collected event timeline. The text content is the terminal
// Result.Output when present, otherwise the concatenation of every text
// event. stop_reason is always end_turn for v1 (matching the stream).
func buildAnthropicMessage(handle *facadepkg.RunHandle, model string, events []protocol.Event) anthropicMessage {
	content := ""
	var usage anthropicUsage
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == protocol.EventResult && events[i].Result != nil {
			r := events[i].Result
			if r.Output != "" {
				content = r.Output
			}
			if r.Usage != nil {
				var in, out int
				for _, u := range r.Usage {
					in += u.InputTokens
					out += u.OutputTokens
				}
				usage = anthropicUsage{InputTokens: in, OutputTokens: out}
			}
			break
		}
	}
	if content == "" {
		content = aggregateText(events)
	}
	return anthropicMessage{
		ID:    handle.ID,
		Type:  "message",
		Role:  "assistant",
		Model: model,
		Content: []anthropicContentBlock{{
			Type: "text",
			Text: content,
		}},
		StopReason: "end_turn",
		Usage:      usage,
	}
}
