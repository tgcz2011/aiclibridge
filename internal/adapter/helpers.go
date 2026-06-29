package adapter

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ── Environment helpers ──

func buildEnv(extra map[string]string) []string {
	return mergeEnv(os.Environ(), extra)
}

func mergeEnv(base []string, extra map[string]string) []string {
	env := make([]string, 0, len(base)+len(extra))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if isFilteredChildEnvKey(key) {
			continue
		}
		env = append(env, entry)
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// isFilteredChildEnvKey reports whether an inherited env var is an internal
// Claude Code runtime/session marker that must NOT leak into the spawned child
// (otherwise the child mistakes itself for a nested or resumed session, or
// inherits the parent's exec path / transport).
//
// It must NOT strip the user-facing CLAUDE_CODE_* configuration namespace
// (CLAUDE_CODE_GIT_BASH_PATH, CLAUDE_CODE_USE_BEDROCK, CLAUDE_CODE_USE_VERTEX,
// CLAUDE_CODE_MAX_OUTPUT_TOKENS, CLAUDE_CODE_TMPDIR, ...): users set those
// deliberately and the child needs them. Blanket-stripping the whole prefix is
// what broke Windows — CLAUDE_CODE_GIT_BASH_PATH was silently removed, so Claude
// Code could not find bash.exe and exited immediately. Strip internal markers by
// exact name and let every other CLAUDE_CODE_* var through.
//
// The denylist holds only undocumented, per-process runtime markers. Anything in
// the public env-vars reference (https://code.claude.com/docs/en/env-vars) is
// user config and stays out of this list — including CLAUDE_CODE_TMPDIR, a
// documented temp-dir override under which Claude Code creates its own
// per-session subdir, so inheriting it is harmless.
func isFilteredChildEnvKey(key string) bool {
	switch key {
	case "CLAUDECODE", // "1" when running inside Claude Code
		"CLAUDE_CODE_ENTRYPOINT", // entrypoint marker (cli/sdk-cli/...)
		"CLAUDE_CODE_EXECPATH",   // path to the running CLI binary
		"CLAUDE_CODE_SESSION_ID", // per-session identifier
		"CLAUDE_CODE_SSE_PORT":   // IDE-extension transport port
		return true
	}
	// CLAUDECODE_* (no underscore between CLAUDE and CODE) is wholly internal;
	// keep stripping it. The user-facing config namespace is CLAUDE_CODE_*.
	return strings.HasPrefix(key, "CLAUDECODE_")
}

// ── Custom-arg filter helpers ──

// blockedArgMode specifies whether a blocked arg takes a value or is standalone.
type blockedArgMode int

const (
	blockedWithValue  blockedArgMode = iota // flag takes a value (next arg or =value)
	blockedStandalone                       // flag is boolean, no value
)

// claudeBlockedArgs are flags hardcoded by the daemon that must not be
// overridden by user-configured custom_args. Overriding these would break
// the daemon↔Claude communication protocol. This set is claude-specific but
// lives in the shared helpers module so filterCustomArgs stays generic and
// each backend can supply its own blocked set.
var claudeBlockedArgs = map[string]blockedArgMode{
	"-p":                blockedStandalone, // non-interactive mode
	"--output-format":   blockedWithValue,  // stream-json protocol
	"--input-format":    blockedWithValue,  // stream-json protocol
	"--permission-mode": blockedWithValue,  // bypassPermissions for autonomous operation
	"--mcp-config":      blockedWithValue,  // set by daemon from agent.mcp_config
	// `--effort` is owned by the per-agent thinking_level picker so a
	// user-supplied custom_arg cannot silently outvote it. The daemon
	// injects --effort only when opts.ThinkingLevel is set; if a user
	// nevertheless writes it in custom_args we drop the duplicate and
	// log a warning rather than letting the CLI receive two conflicting
	// --effort values.
	"--effort": blockedWithValue,
}

// filterCustomArgs removes protocol-critical flags from user-configured custom
// args to prevent breaking daemon↔agent communication. Each backend defines its
// own blocked set (the flags it hardcodes). This is intentionally narrow — we
// only block args that would break the communication protocol, not every
// possible dangerous flag. Workspace members are trusted to configure agents
// sensibly, same as with custom_env.
//
// Shell quoting is stripped from each arg before processing: users commonly
// type custom_args in config fields using shell syntax (e.g.
// --deny-tool='write'). Since the daemon spawns processes directly without a
// shell, those quotes would otherwise be passed literally to the child process,
// which typically rejects them as unrecognised flag values.
func filterCustomArgs(args []string, blocked map[string]blockedArgMode, logger *slog.Logger) []string {
	if len(args) == 0 {
		return args
	}
	filtered := make([]string, 0, len(args))
	skip := false
	for _, raw := range args {
		if skip {
			skip = false
			continue
		}
		arg := unshellQuoteArg(raw)
		flag := arg
		hasInlineValue := false
		if idx := strings.Index(arg, "="); idx > 0 {
			flag = arg[:idx]
			hasInlineValue = true
		}
		mode, isBlocked := blocked[flag]
		if isBlocked {
			logger.Warn("custom_args: blocked protocol-critical flag, skipping", "flag", flag)
			if mode == blockedWithValue && !hasInlineValue {
				// The next arg is the value for this flag — skip it too.
				skip = true
			}
			continue
		}
		filtered = append(filtered, arg)
	}
	return filtered
}

// unshellQuoteArg strips a single layer of shell-style single or double quotes
// from an argument. It handles two forms:
//
//   - --flag='value' or --flag="value" → --flag=value
//   - 'standalone' or "standalone"     → standalone
//
// Only flag-style args (`-x=…`, `--flag=…`) get inline value unquoting. Plain
// assignment syntax like `model="o3"` is left alone because the quotes may be
// semantic for the child process (for example Codex `-c model="o3"`). Only
// matching outer quotes are stripped; no escape processing is done.
func unshellQuoteArg(arg string) string {
	if strings.HasPrefix(arg, "-") {
		if idx := strings.Index(arg, "="); idx > 0 {
			value := arg[idx+1:]
			if unquoted, ok := stripSurroundingQuotes(value); ok {
				return arg[:idx+1] + unquoted
			}
			return arg
		}
	}
	if unquoted, ok := stripSurroundingQuotes(arg); ok {
		return unquoted
	}
	return arg
}

// stripSurroundingQuotes removes a matching outer pair of single or double
// quotes from s and returns (unquoted, true). Returns (s, false) if s does not
// start and end with the same quote character.
func stripSurroundingQuotes(s string) (string, bool) {
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1], true
		}
	}
	return s, false
}

// ── Version detection ──

// detectVersionTimeout bounds a single `<cli> --version` probe. Version
// detection runs inside the daemon's blocking preflight, so a CLI that never
// returns from `--version` would otherwise stall the whole loop and every
// runtime on the host would appear disconnected. A real `--version` returns
// well under this bound even on a cold cache or with Windows AV scanning;
// the timeout exists only to fail a wedged probe fast and in isolation. A
// var (not const) so tests can shrink it without waiting out the real bound.
var detectVersionTimeout = 10 * time.Second

// versionRe matches version strings like "2.1.100", "v2.0.0", or
// "2.1.100 (Claude Code)" — it extracts the first three numeric components.
// Shared by detectCLIVersion / extractVersionLine and any other version
// parsing path; the daemon's <cli> --version output may carry a semver
// either bare or with a vendor suffix, so the regex must accept both.
var versionRe = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)

// detectCLIVersion runs the agent CLI with --version and returns the
// reported version line. Best-effort preflight — call sites that need a
// guaranteed short-lived probe should not rely on this.
func detectCLIVersion(ctx context.Context, execPath string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, detectVersionTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, execPath, "--version")
	hideAgentWindow(cmd)
	// exec.CommandContext only kills the direct child on timeout. A broken CLI
	// (node/bun shim) can leave grandchildren that inherited and still hold our
	// stdout pipe open, and cmd.Output() blocks in Wait() until that pipe
	// closes — defeating the timeout above. WaitDelay forces the pipes shut and
	// reaps shortly after the context fires so this call always returns.
	cmd.WaitDelay = 2 * time.Second
	data, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return extractVersionLine(string(data)), nil
}

// extractVersionLine pulls the version line out of a `<cli> --version`
// capture, discarding leading shell noise. On Windows, npm-installed CLI
// shims emit `chcp` output like `Active code page: 65001` before the real
// version reaches stdout, and the raw concatenation was being persisted as
// the runtime version.
//
// The heuristic: return the first non-empty line that contains a semver-
// shaped token (matches versionRe). Full version strings like
// "2.1.5 (Claude Code)" or "codex-cli 0.118.0" survive unchanged because
// the whole matching line is returned. If no line carries a semver token,
// fall back to the trimmed raw output so unusual version formats aren't
// silently dropped to empty.
func extractVersionLine(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if versionRe.MatchString(line) {
			return line
		}
	}
	return strings.TrimSpace(raw)
}

// ── logWriter ──

// logWriter adapts a *slog.Logger to an io.Writer for capturing subprocess
// stderr / stdout streams at debug level. It trims trailing whitespace so
// each Write emits a single, loggable line; partial writes accumulate
// inside the underlying file object's buffering.
type logWriter struct {
	logger *slog.Logger
	prefix string
}

func newLogWriter(logger *slog.Logger, prefix string) *logWriter {
	return &logWriter{logger: logger, prefix: prefix}
}

func (w *logWriter) Write(p []byte) (int, error) {
	text := strings.TrimSpace(string(p))
	if text != "" {
		w.logger.Debug(w.prefix + text)
	}
	return len(p), nil
}

// ── stderrTail ──

// stderrTailBytes bounds the stderr tail captured for inclusion in
// error messages when an agent CLI exits before emitting a structured
// error (e.g. V8 abort on Windows, Bun panic, OOM). Large enough to
// contain typical CLI error lines, small enough to stay sensible inside
// a task-level Result.Error string.
const stderrTailBytes = 2048

// stderrTail forwards writes to an inner writer (typically the daemon's
// log) while also retaining a bounded tail of the bytes written. Consumers
// call Tail() to include that context in error messages when the agent
// process exits before it emits a structured error — otherwise all the
// user sees is "exit status N", with the real reason stuck in daemon logs.
//
// All backends that supervise a child CLI process should wire their
// cmd.Stderr through this type, and on failure include Tail() in
// Result.Error.
type stderrTail struct {
	inner io.Writer
	max   int

	mu  sync.Mutex
	buf []byte
}

func newStderrTail(inner io.Writer, max int) *stderrTail {
	if max <= 0 {
		max = stderrTailBytes
	}
	return &stderrTail{inner: inner, max: max}
}

func (s *stderrTail) Write(p []byte) (int, error) {
	if _, err := s.inner.Write(p); err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.buf = append(s.buf, p...)
	if len(s.buf) > s.max {
		s.buf = s.buf[len(s.buf)-s.max:]
	}
	s.mu.Unlock()
	return len(p), nil
}

// Tail returns the captured stderr with leading/trailing whitespace
// trimmed; empty string means nothing was written or everything was
// whitespace.
func (s *stderrTail) Tail() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(string(s.buf))
}
