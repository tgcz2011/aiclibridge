package adapter

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// ── CheckCLIVersion ──

// TestCheckCLIVersionDefaultArgs drives the test binary itself as the
// "CLI" via the helper-process pattern (see TestHelperProcessVersion).
// The probe runs `<test binary> -test.run=TestHelperProcessVersion`; the
// re-exec'd binary runs only the helper, which prints a version line and
// exits 0. CheckCLIVersion should return that line (trimmed) with no
// error, proving the binary is invoked with the supplied args and its
// combined output is returned.
func TestCheckCLIVersionDefaultArgs(t *testing.T) {
	t.Setenv("GO_AICLIBRIDGE_HELPER_PROCESS", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	got, err := CheckCLIVersion(ctx, os.Args[0], "-test.run=TestHelperProcessVersion")
	if err != nil {
		t.Fatalf("CheckCLIVersion: %v", err)
	}
	if !strings.Contains(got, "1.2.3") {
		t.Errorf("CheckCLIVersion output: got %q, want it to contain %q", got, "1.2.3")
	}
}

// TestCheckCLIVersionCustomArgs confirms caller-supplied args reach the
// binary verbatim: in echo mode the helper prints every arg it received,
// so a positional sentinel arg must appear in the output. A positional
// arg (not starting with "-") is used so the test binary's own flag
// parser accepts it rather than rejecting an unknown --flag.
func TestCheckCLIVersionCustomArgs(t *testing.T) {
	t.Setenv("GO_AICLIBRIDGE_HELPER_PROCESS", "1")
	t.Setenv("GO_AICLIBRIDGE_HELPER_MODE", "echo")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	got, err := CheckCLIVersion(ctx, os.Args[0], "-test.run=TestHelperProcessVersion", "sentinel-arg")
	if err != nil {
		t.Fatalf("CheckCLIVersion: %v", err)
	}
	if !strings.Contains(got, "sentinel-arg") {
		t.Errorf("expected echoed sentinel-arg in output, got %q", got)
	}
}

// TestCheckCLIVersionMissingBinary covers the error path: a binary that
// does not exist returns ("", err). Callers are expected to log a warning
// rather than fail — the helper's contract is best-effort.
func TestCheckCLIVersionMissingBinary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := CheckCLIVersion(ctx, "/no/such/binary/anywhere/aiclibridge")
	if err == nil {
		t.Fatal("expected error for missing binary, got nil")
	}
	if got != "" {
		t.Errorf("expected empty version on error, got %q", got)
	}
}

// TestCheckCLIVersionTimeout covers the context-deadline path: the helper
// sleeps well past the deadline, so the probe must error out instead of
// hanging the caller.
func TestCheckCLIVersionTimeout(t *testing.T) {
	t.Setenv("GO_AICLIBRIDGE_HELPER_PROCESS", "1")
	t.Setenv("GO_AICLIBRIDGE_HELPER_MODE", "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := CheckCLIVersion(ctx, os.Args[0], "-test.run=TestHelperProcessVersion")
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

// TestHelperProcessVersion is the helper-process entry point re-executed
// by the CheckCLIVersion tests above. It is a normal (no-op) test when
// run by `go test` (GO_AICLIBRIDGE_HELPER_PROCESS unset → return). When
// the env var is set it behaves like a CLI --version probe and exits 0.
// GO_AICLIBRIDGE_HELPER_MODE selects the behaviour: default prints
// "1.2.3", "echo" echoes the trailing args, "sleep" blocks to exercise
// the context timeout.
func TestHelperProcessVersion(t *testing.T) {
	if os.Getenv("GO_AICLIBRIDGE_HELPER_PROCESS") != "1" {
		return
	}
	switch os.Getenv("GO_AICLIBRIDGE_HELPER_MODE") {
	case "echo":
		// Echo every arg after the test-binary path so the caller can
		// confirm its args reached the binary verbatim.
		fmt.Println(strings.Join(os.Args[1:], " "))
	case "sleep":
		time.Sleep(30 * time.Second)
		fmt.Println("should-not-reach")
	default:
		fmt.Println("1.2.3")
	}
	os.Exit(0)
}

// ── WarnOnVersion ──

func TestWarnOnVersion(t *testing.T) {
	tests := []struct {
		name       string
		got        string
		min        string
		wantLevel  string // "warn", "info", or "none"
		wantSubstr string
	}{
		{name: "empty got is no-op", got: "", min: "1.0.0", wantLevel: "none"},
		{name: "below min warns", got: "1.0.0", min: "2.0.0", wantLevel: "warn", wantSubstr: "below minimum"},
		{name: "equal min no warn", got: "2.0.0", min: "2.0.0", wantLevel: "none"},
		{name: "above min no warn", got: "3.1.0", min: "3.0.0", wantLevel: "none"},
		{name: "shorter equal (2.1 == 2.1.0)", got: "2.1", min: "2.1.0", wantLevel: "none"},
		{name: "shorter below (2.0 < 2.0.1)", got: "2.0", min: "2.0.1", wantLevel: "warn", wantSubstr: "below minimum"},
		{name: "longer above (2.1.0.0 > 2.1)", got: "2.1.0.0", min: "2.1", wantLevel: "none"},
		{name: "unparseable got logs info", got: "1.2.3-beta", min: "1.0.0", wantLevel: "info", wantSubstr: "could not parse"},
		{name: "unparseable min logs info", got: "1.0.0", min: "x.y.z", wantLevel: "info", wantSubstr: "could not parse"},
		{name: "non-numeric component logs info", got: "1.2.x", min: "1.0.0", wantLevel: "info", wantSubstr: "could not parse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			WarnOnVersion(logger, "testcli", tt.got, tt.min)
			out := buf.String()
			switch tt.wantLevel {
			case "none":
				if out != "" {
					t.Errorf("expected no log output, got %q", out)
				}
			case "warn":
				if !strings.Contains(out, "level=WARN") {
					t.Errorf("expected WARN level, got %q", out)
				}
			case "info":
				if !strings.Contains(out, "level=INFO") {
					t.Errorf("expected INFO level, got %q", out)
				}
			}
			if tt.wantSubstr != "" && !strings.Contains(out, tt.wantSubstr) {
				t.Errorf("expected %q in output, got %q", tt.wantSubstr, out)
			}
		})
	}
}

// TestCompareDotVersions is a focused unit test on the comparison helper
// underpinning WarnOnVersion, covering the ordering and parse-failure
// contract directly (WarnOnVersion only exposes the warn/info/none
// outcome, not the -1/0/+1 result).
func TestCompareDotVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int  // -1, 0, 1
		ok   bool // parse success
	}{
		{"1.0.0", "2.0.0", -1, true},
		{"2.0.0", "1.0.0", 1, true},
		{"1.2.3", "1.2.3", 0, true},
		{"2.1", "2.1.0", 0, true},
		{"2.1.0", "2.1", 0, true},
		{"2.0", "2.0.1", -1, true},
		{"1.10.0", "1.9.0", 1, true}, // numeric, not lexical
		{"1.2.3-beta", "1.0.0", 0, false},
		{"1.0.0", "x.y.z", 0, false},
		{"", "1.0.0", 0, false},
		{"1", "1.0.0", 0, true},
	}
	for _, tt := range tests {
		got, ok := compareDotVersions(tt.a, tt.b)
		if ok != tt.ok {
			t.Errorf("compareDotVersions(%q,%q) ok: got %v, want %v", tt.a, tt.b, ok, tt.ok)
			continue
		}
		if ok && got != tt.want {
			t.Errorf("compareDotVersions(%q,%q): got %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
