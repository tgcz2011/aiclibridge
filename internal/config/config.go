package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// KnownAgents lists the agent names aiclibridge supports. The order is
// stable so Defaults and env-override loops visit agents deterministically.
// v0.2 adds 13 more CLIs surfaced from AionUi's ACP backend catalogue:
// codebuddy (stream-json, Claude SDK schema), copilot/goose/cursor/kimi/
// kiro/qoder/hermes/auggie (ACP JSON-RPC via acp.go), and droid/snow/vibe/
// aion (stub — protocol unknown, awaiting upstream docs).
var KnownAgents = []string{
	// v0.1 stream-json / NDJSON / JSON-RPC app-server family
	"claude", "codex", "opencode", "openclaw", "qwen", "gemini",
	// v0.2 stream-json (Claude SDK schema, same flags as qwen)
	"codebuddy",
	// v0.2 ACP JSON-RPC family (generic adapter in acp.go)
	"copilot", "goose", "cursor", "kimi", "kiro", "qoder", "hermes", "auggie",
	// v0.2 stubs (protocol not yet documented; registered but Execute
	// returns ErrNotImplemented so the catalog lists them honestly)
	"droid", "snow", "vibe", "aion",
}

// knownAgent is a set for O(1) membership checks during Validate.
var knownAgent = func() map[string]struct{} {
	m := make(map[string]struct{}, len(KnownAgents))
	for _, a := range KnownAgents {
		m[a] = struct{}{}
	}
	return m
}()

// validLogLevels is the set of accepted log_level values.
var validLogLevels = map[string]struct{}{
	"debug": {},
	"info":  {},
	"warn":  {},
	"error": {},
}

// Config is the top-level daemon configuration. It is loaded from YAML
// (see Load) and then partially overridden by AICLIBRIDGE_* env vars.
type Config struct {
	Listen           string                 `yaml:"listen"`
	APIKey           string                 `yaml:"api_key"`
	DataDir          string                 `yaml:"data_dir"`
	LogLevel         string                 `yaml:"log_level"`
	DefaultTimeoutMs int                    `yaml:"default_timeout_ms"`
	Agents           map[string]AgentConfig `yaml:"agents"`
}

// AgentConfig is the per-CLI slice of Config. Fields map 1:1 onto
// adapter.Config and adapter.ExecOptions so the daemon can build a
// Backend and per-run ExecOptions without re-interpretation.
type AgentConfig struct {
	// Enabled gates whether the agent is exposed at all. Disabled
	// agents are absent from the /v1/agents listing and reject runs.
	Enabled bool `yaml:"enabled"`
	// ExecutablePath overrides the CLI binary location. Empty means
	// "look up via PATH" (claude, codex, opencode, openclaw).
	ExecutablePath string `yaml:"executable_path"`
	// ExtraArgs are daemon-wide default CLI arguments appended after
	// the daemon's own flags and before CustomArgs.
	ExtraArgs []string `yaml:"extra_args"`
	// CustomArgs are user-defined CLI arguments appended last.
	CustomArgs []string `yaml:"custom_args"`
	// Env is extra environment variables (key: value) for the agent
	// subprocess, merged on top of the daemon's own environment.
	Env map[string]string `yaml:"env"`
	// MCPConfig carries an MCP server configuration as JSON, mirroring
	// adapter.ExecOptions.McpConfig (a json.RawMessage). yaml.v3
	// base64-encodes bare []byte, which would force users to
	// base64-encode inline JSON; MCPConfig wraps json.RawMessage with a
	// custom UnmarshalYAML so users can author it as an inline YAML
	// mapping (re-encoded to JSON on load) or leave it null.
	MCPConfig MCPConfig `yaml:"mcp_config"`
	// ThinkingLevel is the default reasoning effort
	// (low|medium|high|xhigh|max for claude, etc.). Empty means "use
	// the runtime/model default".
	ThinkingLevel string `yaml:"thinking_level"`
	// OpenclawMode selects local vs gateway routing for the openclaw
	// backend (local|gateway). Other backends ignore this field. The
	// empty string is treated as "local" by the openclaw adapter.
	OpenclawMode string `yaml:"openclaw_mode"`
}

// MCPConfig is a JSON-encoded MCP server configuration. It is a named
// type over json.RawMessage so it can carry a custom YAML unmarshaler
// while remaining trivially convertible to the json.RawMessage the
// adapter consumes (via Raw).
type MCPConfig json.RawMessage

// Raw returns the underlying json.RawMessage, or nil if no MCP config
// was set. Pass this directly to adapter.ExecOptions.McpConfig.
func (m MCPConfig) Raw() json.RawMessage { return json.RawMessage(m) }

// UnmarshalYAML decodes a YAML node into a Go value and re-encodes it
// as JSON. A null/empty node leaves the config empty (Raw() == nil).
// This lets users write:
//
//	mcp_config:
//	  mcpServers:
//	    fs:
//	      command: npx
//	      args: [-y, "@modelcontextprotocol/server-filesystem", /tmp]
//
// instead of base64-encoding inline JSON.
func (m *MCPConfig) UnmarshalYAML(value *yaml.Node) error {
	var v any
	if err := value.Decode(&v); err != nil {
		return fmt.Errorf("decode mcp_config: %w", err)
	}
	if v == nil {
		*m = nil
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("re-encode mcp_config as json: %w", err)
	}
	*m = MCPConfig(b)
	return nil
}

// Defaults returns a Config with sensible defaults: listen on the
// loopback, data dir under ./data, info logging, no global timeout,
// and all known agents enabled with empty (PATH-discovered) executables.
// Env overrides are NOT applied here; call Load to layer them on.
func Defaults() *Config {
	c := &Config{
		Listen:           "127.0.0.1:8787",
		DataDir:          "./data",
		LogLevel:         "info",
		DefaultTimeoutMs: 0,
		Agents:           make(map[string]AgentConfig, len(KnownAgents)),
	}
	for _, name := range KnownAgents {
		c.Agents[name] = AgentConfig{Enabled: true}
	}
	return c
}

// Load reads a YAML config from path, applies env overrides, and returns
// the resulting Config. If path is empty or the named file does not
// exist, Defaults are used (env overrides still apply). Load is
// deliberately lenient about a missing file so the daemon can start
// with no config at all; a caller that wants to reject an explicitly
// requested --config path whose file is absent should os.Stat the
// resolved path before calling Load (Load cannot tell whether path
// came from --config, an env pointer, or auto-discovery).
func Load(path string) (*Config, error) {
	c := Defaults()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read config %q: %w", path, err)
			}
			// Missing file: keep Defaults, fall through to env overrides.
		} else {
			if err := yaml.Unmarshal(data, c); err != nil {
				return nil, fmt.Errorf("parse config %q: %w", path, err)
			}
			if c.Agents == nil {
				c.Agents = make(map[string]AgentConfig, len(KnownAgents))
			}
			// Backfill any missing known agents as enabled so env
			// overrides and downstream lookups always find them.
			for _, name := range KnownAgents {
				if _, ok := c.Agents[name]; !ok {
					c.Agents[name] = AgentConfig{Enabled: true}
				}
			}
		}
	}
	applyEnvOverrides(c)
	return c, nil
}

// applyEnvOverrides layers AICLIBRIDGE_* environment variables on top
// of the config in place. Env wins over the YAML file. Unparseable
// numeric/bool values are ignored (the existing value is kept) so a
// typo in env never silently zeroes a field.
func applyEnvOverrides(c *Config) {
	if v, ok := os.LookupEnv("AICLIBRIDGE_LISTEN"); ok {
		c.Listen = v
	}
	if v, ok := os.LookupEnv("AICLIBRIDGE_API_KEY"); ok {
		c.APIKey = v
	}
	if v, ok := os.LookupEnv("AICLIBRIDGE_DATA_DIR"); ok {
		c.DataDir = v
	}
	if v, ok := os.LookupEnv("AICLIBRIDGE_LOG_LEVEL"); ok {
		c.LogLevel = v
	}
	if v, ok := os.LookupEnv("AICLIBRIDGE_DEFAULT_TIMEOUT_MS"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			c.DefaultTimeoutMs = n
		}
	}
	for _, name := range KnownAgents {
		prefix := "AICLIBRIDGE_AGENTS_" + strings.ToUpper(name) + "_"
		a := c.Agents[name]
		if v, ok := os.LookupEnv(prefix + "ENABLED"); ok {
			if b, err := strconv.ParseBool(v); err == nil {
				a.Enabled = b
			}
		}
		if v, ok := os.LookupEnv(prefix + "EXECUTABLE_PATH"); ok {
			a.ExecutablePath = v
		}
		if v, ok := os.LookupEnv(prefix + "THINKING_LEVEL"); ok {
			a.ThinkingLevel = v
		}
		c.Agents[name] = a
	}
}

// Validate checks the config for required fields and known values. It
// returns an error if listen is empty, log_level is not one of
// debug|info|warn|error, or any agent name is outside the KnownAgents
// set (claude|codex|opencode|openclaw|qwen|gemini).
func (c *Config) Validate() error {
	if c.Listen == "" {
		return errors.New("config: listen must not be empty")
	}
	if _, ok := validLogLevels[c.LogLevel]; !ok {
		return fmt.Errorf("config: invalid log_level %q (want debug|info|warn|error)", c.LogLevel)
	}
	for name := range c.Agents {
		if _, ok := knownAgent[name]; !ok {
			return fmt.Errorf("config: unknown agent %q (want one of %s)", name, strings.Join(KnownAgents, "|"))
		}
	}
	return nil
}

// ResolveConfigPath returns the config file path to load, in priority
// order:
//
//  1. explicit (the --config flag), used verbatim if non-empty.
//  2. $AICLIBRIDGE_CONFIG, used verbatim if non-empty.
//  3. ./aiclibridge.yaml, if it exists.
//  4. ~/.aiclibridge/config.yaml, if it exists.
//  5. "" (no file — Load will use Defaults).
//
// Explicit and env paths are returned verbatim (not existence-checked)
// so the caller can distinguish "user asked for this file" from
// "auto-discovered"; auto-discovered candidates are existence-checked
// so a missing default is skipped rather than returned. The returned
// error is reserved for future use and is currently always nil.
func ResolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if v := os.Getenv("AICLIBRIDGE_CONFIG"); v != "" {
		return v, nil
	}
	if fileExists("./aiclibridge.yaml") {
		return "./aiclibridge.yaml", nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".aiclibridge", "config.yaml")
		if fileExists(p) {
			return p, nil
		}
	}
	return "", nil
}

// fileExists reports whether path names an existing regular file or
// symlink target. Directories return false.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
