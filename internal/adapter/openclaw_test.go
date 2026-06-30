package adapter

import (
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
)

// indexOf returns the first index of s in args, or -1 if absent.
func indexOf(args []string, s string) int {
	for i, a := range args {
		if a == s {
			return i
		}
	}
	return -1
}

func countOccurrences(args []string, s string) int {
	n := 0
	for _, a := range args {
		if a == s {
			n++
		}
	}
	return n
}

// TestOpenclawParseFixture feeds the recorded openclaw 2026.5.5 stdout
// fixture through processOutput and asserts the parsed result + the
// streamed Message sequence. This is the regression test for the
// whole-buffer fast path: the fixture is 1000+ lines of pretty-printed
// JSON, exactly the shape the line-by-line scanner used to fail on under
// partial / chunked reads.
func TestOpenclawParseFixture(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/openclaw-2026.5.5-stdout.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if len(data) < 1000 {
		t.Fatalf("fixture too small (%d bytes); did the file get truncated?", len(data))
	}
	if !strings.Contains(string(data), "\n  ") {
		t.Fatal("fixture is not pretty-printed; this test must run against multi-line JSON")
	}

	b := &openclawBackend{cfg: Config{Logger: slog.Default()}}
	ch := make(chan Message, 256)

	res := b.processOutput(strings.NewReader(string(data)), ch)

	if res.status != "completed" {
		t.Errorf("status: got %q, want %q", res.status, "completed")
	}
	if res.errMsg != "" {
		t.Errorf("errMsg: got %q, want empty", res.errMsg)
	}
	if res.output != "hi" {
		t.Errorf("output: got %q, want %q", res.output, "hi")
	}
	if res.sessionID == "" {
		t.Error("sessionID: got empty, want non-empty")
	}
	if res.model != "anthropic/claude-opus-4.7" {
		t.Errorf("model: got %q, want %q", res.model, "anthropic/claude-opus-4.7")
	}
	if res.usage.InputTokens != 34620 {
		t.Errorf("usage.InputTokens: got %d, want %d", res.usage.InputTokens, 34620)
	}
	if res.usage.OutputTokens != 6 {
		t.Errorf("usage.OutputTokens: got %d, want %d", res.usage.OutputTokens, 6)
	}
	if res.usage.CacheWriteTokens != 46482 {
		t.Errorf("usage.CacheWriteTokens: got %d, want %d", res.usage.CacheWriteTokens, 46482)
	}

	close(ch)

	var msgs []Message
	var gotText bool
	for msg := range ch {
		msgs = append(msgs, msg)
		if msg.Type == MessageText && strings.Contains(msg.Content, "hi") {
			gotText = true
		}
	}
	if !gotText {
		t.Errorf("expected a MessageText event containing %q, got %d messages", "hi", len(msgs))
	}
	if len(msgs) == 0 {
		t.Error("expected at least one message, got 0")
	}
}

// TestOpenclawMode exercises the openclaw_mode routing on buildOpenclawArgs:
// "local" (and "" default) keep --local; "gateway" drops it. Daemon-managed
// flags survive both modes.
func TestOpenclawMode(t *testing.T) {
	t.Parallel()

	// "local" and "" both keep --local so existing agents do not silently
	// change routing.
	for _, mode := range []string{"", "local"} {
		args := buildOpenclawArgs("do work", "ses-local", ExecOptions{
			OpenclawMode: mode,
		}, slog.Default())
		if idx := indexOf(args, "--local"); idx == -1 {
			t.Errorf("mode=%q: expected --local in args, got %v", mode, args)
		}
		for _, want := range []string{"agent", "--json", "--session-id", "--message"} {
			if indexOf(args, want) == -1 {
				t.Errorf("mode=%q: missing daemon-managed flag %q in %v", mode, want, args)
			}
		}
	}

	// "gateway" must drop --local.
	args := buildOpenclawArgs("do work", "ses-gw", ExecOptions{
		OpenclawMode: "gateway",
	}, slog.Default())
	if idx := indexOf(args, "--local"); idx != -1 {
		t.Errorf("gateway mode must not append --local, got %v", args)
	}
	for _, want := range []string{"agent", "--json", "--session-id", "--message"} {
		if indexOf(args, want) == -1 {
			t.Errorf("gateway mode dropped daemon-managed flag %q: %v", want, args)
		}
	}
}

// TestOpenclawDiscovery exercises the JSON parser path of
// DiscoverOpenclawAgents: a captured `openclaw agents list --json` blob is
// fed directly through parseOpenclawAgentsJSON (the function the
// discoverer tries first). It must produce a stable list of Models
// with the expected id/label/provider fields.
func TestOpenclawDiscovery(t *testing.T) {
	t.Parallel()

	// Top-level array shape (the most common form across openclaw
	// builds). Tests ID routing key, optional model enrichment in the
	// dropdown label, and dedup on duplicate ids.
	jsonArray := `[
		{"id":"main","name":"Main","model":"anthropic/claude-opus-4.7"},
		{"id":"research","name":"Research Bot","model":"deepseek-chat"},
		{"id":"dup","name":"first","model":"m1"},
		{"id":"dup","name":"second","model":"m2"}
	]`
	models, ok := parseOpenclawAgentsJSON([]byte(jsonArray))
	if !ok {
		t.Fatal("parseOpenclawAgentsJSON: top-level array shape rejected")
	}
	if len(models) != 3 {
		t.Fatalf("expected 3 models (duplicate 'dup' dropped), got %d: %+v", len(models), models)
	}
	if models[0].ID != "main" || models[0].Provider != "openclaw" {
		t.Errorf("model[0]: got %+v, want id=main provider=openclaw", models[0])
	}
	if !strings.Contains(models[0].Label, "anthropic/claude-opus-4.7") {
		t.Errorf("model[0].Label = %q, want it to include the model", models[0].Label)
	}
	if models[1].Label != "Research Bot (deepseek-chat)" {
		t.Errorf("model[1].Label = %q, want %q", models[1].Label, "Research Bot (deepseek-chat)")
	}
	if models[2].ID != "dup" {
		t.Errorf("model[2].ID = %q, want %q (first wins)", models[2].ID, "dup")
	}
	if models[2].Label != "first (m1)" {
		t.Errorf("model[2].Label = %q, want %q", models[2].Label, "first (m1)")
	}

	// Wrapped shape `{ "agents": [...] }` — also supported.
	wrapped := `{"agents":[{"id":"a1","name":"Agent One","model":"m1"}]}`
	models2, ok := parseOpenclawAgentsJSON([]byte(wrapped))
	if !ok {
		t.Fatal("parseOpenclawAgentsJSON: wrapped shape rejected")
	}
	if len(models2) != 1 || models2[0].ID != "a1" || models2[0].Provider != "openclaw" {
		t.Errorf("wrapped models: %+v", models2)
	}

	// Garbage input returns ok=false rather than an error — the caller
	// (DiscoverOpenclawAgents) falls through to the text parser.
	if _, ok := parseOpenclawAgentsJSON([]byte("not json")); ok {
		t.Error("parseOpenclawAgentsJSON: garbage input should return ok=false")
	}
	if _, ok := parseOpenclawAgentsJSON([]byte("")); ok {
		t.Error("parseOpenclawAgentsJSON: empty input should return ok=false")
	}
}

// TestOpenclawBlockedArgs verifies that protocol-critical flags in
// custom_args are stripped by openclawBlockedArgs. Runs in gateway mode
// so the daemon's hardcoded --local is absent — otherwise its presence
// would mask whether the user's --local was filtered.
func TestOpenclawBlockedArgs(t *testing.T) {
	t.Parallel()

	args := buildOpenclawArgs("task", "ses-1", ExecOptions{
		OpenclawMode: "gateway",
		CustomArgs: []string{
			"--local",
			"--json",
			"--session-id", "hijacked",
			"--message", "hijacked",
			"--model", "gpt-4o",
			"--system-prompt", "You are helpful",
			"--agent", "research-bot",
		},
	}, slog.Default())

	// In gateway mode the daemon only appends --json, --session-id,
	// --message itself. The user's --local, --model, --system-prompt
	// must be filtered.
	for _, blocked := range []string{"--local", "--model", "--system-prompt"} {
		if indexOf(args, blocked) != -1 {
			t.Errorf("%s should be filtered from custom_args, got %v", blocked, args)
		}
	}
	// --json must appear exactly once — the daemon-managed value,
	// not a custom_args copy.
	if n := countOccurrences(args, "--json"); n != 1 {
		t.Errorf("expected 1 --json (daemon-managed), got %d: %v", n, args)
	}
	// --session-id and --message must each appear exactly once — the
	// daemon-managed value, not the custom_args copy.
	if n := countOccurrences(args, "--session-id"); n != 1 {
		t.Errorf("expected 1 --session-id (daemon-managed), got %d: %v", n, args)
	}
	if n := countOccurrences(args, "--message"); n != 1 {
		t.Errorf("expected 1 --message (daemon-managed), got %d: %v", n, args)
	}
	// Whitelisted pass-through flag must survive filtering.
	agentIdx := indexOf(args, "--agent")
	if agentIdx == -1 || agentIdx+1 >= len(args) || args[agentIdx+1] != "research-bot" {
		t.Errorf("expected --agent research-bot to survive filtering, got %v", args)
	}
}

// TestOpenclawGatewayModeFiltersLocalFromCustomArgs pins that the
// openclaw_mode field is the single source of truth for local/gateway
// routing. A user trying to re-introduce --local via custom_args under
// gateway mode is expressing contradictory intent; the blocked-args
// filter wins so the run actually reaches the gateway as configured.
func TestOpenclawGatewayModeFiltersLocalFromCustomArgs(t *testing.T) {
	t.Parallel()

	args := buildOpenclawArgs("do work", "ses-mix", ExecOptions{
		OpenclawMode: "gateway",
		CustomArgs:   []string{"--local"},
	}, slog.Default())

	if indexOf(args, "--local") != -1 {
		t.Errorf("gateway mode must filter custom_args --local, got %v", args)
	}
}

// TestOpenclawMissingExecutable verifies that Execute returns a clean
// error when the configured openclaw binary cannot be found on PATH.
// This is the pre-spawn failure path: LookPath runs before any subprocess
// is started, so a missing binary surfaces a descriptive error rather
// than a half-started Session.
func TestOpenclawMissingExecutable(t *testing.T) {
	t.Parallel()

	b := &openclawBackend{cfg: Config{
		ExecutablePath: "/nonexistent/aiclibridge-openclaw-fixture",
		Logger:         slog.Default(),
	}}
	_, err := b.Execute(t.Context(), "do work", ExecOptions{})
	if err == nil {
		t.Fatal("Execute: expected error for missing executable, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "openclaw executable not found") {
		t.Errorf("Execute error should mention 'openclaw executable not found', got %q", msg)
	}
	if !strings.Contains(msg, "/nonexistent/aiclibridge-openclaw-fixture") {
		t.Errorf("Execute error should name the missing path, got %q", msg)
	}
}

// TestOpenclawCustomArgsContains covers the helper that buildOpenclawArgs
// uses to decide whether to inject --agent <opts.Model>. The function
// must recognise both the standalone "--agent" token and the
// "--agent=value" form so a user-configured custom_args wins either way.
func TestOpenclawCustomArgsContains(t *testing.T) {
	t.Parallel()

	if !customArgsContains([]string{"--agent", "foo"}, "--agent") {
		t.Error("standalone --agent should match")
	}
	if !customArgsContains([]string{"--agent=foo"}, "--agent") {
		t.Error("--agent=value form should match")
	}
	if customArgsContains([]string{"--agents", "foo"}, "--agent") {
		t.Error("--agents must not match --agent")
	}
	if customArgsContains([]string{}, "--agent") {
		t.Error("empty args must not match")
	}
}

// TestIsOpenclawIdentifier pins the strict identifier rule used by
// parseOpenclawAgents: starts with a letter, identifier-safe chars only,
// no trailing colon. TUI decoration like `│`, `╭`, `◇`, `|` must be
// rejected so we never surface "Identity:" as a selectable agent.
func TestIsOpenclawIdentifier(t *testing.T) {
	t.Parallel()

	good := []string{"main", "research-bot", "deepseek_chat", "anthropic/claude-opus-4.7", "model.v1"}
	for _, s := range good {
		if !isOpenclawIdentifier(s) {
			t.Errorf("isOpenclawIdentifier(%q) = false, want true", s)
		}
	}
	bad := []string{"", "1starts-with-digit", "Identity:", "│", "╭", "◇", "has space", "semicolon;"}
	for _, s := range bad {
		if isOpenclawIdentifier(s) {
			t.Errorf("isOpenclawIdentifier(%q) = true, want false", s)
		}
	}
}

// TestParseOpenclawAgents_Text exercises the text fallback parser. The
// default `openclaw agents list` output is a decorated banner with
// section headers — only lines that look like a real `<name> <model>`
// row should be surfaced, and the result must not contain section
// headers like "Identity:" or TUI decoration.
func TestParseOpenclawAgents_Text(t *testing.T) {
	t.Parallel()

	input := `Identity:
╭─────────────────────────────────────╮
│  agents                             │
╰─────────────────────────────────────╯

main anthropic/claude-opus-4.7
research deepseek-chat
also-ok claude-sonnet-4
✗ broken
`
	got := parseOpenclawAgents(input)
	want := []string{"main", "research", "also-ok"}
	if len(got) != len(want) {
		t.Fatalf("got %d models, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("model[%d].ID = %q, want %q", i, got[i].ID, w)
		}
		if got[i].Provider != "openclaw" {
			t.Errorf("model[%d].Provider = %q, want openclaw", i, got[i].Provider)
		}
	}
	// Sanity: the broken "✗ broken" line and "Identity:" header must
	// not be surfaced.
	ids := []string{}
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	if slices.Contains(ids, "broken") || slices.Contains(ids, "Identity:") {
		t.Errorf("text parser surfaced decoration/header: %v", ids)
	}
}
