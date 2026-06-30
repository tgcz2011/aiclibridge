package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tgcz2011/aiclibridge/internal/config"
	"github.com/tgcz2011/aiclibridge/internal/detect"
	facadepkg "github.com/tgcz2011/aiclibridge/internal/facade"
	"github.com/tgcz2011/aiclibridge/pkg/protocol"
)

// ── fakeFacade ──
//
// fakeFacade implements the unexported facade interface so tests can drive
// the HTTP layer without spawning real CLI subprocesses. Each StartRun
// spawns a goroutine that emits the preconfigured events + terminal event
// and closes the channel, mirroring the real Facade's forwarder.

// fakeFacade is a controllable test double for facade.Facade. It is safe
// for concurrent use (mutex guards all mutable fields).
type fakeFacade struct {
	mu          sync.Mutex
	catalog     []detect.CLIInfo
	events      []protocol.Event // events emitted per StartRun (excluding terminal)
	terminal    protocol.Event   // terminal event appended after events
	startErr    error
	getRunErr   error
	cancelErr   error
	listErr     error
	panicOnStart bool

	idCounter int
	started   []facadepkg.RunRequest
	cancelled []string
	liveRuns  map[string]*facadepkg.RunHandle
}

func (f *fakeFacade) StartRun(ctx context.Context, req facadepkg.RunRequest) (*facadepkg.RunHandle, error) {
	f.mu.Lock()
	f.idCounter++
	id := f.idCounter
	f.started = append(f.started, req)
	events := append([]protocol.Event(nil), f.events...)
	terminal := f.terminal
	startErr := f.startErr
	panicOnStart := f.panicOnStart
	f.mu.Unlock()

	if panicOnStart {
		panic("intentional panic for test")
	}
	if startErr != nil {
		return nil, startErr
	}

	runID := "run-" + itoa(id)
	runCtx, cancel := context.WithCancel(ctx)
	// Buffer holds every event + terminal so the goroutine never blocks;
	// tests stay deterministic without synchronising on send timing.
	ch := make(chan protocol.Event, len(events)+2)
	handle := &facadepkg.RunHandle{
		ID:      runID,
		Adapter: "fake",
		Model:   req.Model,
		Events:  ch,
		Cancel:  cancel,
	}
	f.mu.Lock()
	if f.liveRuns == nil {
		f.liveRuns = map[string]*facadepkg.RunHandle{}
	}
	f.liveRuns[runID] = handle
	f.mu.Unlock()

	go func() {
		defer close(ch)
		defer cancel()
		for _, ev := range events {
			select {
			case ch <- ev:
			case <-runCtx.Done():
				return
			}
		}
		if terminal.Type != "" {
			select {
			case ch <- terminal:
			case <-runCtx.Done():
				return
			}
		}
	}()
	return handle, nil
}

func (f *fakeFacade) GetRun(ctx context.Context, id string) (*facadepkg.RunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getRunErr != nil {
		return nil, f.getRunErr
	}
	events := append([]protocol.Event(nil), f.events...)
	if f.terminal.Type != "" {
		events = append(events, f.terminal)
	}
	return &facadepkg.RunResult{
		ID:     id,
		Status: "completed",
		Output: "replayed output",
		Events: events,
	}, nil
}

func (f *fakeFacade) CancelRun(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, id)
	if f.cancelErr != nil {
		return f.cancelErr
	}
	if handle, ok := f.liveRuns[id]; ok {
		handle.Cancel()
		return nil
	}
	return errNotFound(id)
}

func (f *fakeFacade) ListAgents(ctx context.Context) ([]detect.CLIInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.catalog, f.listErr
}

func (f *fakeFacade) ListProviders(ctx context.Context, cli string) ([]detect.ProviderInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.catalog {
		if c.Name == cli {
			return c.Providers, nil
		}
	}
	return nil, errNotFound(cli)
}

// ── Test helpers ──

// newFakeFacade returns a facade preloaded with the default catalog and a
// simple "Hello world" event timeline ending in a completed result.
func newFakeFacade() *fakeFacade {
	return &fakeFacade{
		catalog: detect.DefaultCatalog(),
		events: []protocol.Event{
			{Type: protocol.EventText, Seq: 0, Content: "Hello"},
			{Type: protocol.EventText, Seq: 1, Content: " world"},
		},
		terminal: protocol.Event{
			Type: protocol.EventResult,
			Seq:  2,
			Result: &protocol.ResultPayload{
				Status:     "completed",
				Output:     "Hello world",
				DurationMs: 42,
			},
		},
	}
}

// newTestServer builds a Server wired to fc. cfg may be nil (defaults, no
// auth); a non-nil cfg lets a test enable the API key.
func newTestServer(fc *fakeFacade, cfg *config.Config) *Server {
	if cfg == nil {
		cfg = config.Defaults()
		cfg.APIKey = "" // dev mode: no auth unless a test sets it
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newServer(fc, cfg, logger)
}

// doRequest fires one request at the server and returns the recorder. body
// may be "". headers are applied verbatim (use to set Authorization etc.).
func doRequest(t *testing.T, s *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// authHeader returns a Bearer header map for the given key.
func authHeader(key string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + key}
}

// errNotFound is the canonical "not found" error for the fake facade.
type notFoundErr struct{ id string }

func (e notFoundErr) Error() string { return "fake: run " + e.id + " not found" }
func errNotFound(id string) error    { return notFoundErr{id: id} }

// itoa is a dependency-free int→string to keep test imports small.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// ── Tests ──

// Test 1: healthz returns 200 and the ok status with no auth required.
func TestHealthz(t *testing.T) {
	s := newTestServer(newFakeFacade(), nil)
	rec := doRequest(t, s, "GET", "/healthz", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status: got %q, want %q", body["status"], "ok")
	}
}

// Test 2: with an API key configured, a missing/incorrect key yields 401
// and a correct Bearer token passes. /v1/models stays open without auth.
func TestAuth(t *testing.T) {
	cfg := config.Defaults()
	cfg.APIKey = "secret"
	s := newTestServer(newFakeFacade(), cfg)

	// No auth → 401 on a protected route.
	rec := doRequest(t, s, "GET", "/v1/agents", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth agents: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	// Correct Bearer → 200.
	rec = doRequest(t, s, "GET", "/v1/agents", "", authHeader("secret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("authed agents: got %d, want %d", rec.Code, http.StatusOK)
	}

	// x-api-key form also accepted.
	rec = doRequest(t, s, "GET", "/v1/agents", "", map[string]string{"x-api-key": "secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("x-api-key agents: got %d, want %d", rec.Code, http.StatusOK)
	}

	// /v1/models is exempt from auth (OpenAI clients list unauthenticated).
	rec = doRequest(t, s, "GET", "/v1/models", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("models no-auth: got %d, want %d", rec.Code, http.StatusOK)
	}

	// Wrong key → 401.
	rec = doRequest(t, s, "GET", "/v1/agents", "", authHeader("wrong"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-key agents: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// Test 3: POST /v1/runs stream=true emits native SSE: event: lines, data:
// lines, and a terminal result frame.
func TestCreateRunStreaming(t *testing.T) {
	s := newTestServer(newFakeFacade(), nil)
	body := `{"model":"claude/anthropic/claude-sonnet-4.5","prompt":"hi","stream":true}`
	rec := doRequest(t, s, "POST", "/v1/runs", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type: got %q, want text/event-stream", ct)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "event: text\n") {
		t.Errorf("missing text event frame:\n%s", out)
	}
	if !strings.Contains(out, "event: result\n") {
		t.Errorf("missing result event frame:\n%s", out)
	}
	if !strings.Contains(out, `"content":"Hello"`) {
		t.Errorf("missing Hello content:\n%s", out)
	}
}

// Test 4: POST /v1/runs stream=false returns a RunResult JSON with the
// aggregated output and the events timeline.
func TestCreateRunNonStreaming(t *testing.T) {
	s := newTestServer(newFakeFacade(), nil)
	body := `{"model":"claude/anthropic/claude-sonnet-4.5","prompt":"hi","stream":false}`
	rec := doRequest(t, s, "POST", "/v1/runs", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var result facadepkg.RunResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.Output != "Hello world" {
		t.Errorf("output: got %q, want %q", result.Output, "Hello world")
	}
	if result.Status != "completed" {
		t.Errorf("status: got %q, want %q", result.Status, "completed")
	}
	if len(result.Events) != 3 {
		t.Errorf("events: got %d, want 3", len(result.Events))
	}
}

// Test 5: POST /v1/chat/completions stream=true emits OpenAI chunks ending
// in [DONE]. A bare model name is resolved from the catalog.
func TestOpenAIChatStreaming(t *testing.T) {
	fc := newFakeFacade()
	s := newTestServer(fc, nil)
	// Bare model name → resolved to claude/anthropic/claude-sonnet-4.5.
	body := `{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec := doRequest(t, s, "POST", "/v1/chat/completions", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "chat.completion.chunk") {
		t.Errorf("missing chunk object:\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("missing [DONE] sentinel:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("missing terminal finish_reason stop:\n%s", out)
	}
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Errorf("missing assistant role in leading chunk:\n%s", out)
	}
	// Verify the model was resolved to the full routing key.
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.started) != 1 || fc.started[0].Model != "claude/anthropic/claude-sonnet-4.5" {
		t.Errorf("model resolution: got %+v", fc.started)
	}
}

// Test 6: POST /v1/chat/completions stream=false returns a full
// ChatCompletion with the aggregated content.
func TestOpenAIChatNonStreaming(t *testing.T) {
	s := newTestServer(newFakeFacade(), nil)
	body := `{"model":"claude/anthropic/claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}],"stream":false}`
	rec := doRequest(t, s, "POST", "/v1/chat/completions", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var comp openaiChatCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &comp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if comp.Object != "chat.completion" {
		t.Errorf("object: got %q, want chat.completion", comp.Object)
	}
	if len(comp.Choices) != 1 {
		t.Fatalf("choices: got %d, want 1", len(comp.Choices))
	}
	if comp.Choices[0].Message.Content != "Hello world" {
		t.Errorf("content: got %q, want %q", comp.Choices[0].Message.Content, "Hello world")
	}
	if comp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason: got %q, want stop", comp.Choices[0].FinishReason)
	}
}

// Test 7: GET /v1/models returns every CLI/provider/model triple in the
// OpenAI list shape, with id in CLI/provider/model form.
func TestOpenAIModels(t *testing.T) {
	s := newTestServer(newFakeFacade(), nil)
	rec := doRequest(t, s, "GET", "/v1/models", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var env struct {
		Object string `json:"object"`
		Data    []openAIModelsEntry `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Object != "list" {
		t.Errorf("object: got %q, want list", env.Object)
	}
	if len(env.Data) == 0 {
		t.Fatal("data is empty")
	}
	// Spot-check the first entry: claude/anthropic/claude-sonnet-4.5.
	first := env.Data[0]
	if first.ID != "claude/anthropic/claude-sonnet-4.5" {
		t.Errorf("first id: got %q, want %q", first.ID, "claude/anthropic/claude-sonnet-4.5")
	}
	if first.Object != "model" {
		t.Errorf("first object: got %q, want model", first.Object)
	}
	if first.OwnedBy != "anthropic" {
		t.Errorf("first owned_by: got %q, want anthropic", first.OwnedBy)
	}
}

// Test 8: POST /v1/messages stream=true emits the Anthropic event sequence:
// message_start, content_block_start, content_block_delta, content_block_stop,
// message_delta, message_stop.
func TestAnthropicMessagesStreaming(t *testing.T) {
	s := newTestServer(newFakeFacade(), nil)
	body := `{"model":"claude/anthropic/claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec := doRequest(t, s, "POST", "/v1/messages", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	out := rec.Body.String()
	for _, want := range []string{
		"event: message_start\n",
		"event: content_block_start\n",
		"event: content_block_delta\n",
		"event: content_block_stop\n",
		"event: message_delta\n",
		"event: message_stop\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in stream:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `"text_delta"`) {
		t.Errorf("missing text_delta:\n%s", out)
	}
}

// Test 9: POST /v1/messages stream=false returns a full Anthropic message
// with a single text content block.
func TestAnthropicMessagesNonStreaming(t *testing.T) {
	s := newTestServer(newFakeFacade(), nil)
	body := `{"model":"claude/anthropic/claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}],"stream":false,"system":"be brief"}`
	rec := doRequest(t, s, "POST", "/v1/messages", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var msg anthropicMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Type != "message" {
		t.Errorf("type: got %q, want message", msg.Type)
	}
	if msg.Role != "assistant" {
		t.Errorf("role: got %q, want assistant", msg.Role)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" {
		t.Fatalf("content: got %+v", msg.Content)
	}
	if msg.Content[0].Text != "Hello world" {
		t.Errorf("text: got %q, want %q", msg.Content[0].Text, "Hello world")
	}
	if msg.StopReason != "end_turn" {
		t.Errorf("stop_reason: got %q, want end_turn", msg.StopReason)
	}
}

// Test 10: GET /v1/runs/{id} replays the stored timeline.
func TestGetRun(t *testing.T) {
	s := newTestServer(newFakeFacade(), nil)
	rec := doRequest(t, s, "GET", "/v1/runs/run-99", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var result facadepkg.RunResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.ID != "run-99" {
		t.Errorf("id: got %q, want run-99", result.ID)
	}
	if result.Output != "replayed output" {
		t.Errorf("output: got %q, want %q", result.Output, "replayed output")
	}
}

// Test 11: POST /v1/runs/{id}/cancel returns 200 for a live run and 404
// for an unknown id.
func TestCancelRun(t *testing.T) {
	fc := newFakeFacade()
	// Pre-populate a live run so cancel finds it.
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle := &facadepkg.RunHandle{
		ID:      "run-live",
		Adapter: "fake",
		Events:  make(chan protocol.Event),
		Cancel:  cancel,
	}
	fc.liveRuns = map[string]*facadepkg.RunHandle{"run-live": handle}
	s := newTestServer(fc, nil)

	rec := doRequest(t, s, "POST", "/v1/runs/run-live/cancel", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel live: got %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["cancelled"] != true {
		t.Errorf("cancelled flag: got %v, want true", body["cancelled"])
	}

	// Unknown id → 404.
	rec = doRequest(t, s, "POST", "/v1/runs/nope/cancel", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cancel unknown: got %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// Test 12: GET /v1/agents returns the catalog under {"agents":[...]}.
func TestListAgents(t *testing.T) {
	s := newTestServer(newFakeFacade(), nil)
	rec := doRequest(t, s, "GET", "/v1/agents", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Agents []detect.CLIInfo `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Agents) == 0 {
		t.Fatal("agents is empty")
	}
	if body.Agents[0].Name != "claude" {
		t.Errorf("first agent: got %q, want claude", body.Agents[0].Name)
	}
}

// Test 13: GET /v1/providers returns the deduplicated provider names.
func TestListProviders(t *testing.T) {
	s := newTestServer(newFakeFacade(), nil)
	rec := doRequest(t, s, "GET", "/v1/providers", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Providers []string `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Default catalog has anthropic, openai, google, bytedance.
	want := map[string]bool{"anthropic": false, "openai": false, "google": false, "bytedance": false}
	seen := map[string]bool{}
	for _, p := range body.Providers {
		seen[p] = true
	}
	for w := range want {
		if !seen[w] {
			t.Errorf("provider %q missing from %v", w, body.Providers)
		}
	}
	// No duplicates.
	if len(seen) != len(body.Providers) {
		t.Errorf("duplicate providers: %v", body.Providers)
	}
}

// Test 14: OPTIONS preflight returns 204 with CORS headers.
func TestCORSPreflight(t *testing.T) {
	s := newTestServer(newFakeFacade(), nil)
	rec := doRequest(t, s, "OPTIONS", "/v1/runs", "", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("allow-origin: got %q, want *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("allow-methods header missing")
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") {
		t.Errorf("allow-headers missing Authorization: %q", got)
	}
}

// Test 15: a panicking facade is recovered; the handler returns 500 and the
// server stays up for subsequent requests.
func TestPanicRecovery(t *testing.T) {
	fc := newFakeFacade()
	fc.panicOnStart = true
	s := newTestServer(fc, nil)
	body := `{"model":"claude/anthropic/claude-sonnet-4.5","prompt":"hi","stream":false}`
	rec := doRequest(t, s, "POST", "/v1/runs", body, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	var errBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errObj, ok := errBody["error"].(map[string]any)
	if !ok || errObj["type"] != "server_error" {
		t.Errorf("error envelope: got %+v", errBody)
	}

	// Server is still usable: a normal request to healthz succeeds.
	rec = doRequest(t, s, "GET", "/healthz", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("post-panic healthz: got %d, want %d", rec.Code, http.StatusOK)
	}
}

// Test 16: GET /v1/agents/{cli} returns a single CLI and 404 for unknown.
func TestListAgent(t *testing.T) {
	s := newTestServer(newFakeFacade(), nil)
	rec := doRequest(t, s, "GET", "/v1/agents/claude", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var cli detect.CLIInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &cli); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cli.Name != "claude" {
		t.Errorf("name: got %q, want claude", cli.Name)
	}

	rec = doRequest(t, s, "GET", "/v1/agents/unknown", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown cli: got %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// Test 17: a malformed JSON body yields 400, not a 500 or a hang.
func TestBadJSON(t *testing.T) {
	s := newTestServer(newFakeFacade(), nil)
	rec := doRequest(t, s, "POST", "/v1/runs", "{not json", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
