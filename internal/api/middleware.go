package api

import (
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// corsMiddleware applies permissive CORS headers to every response and
// short-circuits OPTIONS preflight requests with 204. Allow-Origin is "*"
// because aiclibridge is a local-first daemon: the browser client is
// typically a dev tool on the same machine, and per-origin configuration is
// out of scope for v1. The headers are set on every response (including
// errors) so a CORS-aware client always sees them.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, DELETE")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-api-key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recoverMiddleware defers a recover on every request. A panic anywhere in
// the downstream handler chain is converted to a 500 JSON error and logged
// with the stack trace, so a buggy handler or adapter never crashes the
// daemon process. The response is only written if the panic happened before
// the handler committed its own WriteHeader; otherwise we can only log,
// because the response is already in flight.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("api: handler panic recovered",
					"method", r.Method,
					"path", r.URL.Path,
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				// writeJSON only succeeds if the response has not been
				// committed yet; if it has, the write is a no-op (the
				// client gets a truncated response, which is the best we
				// can do for a mid-stream panic).
				writeError(w, http.StatusInternalServerError,
					"internal server error", "server_error", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware records one log line per request at Info level: method,
// path, status, and duration. It wraps the ResponseWriter to capture the
// status code without interfering with the response body or the underlying
// Flusher (for SSE handlers). Duration is measured wall-clock from the
// handler entry to exit; streaming handlers log when the stream ends, which
// may be long after the first byte — that is the intended signal.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		s.logger.Info("api: request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// authMiddleware enforces the single static API key from cfg.APIKey. When
// the key is empty (development mode) every request is allowed through
// without inspection. Otherwise the client must present the key in either
// `Authorization: Bearer <key>` or `x-api-key: <key>`; a missing or
// mismatched key yields a 401 JSON error. Both header forms are accepted so
// the same daemon serves OpenAI clients (Bearer) and Anthropic clients
// (x-api-key) without per-client configuration.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIKey == "" {
			next.ServeHTTP(w, r)
			return
		}
		if extractAPIKey(r) != s.cfg.APIKey {
			writeError(w, http.StatusUnauthorized,
				"invalid api key", "authentication_error", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// extractAPIKey pulls the presented credential from either supported header,
// returning the empty string when neither is present. The Bearer scheme is
// parsed leniently: a missing or malformed prefix yields the empty string
// rather than an error, matching how OpenAI's API treats a missing header
// (it falls through to the 401 path uniformly).
func extractAPIKey(r *http.Request) string {
	if k := r.Header.Get("x-api-key"); k != "" {
		return strings.TrimSpace(k)
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, prefix))
}

// statusRecorder wraps http.ResponseWriter to capture the status code for
// logging. Embedding the http.ResponseWriter interface only promotes the
// interface's own methods (Header/Write/WriteHeader); the concrete type's
// Flush is NOT promoted, so SSE streaming would break unless we forward it
// explicitly below. Hijack/websocket paths are out of scope for v1.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader records the status before delegating. Called at most once per
// response by the downstream handler; subsequent calls are a no-op at the
// net/http layer (it tracks its own committed flag).
func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the wrapped ResponseWriter when it implements
// http.Flusher. Without this, SSE handlers wrapped by loggingMiddleware see
// a non-Flusher writer and fall back to a 500 "streaming not supported".
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
