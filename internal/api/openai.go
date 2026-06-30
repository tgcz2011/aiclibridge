package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	facadepkg "github.com/tgcz2011/aiclibridge/internal/facade"
	"github.com/tgcz2011/aiclibridge/pkg/protocol"
)

// ── OpenAI-compatible Chat Completions ──
//
// This file implements POST /v1/chat/completions and its cancel route. The
// bridge reuses a single underlying run per chat completion (the run id IS
// the chat completion id), so the OpenAI cancel route delegates to the
// facade's CancelRun with the same id. v1 simplifications:
//   - Only role user/assistant/system messages are honoured; tool/function
//     messages are ignored.
//   - Content may be a string or an array of {type,text} parts; non-text
//     parts (images) are dropped from the prompt.
//   - temperature, top_p, max_tokens, user are accepted but ignored — the
//     adapters do not expose sampling controls through the facade.
//   - Tool calls are emitted in the OpenRouter-ish delta.tool_calls shape
//     but without streaming argument deltas (the whole input is sent once).

// openaiChatRequest is the wire shape for POST /v1/chat/completions. It
// accepts the standard OpenAI fields; only model/messages/stream affect
// behaviour, the rest are tolerated and ignored.
type openaiChatRequest struct {
	Model       string           `json:"model"`
	Messages    []openaiMessage  `json:"messages"`
	Stream      bool             `json:"stream"`
	MaxTokens   int              `json:"max_tokens"`
	Temperature float64          `json:"temperature"`
	TopP        float64          `json:"top_p"`
	User        string           `json:"user"`
}

// openaiMessage is one element of the messages array. Content is kept as
// RawMessage so both the string form ("hello") and the multimodal array
// form ([{"type":"text","text":"hello"}]) can be decoded; text() extracts
// the concatenated text from either.
type openaiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// text extracts the textual content of a message. A string content returns
// itself; an array of parts returns the concatenation of every text part.
// Non-text parts (image_url, etc.) are dropped — v1 has no path to forward
// images to the CLI adapters. An empty/absent content yields "".
func (m openaiMessage) text() string {
	if len(m.Content) == 0 {
		return ""
	}
	// String form: "hello".
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	// Array form: [{"type":"text","text":"..."},...].
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
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

// buildOpenAIPrompt collapses an OpenAI messages array into the (prompt,
// systemPrompt) pair the facade expects. The last user message becomes the
// prompt; every prior system/user/assistant message is folded into the
// system prompt as conversation context so the adapter sees the full
// history. explicitSystem (from Anthropic's top-level system field) is
// prepended when non-empty — for OpenAI it is always "".
func buildOpenAIPrompt(messages []openaiMessage, explicitSystem string) (prompt, system string) {
	var sysParts []string
	if explicitSystem != "" {
		sysParts = append(sysParts, explicitSystem)
	}
	lastUser := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			lastUser = i
			break
		}
	}
	for i, m := range messages {
		switch m.Role {
		case "system":
			if i != lastUser {
				sysParts = append(sysParts, m.text())
			}
		case "user":
			if i == lastUser {
				prompt = m.text()
			} else {
				sysParts = append(sysParts, "User: "+m.text())
			}
		case "assistant":
			sysParts = append(sysParts, "Assistant: "+m.text())
		}
	}
	system = strings.Join(sysParts, "\n\n")
	return prompt, system
}

// handleOpenAIChat translates an OpenAI Chat Completions request into a
// facade run. The model field is resolved to CLI/provider/model form (bare
// names are looked up in the catalog). stream=true pipes the run as OpenAI
// chunks; stream=false collects events and returns a full ChatCompletion.
func (s *Server) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	var req openaiChatRequest
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

	prompt, system := buildOpenAIPrompt(req.Messages, "")
	handle, err := s.fc.StartRun(r.Context(), facadepkg.RunRequest{
		Model:        model,
		Prompt:       prompt,
		SystemPrompt: system,
		Stream:       true,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway,
			"facade failed to start run: "+err.Error(), "upstream_error", err)
		return
	}

	if req.Stream {
		s.streamOpenAIChat(w, r, handle, model)
		return
	}
	events, complete := collectEvents(r, handle)
	if !complete {
		return
	}
	writeJSON(w, http.StatusOK, buildOpenAICompletion(handle, model, events))
}

// handleOpenAIChatCancel cancels a chat completion by id. The id is the same
// as the run id (one run per completion), so this delegates to the native
// cancel logic.
func (s *Server) handleOpenAIChatCancel(w http.ResponseWriter, r *http.Request) {
	s.handleCancelRun(w, r)
}

// ── OpenAI streaming ──

// openaiChunk is one Server-Sent-Event frame in the OpenAI streaming
// protocol. finish_reason is a pointer so non-terminal chunks serialise as
// `null` (matching OpenAI) and the terminal chunk serialises as `"stop"`.
type openaiChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
}

// openaiChoice is one element of choices in a streaming chunk. Delta carries
// the incremental content; FinishReason is null until the terminal frame.
type openaiChoice struct {
	Index        int         `json:"index"`
	Delta        openaiDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

// openaiDelta is the incremental payload. Role is set on the first chunk;
// Content carries text; ReasoningContent carries thinking (OpenRouter
// convention); ToolCalls carries tool-use invocations.
type openaiDelta struct {
	Role             string           `json:"role,omitempty"`
	Content          string           `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []openaiToolCall `json:"tool_calls,omitempty"`
}

// openaiToolCall is a simplified OpenAI tool_call delta. The whole input is
// sent in one shot (no streaming argument deltas) — v1 does not surface
// per-token tool arguments.
type openaiToolCall struct {
	Index    int                `json:"index"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function openaiToolFunction `json:"function,omitempty"`
}

// openaiToolFunction carries the tool name and its JSON arguments.
type openaiToolFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// streamOpenAIChat pipes a run's events to the client as OpenAI chat
// completion chunks. It emits a leading role-only chunk, one chunk per
// text/thinking/tool_use event, a terminal chunk with finish_reason "stop",
// and the final `data: [DONE]` sentinel. Client disconnect cancels the run.
func (s *Server) streamOpenAIChat(w http.ResponseWriter, r *http.Request, handle *facadepkg.RunHandle, model string) {
	if !writeSSEHeaders(w) {
		writeError(w, http.StatusInternalServerError, "streaming not supported", "server_error", nil)
		return
	}
	now := time.Now().Unix()
	// Leading chunk: role only, no content. OpenAI clients use this to
	// know the assistant turn has begun.
	lead := openaiChunk{
		ID: handle.ID, Object: "chat.completion.chunk", Created: now, Model: model,
		Choices: []openaiChoice{{Index: 0, Delta: openaiDelta{Role: "assistant"}}},
	}
	if err := writeSSEData(w, mustJSON(lead)); err != nil {
		handle.Cancel()
		return
	}

	gone := r.Context().Done()
	for {
		select {
		case ev, ok := <-handle.Events:
			if !ok {
				term := openaiChunk{
					ID: handle.ID, Object: "chat.completion.chunk", Created: now, Model: model,
					Choices: []openaiChoice{{Index: 0, Delta: openaiDelta{}, FinishReason: ptrString("stop")}},
				}
				_ = writeSSEData(w, mustJSON(term))
				_ = writeSSEData(w, "[DONE]")
				return
			}
			chunk := openAIChunkFromEvent(handle.ID, model, now, ev)
			if chunk == nil {
				continue
			}
			if err := writeSSEData(w, mustJSON(chunk)); err != nil {
				handle.Cancel()
				return
			}
		case <-gone:
			handle.Cancel()
			return
		}
	}
}

// openAIChunkFromEvent converts a single protocol.Event to an OpenAI chunk.
// Returns nil for events with no OpenAI representation (status, log, error,
// tool_result) — they are silently dropped from the stream. The terminal
// result event is NOT handled here; the caller emits it after the channel
// closes so finish_reason is set exactly once.
func openAIChunkFromEvent(id, model string, created int64, ev protocol.Event) *openaiChunk {
	c := &openaiChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []openaiChoice{{Index: 0}},
	}
	switch ev.Type {
	case protocol.EventText:
		c.Choices[0].Delta.Content = ev.Content
	case protocol.EventThinking:
		c.Choices[0].Delta.ReasoningContent = ev.Content
	case protocol.EventToolUse:
		c.Choices[0].Delta.ToolCalls = []openaiToolCall{{
			Index: 0, ID: ev.CallID, Type: "function",
			Function: openaiToolFunction{
				Name:      ev.Tool,
				Arguments: string(ev.Input),
			},
		}}
	default:
		return nil
	}
	return c
}

// ── OpenAI non-streaming ──

// openaiChatCompletion is the full response for stream=false. Choices
// carry a Message (not Delta) and a string finish_reason.
type openaiChatCompletion struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []openaiChoiceMessage  `json:"choices"`
	Usage   openaiUsage            `json:"usage"`
}

// openaiChoiceMessage is one element of choices in a non-streaming response.
type openaiChoiceMessage struct {
	Index        int                    `json:"index"`
	Message      openaiResponseMessage  `json:"message"`
	FinishReason string                 `json:"finish_reason"`
}

// openaiResponseMessage is the assistant message in a non-streaming
// response. Content is always a string (multimodal outputs are out of
// scope for v1).
type openaiResponseMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openaiUsage is the token accounting block. v1 reports 0 when the adapter
// did not emit usage; a future revision can sum per-model usage from the
// terminal result.
type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// buildOpenAICompletion assembles the non-streaming ChatCompletion from the
// collected event timeline. The assistant content is the terminal
// Result.Output when present, otherwise the concatenation of every text
// event (so a run without a terminal result still yields its text). Usage
// is summed across models when the adapter reported any.
func buildOpenAICompletion(handle *facadepkg.RunHandle, model string, events []protocol.Event) openaiChatCompletion {
	content := ""
	finish := "stop"
	var usage openaiUsage
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == protocol.EventResult && events[i].Result != nil {
			r := events[i].Result
			if r.Output != "" {
				content = r.Output
			}
			if r.Status == "failed" || r.Status == "cancelled" || r.Status == "timeout" {
				finish = r.Status
			}
			usage = openAIUsageFromResult(r)
			break
		}
	}
	if content == "" {
		content = aggregateText(events)
	}
	return openaiChatCompletion{
		ID:      handle.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openaiChoiceMessage{{
			Index:        0,
			Message:      openaiResponseMessage{Role: "assistant", Content: content},
			FinishReason: finish,
		}},
		Usage: usage,
	}
}

// openAIUsageFromResult sums per-model token usage into the OpenAI
// prompt/completion/total triple. Returns zeros when the result has no
// usage map (the common v1 case until adapters report usage).
func openAIUsageFromResult(r *protocol.ResultPayload) openaiUsage {
	if r == nil || r.Usage == nil {
		return openaiUsage{}
	}
	var in, out int
	for _, u := range r.Usage {
		in += u.InputTokens
		out += u.OutputTokens
	}
	return openaiUsage{PromptTokens: in, CompletionTokens: out, TotalTokens: in + out}
}

// ── Shared helpers ──

// aggregateText concatenates every text event's content in order. Used as
// the fallback content when a run has no terminal Result.Output.
func aggregateText(events []protocol.Event) string {
	var sb strings.Builder
	for _, ev := range events {
		if ev.Type == protocol.EventText {
			sb.WriteString(ev.Content)
		}
	}
	return sb.String()
}

// mustJSON marshals v and panics on failure. Used only for in-memory shapes
// that are guaranteed marshalable (no channels, no funcs); a panic here is
// caught by the recover middleware and turned into a 500.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("api: marshal chunk: " + err.Error())
	}
	return string(b)
}

// ptrString returns a pointer to s, used to set *string FinishReason fields
// that must serialise as `null` when unset.
func ptrString(s string) *string { return &s }
