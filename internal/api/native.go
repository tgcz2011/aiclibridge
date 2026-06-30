package api

import (
	"net/http"

	"github.com/tgcz2011/aiclibridge/internal/detect"
	facadepkg "github.com/tgcz2011/aiclibridge/internal/facade"
	"github.com/tgcz2011/aiclibridge/pkg/protocol"
)

// ── Native AICLIBridge API handlers ──
//
// These handlers implement the bridge's own surface (not the OpenAI or
// Anthropic compat layers). They are the canonical form: the compat layers
// translate to/from these shapes. Every handler defers nothing special —
// the recover middleware at the chain boundary converts any panic into a
// 500, so handlers can stay linear.

// handleHealthz is the liveness probe. It is unauthenticated so an external
// orchestrator (k8s, systemd, etc.) can check the daemon without holding a
// key. A 200 with {"status":"ok"} means the process is up and the mux is
// serving; it does NOT imply downstream CLIs are installed.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// nativeRunRequest is the wire shape for POST /v1/runs. It maps 1:1 onto
// facade.RunRequest. The JSON tags use snake_case to match the documented
// native API; the facade type uses Go conventions.
type nativeRunRequest struct {
	Model           string            `json:"model"`
	Prompt          string            `json:"prompt"`
	Cwd             string            `json:"cwd"`
	SystemPrompt    string            `json:"system_prompt"`
	ResumeSessionID string            `json:"resume_session_id"`
	MaxTurns        int               `json:"max_turns"`
	TimeoutMs       int64             `json:"timeout_ms"`
	CustomArgs      []string          `json:"custom_args"`
	CustomEnv       map[string]string `json:"custom_env"`
	Stream          bool              `json:"stream"`
}

// handleCreateRun starts a run via the facade. For stream=true it pipes the
// facade event channel to the client as native SSE (protocol.WriteSSEEvent).
// For stream=false it drains the channel, assembles a RunResult from the
// terminal event, and returns it as JSON. Either way the run is started with
// Stream=true internally so this handler is the sole consumer of the live
// channel — the facade's internal drain is never engaged.
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	var req nativeRunRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	handle, err := s.fc.StartRun(r.Context(), facadepkg.RunRequest{
		Model:           req.Model,
		Prompt:          req.Prompt,
		Cwd:             req.Cwd,
		SystemPrompt:    req.SystemPrompt,
		ResumeSessionID: req.ResumeSessionID,
		MaxTurns:        req.MaxTurns,
		TimeoutMs:       req.TimeoutMs,
		CustomArgs:      req.CustomArgs,
		CustomEnv:       req.CustomEnv,
		Stream:          true,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway,
			"facade failed to start run: "+err.Error(), "upstream_error", err)
		return
	}

	if req.Stream {
		if !writeSSEHeaders(w) {
			writeError(w, http.StatusInternalServerError,
				"streaming not supported", "server_error", nil)
			return
		}
		streamNativeEvents(w, r, handle)
		return
	}

	// Non-streaming: collect the full event timeline, then assemble the
	// result. If the client disconnects mid-run, cancel and return silently
	// (no response to write).
	events, complete := collectEvents(r, handle)
	if !complete {
		return
	}
	writeJSON(w, http.StatusOK, buildRunResult(handle, events))
}

// handleGetRun replays a run's stored timeline. It delegates to the facade's
// GetRun, which reads from the store (the source of truth for replay). A
// run that does not exist yields a 404. The response is the full RunResult
// including the Events slice, so a client can reconstruct the exact timeline.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing run id", "invalid_request_error", nil)
		return
	}
	result, err := s.fc.GetRun(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound,
			"run not found: "+err.Error(), "not_found_error", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleCancelRun cancels a live run by id. The facade returns an error if
// the run is not in the live map (already finished), which we surface as 404
// — the cancel endpoint is idempotent only for live runs; cancelling an
// already-done run is a client mistake worth signalling.
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing run id", "invalid_request_error", nil)
		return
	}
	if err := s.fc.CancelRun(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound,
			"run not found: "+err.Error(), "not_found_error", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": true, "id": id})
}

// handleListAgents returns the full CLI catalog. Each entry includes the
// CLI's providers and models, so a client can pick a routing key
// (CLI/provider/model) for POST /v1/runs.
func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.fc.ListAgents(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway,
			"list agents failed: "+err.Error(), "upstream_error", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

// handleListAgent returns a single CLI's catalog entry. It reuses ListAgents
// and filters by name; the facade does not expose a per-CLI catalog lookup
// directly, so this is the simplest correct path. An unknown CLI yields 404.
func (s *Server) handleListAgent(w http.ResponseWriter, r *http.Request) {
	cli := r.PathValue("cli")
	if cli == "" {
		writeError(w, http.StatusBadRequest, "missing cli", "invalid_request_error", nil)
		return
	}
	agents, err := s.fc.ListAgents(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway,
			"list agents failed: "+err.Error(), "upstream_error", err)
		return
	}
	for _, a := range agents {
		if a.Name == cli {
			writeJSON(w, http.StatusOK, a)
			return
		}
	}
	writeError(w, http.StatusNotFound, "cli not found: "+cli, "not_found_error", nil)
}

// handleListProviders returns the deduplicated set of provider names across
// every CLI in the catalog. It is a convenience summary; clients needing the
// full provider/model tree should use /v1/agents.
func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	agents, err := s.fc.ListAgents(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway,
			"list agents failed: "+err.Error(), "upstream_error", err)
		return
	}
	seen := make(map[string]struct{})
	var providers []string
	for _, cli := range agents {
		for _, p := range cli.Providers {
			if _, ok := seen[p.Name]; ok {
				continue
			}
			seen[p.Name] = struct{}{}
			providers = append(providers, p.Name)
		}
	}
	if providers == nil {
		providers = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
}

// handleListModels renders the catalog in the OpenAI /v1/models shape. It is
// mounted at both the OpenAI compat path (GET /v1/models, unauthenticated)
// and reused by the Anthropic compat path (GET /v1/anthropic/models). Each
// (cli, provider, model) triple becomes one entry with id
// "cli/provider/model"; owned_by is the provider name so clients can group
// by upstream.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	agents, err := s.fc.ListAgents(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway,
			"list agents failed: "+err.Error(), "upstream_error", err)
		return
	}
	writeJSON(w, http.StatusOK, openAIModelsEnvelope(agents))
}

// handleAnthropicModels renders the same catalog in Anthropic's model-list
// shape. Anthropic's API returns {"data":[{"id":...,"display_name":...}]}
// rather than OpenAI's {object:"list",data:[...]}, so the wrapper differs
// even though the underlying catalog walk is identical.
func (s *Server) handleAnthropicModels(w http.ResponseWriter, r *http.Request) {
	agents, err := s.fc.ListAgents(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway,
			"list agents failed: "+err.Error(), "upstream_error", err)
		return
	}
	writeJSON(w, http.StatusOK, anthropicModelsEnvelope(agents))
}

// ── Native helpers ──

// collectEvents drains the run's live event channel until it closes,
// returning the full timeline. It watches r.Context().Done() so a client
// disconnect cancels the run rather than wedging the forwarder; in that
// case the second return is false and the caller should write no response.
// This is the non-streaming counterpart to streamNativeEvents.
func collectEvents(r *http.Request, handle *facadepkg.RunHandle) ([]protocol.Event, bool) {
	gone := r.Context().Done()
	var events []protocol.Event
	for {
		select {
		case ev, ok := <-handle.Events:
			if !ok {
				return events, true
			}
			events = append(events, ev)
		case <-gone:
			handle.Cancel()
			return events, false
		}
	}
}

// buildRunResult assembles a facade.RunResult from a closed event timeline.
// It mirrors the assembly the facade itself does in GetRun, but operates on
// the in-memory event slice rather than the store — this is the path used
// by POST /v1/runs with stream=false, where the handler drained the channel
// directly and there is no need for a store round-trip. The terminal
// EventResult (last in the timeline) supplies status/output/usage; absent a
// terminal event the run is reported as completed with whatever text was
// accumulated, which is the safest default for a truncated timeline.
func buildRunResult(handle *facadepkg.RunHandle, events []protocol.Event) *facadepkg.RunResult {
	result := &facadepkg.RunResult{
		ID:     handle.ID,
		Status: "completed",
		Events: events,
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == protocol.EventResult && events[i].Result != nil {
			r := events[i].Result
			result.Status = r.Status
			result.Output = r.Output
			result.Error = r.Error
			result.DurationMs = r.DurationMs
			result.SessionID = r.SessionID
			result.Usage = r.Usage
			break
		}
	}
	return result
}

// openAIModelsEntry is one element of the OpenAI /v1/models list. The
// created field is a fixed startup-ish epoch; OpenAI clients treat it as
// informational only, and using time.Now would make the response
// non-cacheable, so we pin it to a constant.
type openAIModelsEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// openAIModelsEnvelope walks the catalog and emits the OpenAI list shape.
// The created timestamp is pinned to 1 (a stable, cache-friendly value);
// clients do not rely on it for ordering.
func openAIModelsEnvelope(agents []detect.CLIInfo) map[string]any {
	data := make([]openAIModelsEntry, 0)
	for _, cli := range agents {
		for _, p := range cli.Providers {
			for _, m := range p.Models {
				data = append(data, openAIModelsEntry{
					ID:      detect.ModelName(cli.Name, p.Name, m.Name),
					Object:  "model",
					Created: 1,
					OwnedBy: p.Name,
				})
			}
		}
	}
	return map[string]any{
		"object": "list",
		"data":   data,
	}
}

// anthropicModelsEntry is one element of the Anthropic model list. Anthropic
// uses display_name rather than owned_by; we fall back to the model id when
// the catalog has no DisplayName.
type anthropicModelsEntry struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Type        string `json:"type"`
}

// anthropicModelsEnvelope walks the catalog and emits the Anthropic list
// shape. The top-level wrapper is {"data":[...]} to match Anthropic's
// /v1/models response.
func anthropicModelsEnvelope(agents []detect.CLIInfo) map[string]any {
	data := make([]anthropicModelsEntry, 0)
	for _, cli := range agents {
		for _, p := range cli.Providers {
			for _, m := range p.Models {
				name := m.DisplayName
				if name == "" {
					name = m.Name
				}
				data = append(data, anthropicModelsEntry{
					ID:          detect.ModelName(cli.Name, p.Name, m.Name),
					DisplayName: name,
					Type:        "model",
				})
			}
		}
	}
	return map[string]any{"data": data}
}
