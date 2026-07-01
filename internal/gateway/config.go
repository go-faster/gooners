// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/go-faster/errors"
)

// Config is the top-level TOML configuration for the gateway.
type Config struct {
	Server    ServerConfig     `toml:"server"`
	Upstreams []UpstreamConfig `toml:"upstream"`
	Secrets   []SecretConfig   `toml:"secret"`
	Telemetry TelemetryConfig  `toml:"telemetry"`
	Redact    RedactConfig     `toml:"redact"`
}

// setDefaults applies server name and telemetry defaults.
func (c *Config) setDefaults() {
	if c.Server.Name == "" {
		c.Server.Name = "mcpgateway"
	}
}

// ServerConfig configures the gateway's own MCP server identity.
type ServerConfig struct {
	Name         string `toml:"name"`
	Instructions string `toml:"instructions"`
}

// UpstreamConfig describes one upstream MCP server to proxy.
type UpstreamConfig struct {
	Name      string            `toml:"name"`
	Kind      string            `toml:"kind"`
	Command   []string          `toml:"command"`
	URL       string            `toml:"url"`
	Headers   map[string]string `toml:"headers"`
	Env       map[string]string `toml:"env"`
	Tools     ToolsConfig       `toml:"tools"`
	Reconnect *ReconnectConfig  `toml:"reconnect"`
	// Redact overrides the global redact config when present; nil inherits the global [redact] section.
	Redact *RedactConfig `toml:"redact"`
}

// ReconnectConfig configures per-upstream reconnect supervision.
type ReconnectConfig struct {
	KeepAlive      string `toml:"keepalive"`
	InitialBackoff string `toml:"initial_backoff"`
	MaxBackoff     string `toml:"max_backoff"`
}

// ToolsConfig controls tool filtering, namespacing and description trimming for an upstream.
type ToolsConfig struct {
	Allow   []string `toml:"allow"`
	Deny    []string `toml:"deny"`
	Prefix  string   `toml:"prefix"`
	DescMax int      `toml:"desc_max"`
}

// SecretConfig defines a named secret that can be interpolated into env/headers.
type SecretConfig struct {
	Name    string `toml:"name"`
	Value   string `toml:"value"`
	Env     string `toml:"env"`
	File    string `toml:"file"`
	Command string `toml:"command"`
}

// TelemetryConfig configures optional OTLP telemetry export.
type TelemetryConfig struct {
	Enabled      bool   `toml:"enabled"`
	OTLPEndpoint string `toml:"otlp_endpoint"`
	MetricsAddr  string `toml:"metrics_addr"`
}

// RedactConfig configures output secret redaction applied to all tool text content.
type RedactConfig struct {
	Enabled    bool     `toml:"enabled"`
	Patterns   []string `toml:"patterns"`
	MinEntropy float64  `toml:"min_entropy"`
}

// Load reads a TOML file, decodes it, applies defaults and validates.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: operator-controlled config file path
	if err != nil {
		return nil, errors.Wrap(err, "read config")
	}
	var c Config
	if _, err := toml.Decode(string(data), &c); err != nil {
		return nil, errors.Wrap(err, "decode toml")
	}
	c.setDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks required fields and uniqueness constraints.
func (c *Config) Validate() error {
	if len(c.Upstreams) == 0 {
		return errors.New("at least one upstream required")
	}
	seenUp := map[string]bool{}
	for i, u := range c.Upstreams {
		if u.Name == "" {
			return fmt.Errorf("upstream[%d]: name is required", i)
		}
		if seenUp[u.Name] {
			return fmt.Errorf("upstream name %q duplicated", u.Name)
		}
		seenUp[u.Name] = true
		switch u.Kind {
		case "stdio", "http", "sse":
		default:
			return fmt.Errorf("upstream %q: invalid kind %q (want stdio|http|sse)", u.Name, u.Kind)
		}
		if u.Kind == "stdio" && len(u.Command) == 0 {
			return fmt.Errorf("upstream %q: stdio requires command", u.Name)
		}
		if (u.Kind == "http" || u.Kind == "sse") && u.URL == "" {
			return fmt.Errorf("upstream %q: %s requires url", u.Name, u.Kind)
		}
		if u.Reconnect != nil {
			if err := validateReconnectConfig(u.Name, u.Reconnect); err != nil {
				return err
			}
		}
	}
	seenSec := map[string]bool{}
	for i, s := range c.Secrets {
		if s.Name == "" {
			return fmt.Errorf("secret[%d]: name is required", i)
		}
		if seenSec[s.Name] {
			return fmt.Errorf("secret name %q duplicated", s.Name)
		}
		seenSec[s.Name] = true
	}

	var joinErrs []error
	for _, u := range c.Upstreams {
		for _, v := range u.Env {
			for name := range extractSecretRefs(v) {
				if !seenSec[name] {
					joinErrs = append(joinErrs, fmt.Errorf("upstream %q: secret %q referenced in env/headers is not defined", u.Name, name))
				}
			}
		}
		for _, v := range u.Headers {
			for name := range extractSecretRefs(v) {
				if !seenSec[name] {
					joinErrs = append(joinErrs, fmt.Errorf("upstream %q: secret %q referenced in env/headers is not defined", u.Name, name))
				}
			}
		}
		if u.Redact != nil && u.Redact.Enabled && len(u.Redact.Patterns) > 0 {
			if _, err := NewRedactor(u.Redact.Patterns, u.Redact.MinEntropy); err != nil {
				joinErrs = append(joinErrs, errors.Wrapf(err, "upstream %q: compile redact patterns", u.Name))
			}
		}
	}
	if len(joinErrs) > 0 {
		return errors.Join(joinErrs...)
	}
	if c.Redact.Enabled && len(c.Redact.Patterns) > 0 {
		if _, err := NewRedactor(c.Redact.Patterns, c.Redact.MinEntropy); err != nil {
			return errors.Wrap(err, "compile redact patterns")
		}
	}
	if c.Telemetry.Enabled {
		if c.Telemetry.OTLPEndpoint == "" && c.Telemetry.MetricsAddr == "" {
			return fmt.Errorf("telemetry: enabled but no otlp_endpoint or metrics_addr configured")
		}
		if c.Telemetry.OTLPEndpoint != "" {
			u, err := url.Parse(c.Telemetry.OTLPEndpoint)
			if err != nil {
				return fmt.Errorf("telemetry: invalid otlp_endpoint %q: %w", c.Telemetry.OTLPEndpoint, err)
			}
			if u.Scheme == "" || u.Host == "" {
				return fmt.Errorf("telemetry: otlp_endpoint %q must be a full URL with scheme and host", c.Telemetry.OTLPEndpoint)
			}
		}
		if c.Telemetry.MetricsAddr != "" {
			if _, _, err := net.SplitHostPort(c.Telemetry.MetricsAddr); err != nil {
				return fmt.Errorf("telemetry: invalid metrics_addr %q: %w", c.Telemetry.MetricsAddr, err)
			}
		}
	}
	return nil
}

func validateReconnectConfig(upstream string, cfg *ReconnectConfig) error {
	keepAlive, err := parseOptionalDuration(cfg.KeepAlive)
	if err != nil {
		return fmt.Errorf("upstream %q: reconnect: keepalive: %w", upstream, err)
	}
	initialBackoff, err := parseOptionalDuration(cfg.InitialBackoff)
	if err != nil {
		return fmt.Errorf("upstream %q: reconnect: initial_backoff: %w", upstream, err)
	}
	maxBackoff, err := parseOptionalDuration(cfg.MaxBackoff)
	if err != nil {
		return fmt.Errorf("upstream %q: reconnect: max_backoff: %w", upstream, err)
	}
	if cfg.InitialBackoff != "" && cfg.MaxBackoff != "" && initialBackoff > maxBackoff {
		return fmt.Errorf("upstream %q: reconnect: initial_backoff must be <= max_backoff", upstream)
	}
	_ = keepAlive
	return nil
}

func parseOptionalDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}
