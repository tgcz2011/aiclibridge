package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	c := Defaults()
	if c.Listen != "127.0.0.1:8787" {
		t.Errorf("Listen: got %q, want 127.0.0.1:8787", c.Listen)
	}
	if c.DataDir != "./data" {
		t.Errorf("DataDir: got %q, want ./data", c.DataDir)
	}
	if c.LogLevel != "info" {
		t.Errorf("LogLevel: got %q, want info", c.LogLevel)
	}
	if c.DefaultTimeoutMs != 0 {
		t.Errorf("DefaultTimeoutMs: got %d, want 0", c.DefaultTimeoutMs)
	}
	if c.APIKey != "" {
		t.Errorf("APIKey: got %q, want empty", c.APIKey)
	}
	if len(c.Agents) != len(KnownAgents) {
		t.Fatalf("Agents: got %d entries, want %d", len(c.Agents), len(KnownAgents))
	}
	for _, name := range KnownAgents {
		a, ok := c.Agents[name]
		if !ok {
			t.Errorf("agent %q missing from Agents", name)
			continue
		}
		if !a.Enabled {
			t.Errorf("agent %q Enabled: got false, want true", name)
		}
		if a.ExecutablePath != "" {
			t.Errorf("agent %q ExecutablePath: got %q, want empty", name, a.ExecutablePath)
		}
		if a.MCPConfig.Raw() != nil {
			t.Errorf("agent %q MCPConfig: got %v, want nil", name, a.MCPConfig.Raw())
		}
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aiclibridge.yaml")
	content := []byte(`listen: 0.0.0.0:9999
api_key: "sk-test-123"
data_dir: /var/lib/aiclibridge
log_level: debug
default_timeout_ms: 30000
agents:
  claude:
    enabled: false
    executable_path: /usr/local/bin/claude
    extra_args: ["--no-update"]
    custom_args: ["--dangerously-skip-permissions"]
    env:
      ANTHROPIC_API_KEY: sk-ant-xxx
    thinking_level: high
  openclaw:
    enabled: true
    openclaw_mode: gateway
    thinking_level: medium
`)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != "0.0.0.0:9999" {
		t.Errorf("Listen: got %q, want 0.0.0.0:9999", c.Listen)
	}
	if c.APIKey != "sk-test-123" {
		t.Errorf("APIKey: got %q, want sk-test-123", c.APIKey)
	}
	if c.DataDir != "/var/lib/aiclibridge" {
		t.Errorf("DataDir: got %q, want /var/lib/aiclibridge", c.DataDir)
	}
	if c.LogLevel != "debug" {
		t.Errorf("LogLevel: got %q, want debug", c.LogLevel)
	}
	if c.DefaultTimeoutMs != 30000 {
		t.Errorf("DefaultTimeoutMs: got %d, want 30000", c.DefaultTimeoutMs)
	}
	claude := c.Agents["claude"]
	if claude.Enabled {
		t.Errorf("claude.Enabled: got true, want false")
	}
	if claude.ExecutablePath != "/usr/local/bin/claude" {
		t.Errorf("claude.ExecutablePath: got %q, want /usr/local/bin/claude", claude.ExecutablePath)
	}
	if len(claude.ExtraArgs) != 1 || claude.ExtraArgs[0] != "--no-update" {
		t.Errorf("claude.ExtraArgs: got %v, want [--no-update]", claude.ExtraArgs)
	}
	if len(claude.CustomArgs) != 1 || claude.CustomArgs[0] != "--dangerously-skip-permissions" {
		t.Errorf("claude.CustomArgs: got %v, want [--dangerously-skip-permissions]", claude.CustomArgs)
	}
	if claude.Env["ANTHROPIC_API_KEY"] != "sk-ant-xxx" {
		t.Errorf("claude.Env[ANTHROPIC_API_KEY]: got %q, want sk-ant-xxx", claude.Env["ANTHROPIC_API_KEY"])
	}
	if claude.ThinkingLevel != "high" {
		t.Errorf("claude.ThinkingLevel: got %q, want high", claude.ThinkingLevel)
	}
	oc := c.Agents["openclaw"]
	if oc.OpenclawMode != "gateway" {
		t.Errorf("openclaw.OpenclawMode: got %q, want gateway", oc.OpenclawMode)
	}
	if oc.ThinkingLevel != "medium" {
		t.Errorf("openclaw.ThinkingLevel: got %q, want medium", oc.ThinkingLevel)
	}
	// Agents omitted from the file must be backfilled as enabled so
	// downstream lookups always find the four known agents.
	codex := c.Agents["codex"]
	if !codex.Enabled {
		t.Errorf("codex (omitted in file) should be backfilled as enabled=true")
	}
	opencode := c.Agents["opencode"]
	if !opencode.Enabled {
		t.Errorf("opencode (omitted in file) should be backfilled as enabled=true")
	}
}

func TestLoadMCPConfigMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aiclibridge.yaml")
	content := []byte(`listen: 127.0.0.1:8787
log_level: info
agents:
  claude:
    enabled: true
    mcp_config:
      mcpServers:
        fs:
          command: npx
          args:
            - -y
            - "@modelcontextprotocol/server-filesystem"
            - /tmp
`)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	raw := c.Agents["claude"].MCPConfig.Raw()
	if raw == nil {
		t.Fatal("MCPConfig.Raw() is nil, want JSON")
	}
	// The YAML mapping must round-trip into a compact JSON object
	// carrying the mcpServers key and the nested command value.
	got := string(raw)
	if !strings.Contains(got, `"mcpServers"`) {
		t.Errorf("MCP JSON missing mcpServers key: %s", got)
	}
	if !strings.Contains(got, `"command":"npx"`) {
		t.Errorf("MCP JSON missing command:npx: %s", got)
	}
}

func TestLoadMCPConfigNull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aiclibridge.yaml")
	content := []byte(`listen: 127.0.0.1:8787
log_level: info
agents:
  claude:
    enabled: true
    mcp_config: null
`)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if raw := c.Agents["claude"].MCPConfig.Raw(); raw != nil {
		t.Errorf("MCPConfig.Raw(): got %v, want nil", raw)
	}
}

func TestEnvOverridesWinOverFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aiclibridge.yaml")
	// File sets one set of values; env must win with another.
	content := []byte(`listen: 127.0.0.1:1111
log_level: info
api_key: from-file
agents:
  claude:
    enabled: true
    executable_path: /from/file/bin
    thinking_level: low
`)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("AICLIBRIDGE_LISTEN", "127.0.0.1:2222")
	t.Setenv("AICLIBRIDGE_API_KEY", "from-env")
	t.Setenv("AICLIBRIDGE_LOG_LEVEL", "warn")
	t.Setenv("AICLIBRIDGE_DEFAULT_TIMEOUT_MS", "12000")
	t.Setenv("AICLIBRIDGE_DATA_DIR", "/from/env/data")
	t.Setenv("AICLIBRIDGE_AGENTS_CLAUDE_ENABLED", "false")
	t.Setenv("AICLIBRIDGE_AGENTS_CLAUDE_EXECUTABLE_PATH", "/from/env/bin")
	t.Setenv("AICLIBRIDGE_AGENTS_CLAUDE_THINKING_LEVEL", "high")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != "127.0.0.1:2222" {
		t.Errorf("Listen: got %q, want 127.0.0.1:2222 (env wins)", c.Listen)
	}
	if c.APIKey != "from-env" {
		t.Errorf("APIKey: got %q, want from-env (env wins)", c.APIKey)
	}
	if c.LogLevel != "warn" {
		t.Errorf("LogLevel: got %q, want warn (env wins)", c.LogLevel)
	}
	if c.DefaultTimeoutMs != 12000 {
		t.Errorf("DefaultTimeoutMs: got %d, want 12000 (env wins)", c.DefaultTimeoutMs)
	}
	if c.DataDir != "/from/env/data" {
		t.Errorf("DataDir: got %q, want /from/env/data (env wins)", c.DataDir)
	}
	claude := c.Agents["claude"]
	if claude.Enabled {
		t.Errorf("claude.Enabled: got true, want false (env wins)")
	}
	if claude.ExecutablePath != "/from/env/bin" {
		t.Errorf("claude.ExecutablePath: got %q, want /from/env/bin (env wins)", claude.ExecutablePath)
	}
	if claude.ThinkingLevel != "high" {
		t.Errorf("claude.ThinkingLevel: got %q, want high (env wins)", claude.ThinkingLevel)
	}
}

func TestEnvOverridesApplyWithNoFile(t *testing.T) {
	// Env overrides apply even with no config file (Defaults + env).
	t.Setenv("AICLIBRIDGE_LISTEN", "0.0.0.0:7777")
	t.Setenv("AICLIBRIDGE_AGENTS_CODEX_ENABLED", "false")
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != "0.0.0.0:7777" {
		t.Errorf("Listen: got %q, want 0.0.0.0:7777 (env on defaults)", c.Listen)
	}
	if c.Agents["codex"].Enabled {
		t.Errorf("codex.Enabled: got true, want false (env on defaults)")
	}
}

func TestEnvOverridesOtherAgents(t *testing.T) {
	// Ensure the codex/opencode/openclaw agent env prefixes also apply.
	t.Setenv("AICLIBRIDGE_AGENTS_OPENCLAW_ENABLED", "false")
	t.Setenv("AICLIBRIDGE_AGENTS_OPENCLAW_EXECUTABLE_PATH", "/opt/openclaw")
	t.Setenv("AICLIBRIDGE_AGENTS_OPENCLAW_THINKING_LEVEL", "max")
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	oc := c.Agents["openclaw"]
	if oc.Enabled {
		t.Errorf("openclaw.Enabled: got true, want false (env)")
	}
	if oc.ExecutablePath != "/opt/openclaw" {
		t.Errorf("openclaw.ExecutablePath: got %q, want /opt/openclaw (env)", oc.ExecutablePath)
	}
	if oc.ThinkingLevel != "max" {
		t.Errorf("openclaw.ThinkingLevel: got %q, want max (env)", oc.ThinkingLevel)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	// A non-existent file path must not error and must yield Defaults
	// (env still applies, but we assert the default-shaped result).
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	c, err := Load(missing)
	if err != nil {
		t.Fatalf("Load(missing): got err %v, want nil", err)
	}
	if c.Listen != "127.0.0.1:8787" {
		t.Errorf("Listen: got %q, want default 127.0.0.1:8787", c.Listen)
	}
	if c.LogLevel != "info" {
		t.Errorf("LogLevel: got %q, want default info", c.LogLevel)
	}
	if !c.Agents["openclaw"].Enabled {
		t.Errorf("openclaw.Enabled: got false, want default true")
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{
			name:    "empty listen",
			mutate:  func(c *Config) { c.Listen = "" },
			wantSub: "listen",
		},
		{
			name:    "invalid log_level",
			mutate:  func(c *Config) { c.LogLevel = "trace" },
			wantSub: "log_level",
		},
		{
			name: "unknown agent",
			mutate: func(c *Config) {
				c.Agents["gemini"] = AgentConfig{Enabled: true}
			},
			wantSub: "unknown agent",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			tc.mutate(c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate: got nil, want error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("Validate error: got %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestValidateOK(t *testing.T) {
	c := Defaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate(Defaults): got err %v, want nil", err)
	}
}

func TestResolveConfigPath(t *testing.T) {
	t.Run("explicit wins over env", func(t *testing.T) {
		t.Setenv("AICLIBRIDGE_CONFIG", "/from/env/config.yaml")
		got, err := ResolveConfigPath("/explicit/path.yaml")
		if err != nil {
			t.Fatalf("ResolveConfigPath: %v", err)
		}
		if got != "/explicit/path.yaml" {
			t.Errorf("got %q, want /explicit/path.yaml", got)
		}
	})

	t.Run("env when no explicit", func(t *testing.T) {
		t.Setenv("AICLIBRIDGE_CONFIG", "/from/env/config.yaml")
		got, err := ResolveConfigPath("")
		if err != nil {
			t.Fatalf("ResolveConfigPath: %v", err)
		}
		if got != "/from/env/config.yaml" {
			t.Errorf("got %q, want /from/env/config.yaml", got)
		}
	})

	t.Run("cwd file beats home when no explicit or env", func(t *testing.T) {
		dir := t.TempDir()
		// os.Chdir is process-wide; restore on cleanup.
		orig, err := os.Getwd()
		if err != nil {
			t.Fatalf("Getwd: %v", err)
		}
		t.Cleanup(func() { _ = os.Chdir(orig) })
		if err := os.Chdir(dir); err != nil {
			t.Fatalf("Chdir: %v", err)
		}
		if err := os.WriteFile("aiclibridge.yaml", []byte("listen: 1.2.3.4:1\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		t.Setenv("AICLIBRIDGE_CONFIG", "")
		got, err := ResolveConfigPath("")
		if err != nil {
			t.Fatalf("ResolveConfigPath: %v", err)
		}
		// ResolveConfigPath returns the literal "./aiclibridge.yaml".
		if got != "./aiclibridge.yaml" {
			t.Errorf("got %q, want ./aiclibridge.yaml", got)
		}
	})

	t.Run("empty when nothing found", func(t *testing.T) {
		dir := t.TempDir()
		orig, err := os.Getwd()
		if err != nil {
			t.Fatalf("Getwd: %v", err)
		}
		t.Cleanup(func() { _ = os.Chdir(orig) })
		if err := os.Chdir(dir); err != nil {
			t.Fatalf("Chdir: %v", err)
		}
		t.Setenv("AICLIBRIDGE_CONFIG", "")
		// Redirect $HOME to an empty temp dir so the
		// ~/.aiclibridge/config.yaml candidate does not resolve to a
		// real file on the developer's machine.
		t.Setenv("HOME", t.TempDir())
		got, err := ResolveConfigPath("")
		if err != nil {
			t.Fatalf("ResolveConfigPath: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty (use Defaults)", got)
		}
	})
}
