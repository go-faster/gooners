// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
	"github.com/go-faster/errors"
)

// Config is the top-level TOML configuration for the gateway.
type Config struct {
	Server    ServerConfig     `toml:"server"`
	Upstreams []UpstreamConfig `toml:"upstream"`
	Secrets   []SecretConfig   `toml:"secret"`
	Telemetry TelemetryConfig  `toml:"telemetry"`
}

// ServerConfig configures the gateway's own MCP server identity.
type ServerConfig struct {
	Name         string `toml:"name"`
	Instructions string `toml:"instructions"`
}

// UpstreamConfig describes one upstream MCP server to proxy.
type UpstreamConfig struct {
	Name    string            `toml:"name"`
	Kind    string            `toml:"kind"`
	Command []string          `toml:"command"`
	URL     string            `toml:"url"`
	Headers map[string]string `toml:"headers"`
	Env     map[string]string `toml:"env"`
	Tools   ToolsConfig       `toml:"tools"`
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

// setDefaults applies server name and telemetry defaults.
func (c *Config) setDefaults() {
	if c.Server.Name == "" {
		c.Server.Name = "mcpgateway"
	}
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
	return nil
}
