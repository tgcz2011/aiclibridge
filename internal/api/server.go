// Package api hosts the HTTP API surface: routing, request decoding,
// response shaping, and middleware. It exposes the facade.Facade over a
// uniform HTTP interface that supports four shapes:
//
//   - Native AICLIBridge runs API (/v1/runs, /v1/agents, /v1/providers)
//   - OpenAI-compatible Chat Completions (/v1/chat/completions, /v1/models)
//   - Anthropic-compatible Messages (/v1/messages)
//   - Discovery / health (/healthz, /v1/models)
//
// The server uses only the standard library's net/http (decision 3: option A),
// leaning on Go 1.22+'s ServeMux path patterns for routing. SSE streaming is
// implemented with stdlib ResponseWriter.Flusher calls. Fault isolation is
// enforced at every handler: panics are recovered into 500 JSON, decode
// failures become 400, facade failures become 502, and a streaming handler
// never blocks the server when a client disconnects.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"strings"

	"github.com/tgcz2011/aiclibridge/internal/config"
	"github.com/tgcz2011/aiclibridge/internal/detect"
	facadepkg "github.com/tgcz2011/aiclibridge/internal/facade"
	"github.com/tgcz2011/aiclibridge/internal/store"
	"github.com/tgcz2011/aiclibridge/pkg/protocol"
)

// facade is the subset of *facade.Facade the API server needs. Defining it
// here decouples the handlers from the concrete type so tests can inject a
// fake. *facade.Facade satisfies this interface automatically (Go structural
// typing); production code passes the real Facade through NewServer.
type facade interface {
	StartRun(ctx context.Context, req facadepkg.RunRequest) (*facadepkg.RunHandle, error)
	GetRun(ctx context.Context, id string) (*facadepkg.RunResult, error)
	CancelRun(ctx context.Context, id string) error
	ListAgents(ctx context.Context) ([]detect.CLIInfo, error)
	ListProviders(ctx context.Context, cli string) ([]detect.ProviderInfo, error)
	// GetUsageStats backs the /v1/stats endpoints. It is a thin store
	// passthrough; the api layer prices the rows via the pricing table.
	GetUsageStats(ctx context.Context, since, until int64) ([]store.UsageStatRow, error)
}

// Server is the HTTP API server. It is safe for concurrent use: the mux is
// built once in NewServer and never mutated, and the facade it holds is
// itself concurrency-safe. A zero Server is NOT usable — always construct
// via NewServer.
type Server struct {
	fc     facade
	cfg    *config.Config
	logger *slog.Logger
	mux    *http.ServeMux
}

// NewServer wires the facade into a configured Server. It builds the route
// table once; Handler returns the resulting mux for use with http.ListenAndServe.
// A nil logger falls back to slog.Default so the server always has a sink.
//
// NewServer accepts the concrete *facade.Facade; the unexported newServer
// accepts the facade interface so tests in this package can inject a fake.
func NewServer(fc *facadepkg.Facade, cfg *config.Config, logger *slog.Logger) *Server {
	return newServer(fc, cfg, logger)
}

// newServer is the testable constructor. It accepts the facade interface so
// tests can inject a fakeFacade without going through the real Facade. The
// concrete *facade.Facade satisfies the interface, so NewServer delegates here.
func newServer(fc facade, cfg *config.Config, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		fc:     fc,
		cfg:    cfg,
		logger: logger,
		mux:    http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// Handler returns the configured mux, ready to hand to http.Server.
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAddr returns the address the server should listen on, derived from
// cfg.Listen. The caller (main.go) uses this to construct http.Server.
func (s *Server) ListenAddr() string { return s.cfg.Listen }

// registerRoutes wires every endpoint onto the mux. Each route is wrapped
// in a middleware chain: CORS (outermost) → recover → logging → auth (when
// required) → handler. /healthz and /v1/models skip auth because OpenAI
// clients frequently list models without credentials and the health probe
// must always succeed.
func (s *Server) registerRoutes() {
	// CORS preflight. Go 1.22+'s ServeMux returns 405 for an OPTIONS
	// request against a method-specific pattern before any handler runs,
	// which would bypass the corsMiddleware that short-circuits OPTIONS to
	// 204. Registering a single catch-all OPTIONS route (unauthenticated,
	// CORS-wrapped) lets every path's preflight reach corsMiddleware. The
	// handler is a no-op because corsMiddleware never calls next for OPTIONS.
	s.mux.Handle("OPTIONS /{path...}", s.chain(func(http.ResponseWriter, *http.Request) {}, false))

	// Health probe — no auth, always available.
	s.mux.Handle("GET /healthz", s.chain(s.handleHealthz, false))

	// Native AICLIBridge API.
	s.mux.Handle("POST /v1/runs", s.chain(s.handleCreateRun, true))
	s.mux.Handle("GET /v1/runs/{id}", s.chain(s.handleGetRun, true))
	s.mux.Handle("POST /v1/runs/{id}/cancel", s.chain(s.handleCancelRun, true))
	s.mux.Handle("GET /v1/agents", s.chain(s.handleListAgents, true))
	s.mux.Handle("GET /v1/agents/{cli}", s.chain(s.handleListAgent, true))
	s.mux.Handle("GET /v1/providers", s.chain(s.handleListProviders, true))

	// OpenAI-compatible. /v1/models is listed without auth (OpenAI clients
	// enumerate models unauthenticated); chat completions require auth.
	s.mux.Handle("GET /v1/models", s.chain(s.handleListModels, false))
	s.mux.Handle("POST /v1/chat/completions", s.chain(s.handleOpenAIChat, true))
	s.mux.Handle("POST /v1/chat/completions/{id}/cancel", s.chain(s.handleOpenAIChatCancel, true))

	// Anthropic-compatible.
	s.mux.Handle("GET /v1/anthropic/models", s.chain(s.handleAnthropicModels, true))
	s.mux.Handle("POST /v1/messages", s.chain(s.handleAnthropicMessages, true))
	s.mux.Handle("POST /v1/messages/{id}/cancel", s.chain(s.handleAnthropicCancel, true))

	// Stats: token usage aggregation, pricing table, and cost summary.
	// All authed — usage data is operational, not for unauthenticated
	// enumeration.
	s.mux.Handle("GET /v1/stats/usage", s.chain(s.handleStatsUsage, true))
	s.mux.Handle("GET /v1/stats/prices", s.chain(s.handleStatsPrices, true))
	s.mux.Handle("GET /v1/stats/summary", s.chain(s.handleStatsSummary, true))

	// pprof debug endpoints. Unauthenticated — the daemon listens on
	// loopback by default so this is safe; if you bind to a public
	// interface, put the daemon behind an authenticating reverse proxy.
	// These endpoints let operators diagnose high-concurrency behaviour
	// (goroutine leaks, heap growth, mutex contention) without restarting.
	s.mux.Handle("GET /debug/pprof/", s.chain(http.HandlerFunc(pprof.Index), false))
	s.mux.Handle("GET /debug/pprof/{path}", s.chain(http.HandlerFunc(pprof.Index), false))
}

// chain wraps a handler in the standard middleware stack. requireAuth
// controls whether the API-key middleware is applied. The order is
// CORS → recover → logging → auth → handler, so a panic anywhere down the
// chain is recovered, every request is logged, and CORS headers apply even
// to error responses.
func (s *Server) chain(h http.HandlerFunc, requireAuth bool) http.Handler {
	var handler http.Handler = h
	if requireAuth {
		handler = s.authMiddleware(handler)
	}
	handler = s.loggingMiddleware(handler)
	handler = s.recoverMiddleware(handler)
	handler = s.corsMiddleware(handler)
	return handler
}

// ── Shared helpers ──

// maxBodyBytes caps request bodies at 10MB to prevent a runaway client
// from exhausting daemon memory. Applies to every JSON-decoded handler.
const maxBodyBytes = 10 * 1024 * 1024

// decodeJSON reads a JSON body into v with a 10MB cap. On failure it writes
// a 400 JSON error and returns false; the caller should return immediately.
// On success it returns true and the caller may proceed.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest,
			"invalid request body: "+err.Error(), "invalid_request_error", nil)
		return false
	}
	return true
}

// writeJSON serialises v as JSON and writes it with the given status. A
// marshal failure is logged but cannot be recovered — the caller already
// committed the status code. The Content-Type is always application/json.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError emits the canonical error envelope used across all endpoints.
// The shape mirrors OpenAI's `{"error":{"message":...,"type":...}}` so
// OpenAI/Anthropic clients get a familiar error body. cause, when non-nil,
// is appended as a `details` field for debugging.
func writeError(w http.ResponseWriter, status int, msg, errType string, cause error) {
	body := map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    errType,
		},
	}
	if cause != nil {
		body["error"].(map[string]any)["details"] = cause.Error()
	}
	writeJSON(w, status, body)
}

// writeSSEHeaders commits the SSE response headers and flushes the initial
// empty frame so the client sees the stream open immediately. The caller
// must have a *http.Flusher available; if not, writeSSEHeaders returns false
// and the caller should fall back to a non-streaming error.
func writeSSEHeaders(w http.ResponseWriter) bool {
	if _, ok := w.(http.Flusher); !ok {
		return false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	return true
}

// writeSSEData writes a single `data: <line>\n\n` frame and flushes. Used
// for OpenAI/Anthropic-shaped SSE streams where each frame is a custom JSON
// object (not the protocol.Event shape that protocol.WriteSSEEvent emits).
func writeSSEData(w http.ResponseWriter, data string) error {
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// writeSSENamedEvent writes an SSE frame with both `event:` and `data:`
// lines, used for Anthropic's typed event stream (message_start, etc.).
func writeSSENamedEvent(w http.ResponseWriter, eventType, data string) error {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// streamNativeEvents pipes a facade event channel to the SSE response using
// the native protocol.WriteSSEEvent schema. It cancels the run if the client
// disconnects (r.Context().Done()) or if a write fails (client gone), so a
// dropped consumer never wedges the forwarder. Returns when the channel
// closes (run finished) or the client is gone.
func streamNativeEvents(w http.ResponseWriter, r *http.Request, handle *facadepkg.RunHandle) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError,
			"streaming not supported", "server_error", nil)
		return
	}
	flusher.Flush()
	gone := r.Context().Done()
	for {
		select {
		case ev, ok := <-handle.Events:
			if !ok {
				return
			}
			if err := protocol.WriteSSEEvent(w, ev); err != nil {
				handle.Cancel()
				return
			}
			flusher.Flush()
		case <-gone:
			handle.Cancel()
			return
		}
	}
}

// resolveModel canonicalises a model identifier to the `CLI/provider/model`
// form. A model already containing "/" is returned verbatim (the facade
// validates the segments). A bare model name (no "/") is looked up against
// the catalog; the first matching model name wins. Returns an error if no
// match is found.
func (s *Server) resolveModel(model string) (string, error) {
	if model == "" {
		return "", nil
	}
	if strings.Contains(model, "/") {
		return model, nil
	}
	agents, err := s.fc.ListAgents(context.Background())
	if err != nil {
		return "", err
	}
	for _, cli := range agents {
		for _, p := range cli.Providers {
			for _, m := range p.Models {
				if m.Name == model {
					return detect.ModelName(cli.Name, p.Name, m.Name), nil
				}
			}
		}
	}
	return "", fmt.Errorf("model %q not found in catalog", model)
}
