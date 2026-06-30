package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// ── Test helpers ──

// newAcpTestBackend returns an acpBackend for name with a discarding
// logger, suitable for unit tests that exercise the pure parsing /
// arg-building / protocol paths without spawning a real CLI process.
func newAcpTestBackend(t *testing.T, name string) *acpBackend {
	t.Helper()
	b, err := newAcpBackend(name, Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("newAcpBackend(%q): %v", name, err)
	}
	return b
}

// acpNoopCloseStdin is a no-op closeStdin for processAcpEvents tests where
// the stdin lifecycle is not under test. Mirrors qwen_test.go's helper.
func acpNoopCloseStdin() {}

// acpCaptureStdin is an acpStdin backed by a bytes.Buffer so tests can
// inspect the JSON-RPC requests processAcpEvents writes to the CLI.
type acpCaptureStdin struct {
	buf bytes.Buffer
}

func newAcpCaptureStdin() *acpCaptureStdin {
	return &acpCaptureStdin{}
}

// asWriter returns an acpStdin whose writes land in the capture buffer.
func (c *acpCaptureStdin) asWriter() *acpStdin {
	return &acpStdin{w: &c.buf}
}

// requests parses each newline-delimited JSON-RPC request the capture
// buffer received, in write order. Used to assert on the protocol flow.
func (c *acpCaptureStdin) requests(t *testing.T) []jsonrpcRequest {
	t.Helper()
	var reqs []jsonrpcRequest
	for _, line := range strings.Split(strings.TrimRight(c.buf.String(), "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r jsonrpcRequest
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("acp capture: unmarshal request %q: %v", line, err)
		}
		reqs = append(reqs, r)
	}
	return reqs
}

// drainMessages closes ch and returns its accumulated messages in send
// order. The caller must not have another goroutine writing to ch.
func drainMessages(ch chan Message) []Message {
	close(ch)
	var msgs []Message
	for m := range ch {
		msgs = append(msgs, m)
	}
	return msgs
}

// ── TestAcpBackendConfig ──
//
// Pins the acpBackends table: all eight ACP CLIs are present with the
// expected binary, ACP entry args, MCP flag, and resume mode. Adding a
// CLI is a one-line table edit; this test guards against an accidental
// rename or a dropped entry.

func TestAcpBackendConfig(t *testing.T) {
	t.Parallel()

	want := map[string]acpBackendConfig{
		"copilot": {binary: "copilot", acpArgs: []string{"--acp"}, mcpFlag: "--additional-mcp-config", mcpInline: true, resumeMode: "session"},
		"goose":   {binary: "goose", acpArgs: []string{"acp"}, mcpFlag: "--mcp-config", resumeMode: "none"},
		"cursor":  {binary: "cursor-agent", acpArgs: []string{"--acp"}, mcpFlag: "--mcp-config", resumeMode: "none"},
		"kimi":    {binary: "kimi", acpArgs: []string{"acp"}, mcpFlag: "--mcp-config", resumeMode: "none"},
		"kiro":    {binary: "kiro", acpArgs: []string{"--acp"}, mcpFlag: "--mcp-config", resumeMode: "none"},
		"qoder":   {binary: "qoder", acpArgs: []string{"--acp"}, mcpFlag: "--mcp-config", resumeMode: "none"},
		"hermes":  {binary: "hermes", acpArgs: []string{"--acp"}, mcpFlag: "--mcp-config", resumeMode: "none"},
		"auggie":  {binary: "auggie", acpArgs: []string{"--acp"}, mcpFlag: "--mcp-config", resumeMode: "none"},
	}

	if len(acpBackends) != len(want) {
		t.Fatalf("acpBackends: got %d entries, want %d", len(acpBackends), len(want))
	}

	for name, cfgWant := range want {
		cfg, ok := acpBackends[name]
		if !ok {
			t.Errorf("acpBackends: missing %q", name)
			continue
		}
		if cfg.binary != cfgWant.binary {
			t.Errorf("acpBackends[%q].binary: got %q, want %q", name, cfg.binary, cfgWant.binary)
		}
		if !slices.Equal(cfg.acpArgs, cfgWant.acpArgs) {
			t.Errorf("acpBackends[%q].acpArgs: got %v, want %v", name, cfg.acpArgs, cfgWant.acpArgs)
		}
		if cfg.mcpFlag != cfgWant.mcpFlag {
			t.Errorf("acpBackends[%q].mcpFlag: got %q, want %q", name, cfg.mcpFlag, cfgWant.mcpFlag)
		}
		if cfg.mcpInline != cfgWant.mcpInline {
			t.Errorf("acpBackends[%q].mcpInline: got %v, want %v", name, cfg.mcpInline, cfgWant.mcpInline)
		}
		if cfg.resumeMode != cfgWant.resumeMode {
			t.Errorf("acpBackends[%q].resumeMode: got %q, want %q", name, cfg.resumeMode, cfgWant.resumeMode)
		}
	}

	// newAcpBackend must reject an unknown name so the daemon surfaces a
	// misconfiguration instead of silently defaulting.
	if _, err := newAcpBackend("nope", Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err == nil {
		t.Errorf("newAcpBackend(\"nope\"): expected error, got nil")
	}
}

// ── TestAcpBuildArgs ──
//
// Table-driven coverage of buildAcpArgs across the eight CLIs. Pins the
// ACP entry args (["--acp"] vs ["acp"]) and verifies the daemon-managed
// flags (--mcp-config / --additional-mcp-config / --acp) cannot be
// overridden by user-configured custom_args, while safe flags pass
// through. MCP flags are NOT in buildAcpArgs output — they are appended
// by Execute after the temp file is written.

func TestAcpBuildArgs(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("entry args per CLI", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name       string
			wantEntry  []string // leading args must equal this exactly
		}{
			{"copilot", []string{"--acp"}},
			{"goose", []string{"acp"}},
			{"cursor", []string{"--acp"}},
			{"kimi", []string{"acp"}},
			{"kiro", []string{"--acp"}},
			{"qoder", []string{"--acp"}},
			{"hermes", []string{"--acp"}},
			{"auggie", []string{"--acp"}},
		}
		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				bc := acpBackends[c.name]
				args := buildAcpArgs(bc, ExecOptions{}, logger)
				if len(args) < len(c.wantEntry) {
					t.Fatalf("%s: args %v shorter than entry %v", c.name, args, c.wantEntry)
				}
				if !slices.Equal(args[:len(c.wantEntry)], c.wantEntry) {
					t.Errorf("%s: entry args = %v, want %v", c.name, args[:len(c.wantEntry)], c.wantEntry)
				}
			})
		}
	})

	t.Run("custom args passthrough and protocol flag blocking", func(t *testing.T) {
		t.Parallel()

		bc := acpBackends["copilot"]
		args := buildAcpArgs(bc, ExecOptions{
			ExtraArgs:  []string{"--acp", "--additional-mcp-config", "/etc/evil.json"},
			CustomArgs: []string{"--mcp-config", "/etc/evil2.json", "--verbose", "true"},
		}, logger)

		// The MCP flags (and their values) must be stripped from custom args
		// — buildAcpArgs never adds them (Execute does, after the temp file
		// is written), so any occurrence here is a leaked custom value.
		for _, bad := range []string{"--additional-mcp-config", "--mcp-config", "/etc/evil.json", "/etc/evil2.json"} {
			if slices.Contains(args, bad) {
				t.Errorf("args %v: blocked flag %q leaked through", args, bad)
			}
		}
		// The config's --acp entry must remain exactly once; the --acp the
		// user put in ExtraArgs must be filtered (otherwise it'd appear
		// twice). Counting pins both the kept config copy and the dropped
		// custom copy at once.
		acpCount := 0
		for _, a := range args {
			if a == "--acp" {
				acpCount++
			}
		}
		if acpCount != 1 {
			t.Errorf("args %v: --acp should appear exactly once (from config), got %d", args, acpCount)
		}
		// Safe flags must survive.
		if !slices.Contains(args, "--verbose") || !slices.Contains(args, "true") {
			t.Errorf("args %v: safe custom flags dropped", args)
		}
	})

	t.Run("no model/system-prompt/resume flags injected", func(t *testing.T) {
		t.Parallel()

		bc := acpBackends["copilot"]
		args := buildAcpArgs(bc, ExecOptions{
			Model:           "gpt-5",
			SystemPrompt:    "be terse",
			ResumeSessionID: "sess-xyz",
			MaxTurns:        9,
		}, logger)
		// These are conveyed via JSON-RPC, not argv. No CLI flag should
		// appear for them.
		for _, bad := range []string{"-m", "--model", "--system-prompt", "--resume", "--max-turns", "--continue", "-c"} {
			if slices.Contains(args, bad) {
				t.Errorf("args %v: unexpected %q (conveyed via protocol, not argv)", args, bad)
			}
		}
	})
}

// ── TestAcpJsonRpcEncode ──
//
// Pins the JSON-RPC 2.0 request frame encoder: every frame is terminated
// by a newline, carries jsonrpc:"2.0", the requested id/method/params, and
// omits params entirely when nil (a parameterless method). Decodes back
// into the same struct so the wire shape is asserted, not just the bytes.

func TestAcpJsonRpcEncode(t *testing.T) {
	t.Parallel()

	t.Run("initialize frame", func(t *testing.T) {
		t.Parallel()
		data, err := encodeJsonrpcRequest("initialize", acpInitializeParams{
			ProtocolVersion:    acpProtocolVersion,
			ClientCapabilities: json.RawMessage(acpClientCapabilities),
		}, acpIDInitialize)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if len(data) == 0 || data[len(data)-1] != '\n' {
			t.Errorf("frame %q: missing trailing newline", data)
		}
		var r jsonrpcRequest
		if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if r.Jsonrpc != "2.0" {
			t.Errorf("jsonrpc: got %q, want %q", r.Jsonrpc, "2.0")
		}
		if r.ID != acpIDInitialize {
			t.Errorf("id: got %d, want %d", r.ID, acpIDInitialize)
		}
		if r.Method != "initialize" {
			t.Errorf("method: got %q, want %q", r.Method, "initialize")
		}
		// r.Params is `any` → decoded as map[string]any; re-marshal so we
		// can decode it into the typed acpInitializeParams struct.
		raw, err := json.Marshal(r.Params)
		if err != nil {
			t.Fatalf("re-marshal params: %v", err)
		}
		var p acpInitializeParams
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode params: %v", err)
		}
		if p.ProtocolVersion != acpProtocolVersion {
			t.Errorf("protocolVersion: got %q, want %q", p.ProtocolVersion, acpProtocolVersion)
		}
	})

	t.Run("session/prompt frame carries prompt + session id", func(t *testing.T) {
		t.Parallel()
		data, err := encodeJsonrpcRequest("session/prompt", acpSessionPromptParams{
			Prompt:    "hello",
			SessionID: "sess-1",
		}, acpIDPrompt)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		// Re-decode into a generic map so we can assert the nested fields
		// without depending on the any-typed Params field.
		var frame map[string]any
		if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &frame); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if frame["method"] != "session/prompt" {
			t.Errorf("method: got %v, want session/prompt", frame["method"])
		}
		params, _ := frame["params"].(map[string]any)
		if params["prompt"] != "hello" {
			t.Errorf("params.prompt: got %v, want hello", params["prompt"])
		}
		if params["sessionId"] != "sess-1" {
			t.Errorf("params.sessionId: got %v, want sess-1", params["sessionId"])
		}
	})

	t.Run("nil params omitted", func(t *testing.T) {
		t.Parallel()
		data, err := encodeJsonrpcRequest("session/cancel", nil, acpIDCancel)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		var frame map[string]any
		if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &frame); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, present := frame["params"]; present {
			t.Errorf("nil params must be omitted, got %v", frame["params"])
		}
	})
}

// ── TestAcpMcpConfig ──
//
// Verifies the MCP-injection flag differs per CLI: copilot uses
// --additional-mcp-config (and advertises inline JSON support), every
// other CLI uses --mcp-config. The actual argv append happens in Execute
// after writeMcpConfigToTemp; here we pin the config values that drive
// that branch so a table typo cannot silently route copilot's MCP config
// through the wrong flag.

func TestAcpMcpConfig(t *testing.T) {
	t.Parallel()

	// copilot is the only CLI with the additional-mcp-config flag and the
	// only one advertising inline JSON.
	if got := acpBackends["copilot"].mcpFlag; got != "--additional-mcp-config" {
		t.Errorf("copilot.mcpFlag: got %q, want --additional-mcp-config", got)
	}
	if !acpBackends["copilot"].mcpInline {
		t.Errorf("copilot.mcpInline: got false, want true")
	}

	// Every other CLI routes MCP through --mcp-config and does not
	// advertise inline JSON.
	for _, name := range []string{"goose", "cursor", "kimi", "kiro", "qoder", "hermes", "auggie"} {
		cfg := acpBackends[name]
		if cfg.mcpFlag != "--mcp-config" {
			t.Errorf("%s.mcpFlag: got %q, want --mcp-config", name, cfg.mcpFlag)
		}
		if cfg.mcpInline {
			t.Errorf("%s.mcpInline: got true, want false", name)
		}
	}

	// buildAcpArgs must never include an MCP flag — that append is Execute's
	// job, gated on opts.McpConfig being non-empty. Pinning this guards
	// against a future refactor double-injecting MCP.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, name := range []string{"copilot", "goose", "cursor", "kimi", "kiro", "qoder", "hermes", "auggie"} {
		args := buildAcpArgs(acpBackends[name], ExecOptions{McpConfig: json.RawMessage(`{}`)}, logger)
		if slices.Contains(args, "--mcp-config") || slices.Contains(args, "--additional-mcp-config") {
			t.Errorf("%s: buildAcpArgs leaked an MCP flag %v", name, args)
		}
	}
}

// ── TestAcpResume ──
//
// Pins the resume routing in isolation: when ResumeSessionID is non-empty
// the daemon sends session/load (not session/new) and threads the id into
// the params; when empty it sends session/new. Verified end-to-end through
// processAcpEvents so the routing and the params shape are asserted
// together, not just the helper.

func TestAcpResume(t *testing.T) {
	t.Parallel()

	t.Run("acpSessionLoadMethod routing", func(t *testing.T) {
		t.Parallel()
		if got := acpSessionLoadMethod("sess-1"); got != "session/load" {
			t.Errorf("with resume id: got %q, want session/load", got)
		}
		if got := acpSessionLoadMethod(""); got != "session/new" {
			t.Errorf("without resume id: got %q, want session/new", got)
		}
	})

	t.Run("session/load request on resume", func(t *testing.T) {
		t.Parallel()
		b := newAcpTestBackend(t, "copilot")
		cap := newAcpCaptureStdin()
		ch := make(chan Message, 256)

		ndjson := strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"1"}}`,
			`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"resumed-sess"}}`,
			`{"jsonrpc":"2.0","method":"session/finished","params":{"sessionId":"resumed-sess"}}`,
		}, "\n")

		res := b.processAcpEvents(strings.NewReader(ndjson), ch, cap.asWriter(), acpNoopCloseStdin, "hello", "resume-me", "test-model")
		drainMessages(ch)

		reqs := cap.requests(t)
		if len(reqs) != 3 {
			t.Fatalf("expected 3 requests (init, load, prompt), got %d: %+v", len(reqs), reqs)
		}
		if reqs[1].Method != "session/load" {
			t.Errorf("request[1].method: got %q, want session/load", reqs[1].Method)
		}
		var loadParams acpSessionLoadParams
		_ = json.Unmarshal(marshalJSON(t, reqs[1].Params), &loadParams)
		if loadParams.SessionID != "resume-me" {
			t.Errorf("session/load params.sessionId: got %q, want resume-me", loadParams.SessionID)
		}
		if res.sessionID != "resumed-sess" {
			t.Errorf("result.sessionID: got %q, want resumed-sess", res.sessionID)
		}
	})

	t.Run("session/new request without resume", func(t *testing.T) {
		t.Parallel()
		b := newAcpTestBackend(t, "goose")
		cap := newAcpCaptureStdin()
		ch := make(chan Message, 256)

		ndjson := strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"1"}}`,
			`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"fresh-sess"}}`,
			`{"jsonrpc":"2.0","method":"session/finished","params":{"sessionId":"fresh-sess"}}`,
		}, "\n")

		res := b.processAcpEvents(strings.NewReader(ndjson), ch, cap.asWriter(), acpNoopCloseStdin, "hello", "", "test-model")
		drainMessages(ch)

		reqs := cap.requests(t)
		if len(reqs) != 3 {
			t.Fatalf("expected 3 requests (init, new, prompt), got %d: %+v", len(reqs), reqs)
		}
		if reqs[1].Method != "session/new" {
			t.Errorf("request[1].method: got %q, want session/new", reqs[1].Method)
		}
		if res.sessionID != "fresh-sess" {
			t.Errorf("result.sessionID: got %q, want fresh-sess", res.sessionID)
		}
	})
}

// marshalJSON re-marshals an any-typed field value so it can be decoded
// into a typed struct in tests. jsonrpcRequest.Params is `any`, so the
// decoded value is a generic map; re-marshalling yields RawMessage-safe
// bytes.
func marshalJSON(t *testing.T, v any) []byte {
	t.Helper()
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalJSON: %v", err)
	}
	return b
}

// ── TestAcpParseEvents ──
//
// Feeds synthetic JSON-RPC stdout through processAcpEvents and asserts the
// resulting Message sequence, accumulated output, session id, token usage,
// and terminal status across the four event flows the daemon cares about:
// direct-shape session/message stream, nested Claude-style envelope,
// session/error, and a handshake response error. No real CLI process is
// invoked.

func TestAcpParseEvents(t *testing.T) {
	t.Parallel()

	t.Run("direct shape: text + thinking + tool_use + tool_result + finished", func(t *testing.T) {
		t.Parallel()
		b := newAcpTestBackend(t, "copilot")
		ch := make(chan Message, 256)
		cap := newAcpCaptureStdin()

		ndjson := strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"1"}}`,
			`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"acp-sess-1"}}`,
			`{"jsonrpc":"2.0","method":"session/message","params":{"type":"text","content":"Analyzing "}}`,
			`{"jsonrpc":"2.0","method":"session/message","params":{"type":"thinking","content":"let me think"}}`,
			`{"jsonrpc":"2.0","method":"session/message","params":{"type":"tool_use","tool":"Read","id":"call_1","input":{"file_path":"/tmp/foo"}}}`,
			`{"jsonrpc":"2.0","method":"session/message","params":{"type":"tool_result","toolUseId":"call_1","output":"file contents here"}}`,
			`{"jsonrpc":"2.0","method":"session/message","params":{"type":"text","content":"Done."}}`,
			`{"jsonrpc":"2.0","method":"session/finished","params":{"sessionId":"acp-sess-1","usage":{"inputTokens":150,"outputTokens":30}}}`,
		}, "\n")

		res := b.processAcpEvents(strings.NewReader(ndjson), ch, cap.asWriter(), acpNoopCloseStdin, "do it", "", "test-model")
		msgs := drainMessages(ch)

		if res.status != "completed" {
			t.Errorf("status: got %q, want completed", res.status)
		}
		if res.sessionID != "acp-sess-1" {
			t.Errorf("sessionID: got %q, want acp-sess-1", res.sessionID)
		}
		// ACP has no result frame; accumulated text is the concatenation of
		// the text deltas (no authoritative replacement).
		if res.output != "Analyzing Done." {
			t.Errorf("output: got %q, want %q", res.output, "Analyzing Done.")
		}

		// Expected: status(running after session/new), text, thinking,
		// tool-use, tool-result, text = 6 messages.
		if len(msgs) != 6 {
			t.Fatalf("expected 6 messages, got %d: %+v", len(msgs), msgs)
		}
		if msgs[0].Type != MessageStatus || msgs[0].Status != "running" || msgs[0].SessionID != "acp-sess-1" {
			t.Errorf("msg[0]: got %+v, want status=running session=acp-sess-1", msgs[0])
		}
		if msgs[1].Type != MessageText || msgs[1].Content != "Analyzing " {
			t.Errorf("msg[1]: got %+v, want text 'Analyzing '", msgs[1])
		}
		if msgs[2].Type != MessageThinking || msgs[2].Content != "let me think" {
			t.Errorf("msg[2]: got %+v, want thinking 'let me think'", msgs[2])
		}
		if msgs[3].Type != MessageToolUse || msgs[3].Tool != "Read" || msgs[3].CallID != "call_1" {
			t.Errorf("msg[3]: got %+v, want tool-use Read/call_1", msgs[3])
		}
		if got := msgs[3].Input["file_path"]; got != "/tmp/foo" {
			t.Errorf("msg[3].Input.file_path: got %v, want /tmp/foo", got)
		}
		if msgs[4].Type != MessageToolResult || msgs[4].CallID != "call_1" {
			t.Errorf("msg[4]: got %+v, want tool-result call_1", msgs[4])
		}
		if msgs[4].Output != "file contents here" {
			t.Errorf("msg[4].Output: got %q, want 'file contents here' (unquoted)", msgs[4].Output)
		}
		if msgs[5].Type != MessageText || msgs[5].Content != "Done." {
			t.Errorf("msg[5]: got %+v, want text 'Done.'", msgs[5])
		}

		// Usage: flat shape keyed by fallbackModel ("test-model"). The
		// per-model attempt fails (values are numbers, not objects) and the
		// flat block wins.
		if res.usage == nil {
			t.Fatalf("expected per-model usage map, got nil")
		}
		u, ok := res.usage["test-model"]
		if !ok {
			t.Fatalf("expected usage keyed by test-model, got %v", res.usage)
		}
		if u.InputTokens != 150 || u.OutputTokens != 30 {
			t.Errorf("usage: got input=%d output=%d, want 150/30", u.InputTokens, u.OutputTokens)
		}

		// The handshake must have written exactly three requests: initialize,
		// session/new, session/prompt — in that order.
		reqs := cap.requests(t)
		if len(reqs) != 3 {
			t.Fatalf("expected 3 stdin requests, got %d: %+v", len(reqs), reqs)
		}
		if reqs[0].Method != "initialize" || reqs[1].Method != "session/new" || reqs[2].Method != "session/prompt" {
			t.Errorf("request order: got %v, want initialize/session/new/session/prompt",
				[]string{reqs[0].Method, reqs[1].Method, reqs[2].Method})
		}
	})

	t.Run("nested envelope: message.content[] blocks", func(t *testing.T) {
		t.Parallel()
		b := newAcpTestBackend(t, "cursor")
		ch := make(chan Message, 256)
		cap := newAcpCaptureStdin()

		ndjson := strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"1"}}`,
			`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"n-sess"}}`,
			`{"jsonrpc":"2.0","method":"session/message","params":{"sessionId":"n-sess","message":{"role":"assistant","content":[{"type":"text","text":"hello "},{"type":"tool_use","id":"c1","name":"Bash","input":{"cmd":"ls"}},{"type":"tool_result","tool_use_id":"c1","content":"listed"}]}}}`,
			`{"jsonrpc":"2.0","method":"session/finished","params":{"sessionId":"n-sess","usage":{"nested-model":{"inputTokens":5,"outputTokens":2}}}}`,
		}, "\n")

		res := b.processAcpEvents(strings.NewReader(ndjson), ch, cap.asWriter(), acpNoopCloseStdin, "hi", "", "fallback")
		msgs := drainMessages(ch)

		if res.status != "completed" {
			t.Errorf("status: got %q, want completed", res.status)
		}
		if res.sessionID != "n-sess" {
			t.Errorf("sessionID: got %q, want n-sess", res.sessionID)
		}
		if res.output != "hello " {
			t.Errorf("output: got %q, want 'hello '", res.output)
		}
		// Expected: status, text, tool-use, tool-result = 4 messages.
		if len(msgs) != 4 {
			t.Fatalf("expected 4 messages, got %d: %+v", len(msgs), msgs)
		}
		if msgs[1].Type != MessageText || msgs[1].Content != "hello " {
			t.Errorf("msg[1]: got %+v, want text 'hello '", msgs[1])
		}
		if msgs[2].Type != MessageToolUse || msgs[2].Tool != "Bash" || msgs[2].CallID != "c1" {
			t.Errorf("msg[2]: got %+v, want tool-use Bash/c1", msgs[2])
		}
		if msgs[3].Type != MessageToolResult || msgs[3].CallID != "c1" || msgs[3].Output != "listed" {
			t.Errorf("msg[3]: got %+v, want tool-result c1/'listed'", msgs[3])
		}
		// Per-model usage map wins over the flat fallback.
		if res.usage == nil {
			t.Fatalf("expected per-model usage, got nil")
		}
		u, ok := res.usage["nested-model"]
		if !ok {
			t.Fatalf("expected usage keyed by nested-model, got %v", res.usage)
		}
		if u.InputTokens != 5 || u.OutputTokens != 2 {
			t.Errorf("usage: got input=%d output=%d, want 5/2", u.InputTokens, u.OutputTokens)
		}
	})

	t.Run("session/error terminates with failure", func(t *testing.T) {
		t.Parallel()
		b := newAcpTestBackend(t, "hermes")
		ch := make(chan Message, 256)

		ndjson := strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"1"}}`,
			`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"e-sess"}}`,
			`{"jsonrpc":"2.0","method":"session/message","params":{"type":"text","content":"partial "}}`,
			`{"jsonrpc":"2.0","method":"session/error","params":{"sessionId":"e-sess","message":"boom"}}`,
		}, "\n")

		res := b.processAcpEvents(strings.NewReader(ndjson), ch, newAcpCaptureStdin().asWriter(), acpNoopCloseStdin, "hi", "", "fallback")
		msgs := drainMessages(ch)

		if res.status != "failed" {
			t.Errorf("status: got %q, want failed", res.status)
		}
		if res.errMsg != "boom" {
			t.Errorf("errMsg: got %q, want boom", res.errMsg)
		}
		if res.output != "partial " {
			t.Errorf("output: got %q, want 'partial '", res.output)
		}
		// Last message must be the error event.
		var last Message
		for _, m := range msgs {
			last = m
		}
		if last.Type != MessageError || last.Content != "boom" {
			t.Errorf("last message: got %+v, want error 'boom'", last)
		}
	})

	t.Run("handshake response error is fatal", func(t *testing.T) {
		t.Parallel()
		b := newAcpTestBackend(t, "auggie")
		ch := make(chan Message, 256)

		ndjson := `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"init failed"}}`

		res := b.processAcpEvents(strings.NewReader(ndjson), ch, newAcpCaptureStdin().asWriter(), acpNoopCloseStdin, "hi", "", "fallback")
		msgs := drainMessages(ch)

		if res.status != "failed" {
			t.Errorf("status: got %q, want failed", res.status)
		}
		if !strings.Contains(res.errMsg, "init failed") {
			t.Errorf("errMsg: got %q, want it to contain 'init failed'", res.errMsg)
		}
		// An error event must have been emitted on the channel.
		found := false
		for _, m := range msgs {
			if m.Type == MessageError && strings.Contains(m.Content, "init failed") {
				found = true
			}
		}
		if !found {
			t.Errorf("expected an error Message for the handshake failure, got %+v", msgs)
		}
	})

	t.Run("non-JSON banner lines are skipped", func(t *testing.T) {
		t.Parallel()
		b := newAcpTestBackend(t, "kiro")
		ch := make(chan Message, 256)

		ndjson := strings.Join([]string{
			`some startup banner`,
			`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"1"}}`,
			`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"b-sess"}}`,
			`{"jsonrpc":"2.0","method":"session/message","params":{"type":"text","content":"hi"}}`,
			`{"jsonrpc":"2.0","method":"session/finished","params":{"sessionId":"b-sess"}}`,
		}, "\n")

		res := b.processAcpEvents(strings.NewReader(ndjson), ch, newAcpCaptureStdin().asWriter(), acpNoopCloseStdin, "hi", "", "fallback")
		msgs := drainMessages(ch)

		if res.status != "completed" {
			t.Errorf("status: got %q, want completed", res.status)
		}
		if res.output != "hi" {
			t.Errorf("output: got %q, want hi", res.output)
		}
		if res.sessionID != "b-sess" {
			t.Errorf("sessionID: got %q, want b-sess", res.sessionID)
		}
		_ = msgs
	})
}

// ── TestAcpMissingExecutable ──
//
// Execute must surface a clear error when the CLI binary cannot be found
// (exec.LookPath fails), rather than spawning a process that immediately
// exits with an opaque status. Uses a deliberately nonexistent
// ExecutablePath so the result is deterministic regardless of which CLIs
// happen to be installed on the host.

func TestAcpMissingExecutable(t *testing.T) {
	t.Parallel()

	// copilot is a valid backend name; the missing binary is simulated via
	// ExecutablePath so we exercise the real LookPath branch in Execute.
	b := &acpBackend{
		name: "copilot",
		cfg: Config{
			ExecutablePath: "/nonexistent/path/to/copilot",
			Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}

	// LookPath treats a path with a separator as absolute and checks
	// executability; a missing file yields an error before any goroutine
	// starts, so a short context is sufficient.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := b.Execute(ctx, "hi", ExecOptions{Timeout: 5 * time.Second})
		if err == nil {
			t.Errorf("Execute: expected error for missing executable, got nil")
			return
		}
		if !strings.Contains(err.Error(), "acp executable not found") {
			t.Errorf("Execute error: got %q, want it to contain 'acp executable not found'", err.Error())
		}
		// Also assert the missing-file semantics surfaced. LookPath on an
		// absolute path that does not exist returns a stat error wrapping
		// os.ErrNotExist (NOT exec.ErrNotFound, which is reserved for PATH
		// search misses of relative names); Execute re-wraps it with %w so
		// errors.Is still unwraps to the sentinel.
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Execute error: got %q, want it to wrap os.ErrNotExist", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("Execute did not return within 10s for a missing executable")
	}
}
