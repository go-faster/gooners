// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigDefaults(t *testing.T) {
	c := &Config{}
	c.setDefaults()
	require.Equal(t, "mcpgateway", c.Server.Name)
	require.False(t, c.Telemetry.Enabled)
}

func TestConfigValidateErrors(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "no upstreams",
			cfg:     Config{},
			wantErr: "at least one upstream required",
		},
		{
			name:    "empty upstream name",
			cfg:     Config{Upstreams: []UpstreamConfig{{Name: ""}}},
			wantErr: "name is required",
		},
		{
			name:    "bad kind",
			cfg:     Config{Upstreams: []UpstreamConfig{{Name: "u", Kind: "foo"}}},
			wantErr: "invalid kind",
		},
		{
			name:    "stdio no command",
			cfg:     Config{Upstreams: []UpstreamConfig{{Name: "u", Kind: "stdio"}}},
			wantErr: "requires command",
		},
		{
			name:    "http no url",
			cfg:     Config{Upstreams: []UpstreamConfig{{Name: "u", Kind: "http"}}},
			wantErr: "requires url",
		},
		{
			name:    "route path no slash",
			cfg:     Config{Upstreams: []UpstreamConfig{{Name: "u", Kind: "stdio", Command: []string{"x"}, Route: RouteConfig{Path: "mcp"}}}},
			wantErr: "route.path",
		},
		{
			name:    "route host url",
			cfg:     Config{Upstreams: []UpstreamConfig{{Name: "u", Kind: "stdio", Command: []string{"x"}, Route: RouteConfig{Host: "https://example.com"}}}},
			wantErr: "route.host",
		},
		{
			name:    "route host port",
			cfg:     Config{Upstreams: []UpstreamConfig{{Name: "u", Kind: "stdio", Command: []string{"x"}, Route: RouteConfig{Host: "example.com:8443"}}}},
			wantErr: "route.host",
		},
		{
			name: "duplicate routes",
			cfg: Config{Upstreams: []UpstreamConfig{
				{Name: "u1", Kind: "stdio", Command: []string{"x"}, Route: RouteConfig{Host: "example.com", Path: "/mcp"}},
				{Name: "u2", Kind: "stdio", Command: []string{"x"}, Route: RouteConfig{Host: "example.com", Path: "/mcp"}},
			}},
			wantErr: "duplicates",
		},
		{
			name: "dup secret",
			cfg: Config{
				Upstreams: []UpstreamConfig{{Name: "u", Kind: "stdio", Command: []string{"x"}}},
				Secrets:   []SecretConfig{{Name: "s"}, {Name: "s"}},
			},
			wantErr: "duplicated",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestConfigLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.toml")
	data := `server.name = "gw"
[[upstream]]
name = "u1"
kind = "stdio"
command = ["echo", "hi"]
`
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	c, err := Load(p)
	require.NoError(t, err)
	require.Equal(t, "gw", c.Server.Name)
	require.Len(t, c.Upstreams, 1)
	require.Equal(t, "u1", c.Upstreams[0].Name)
}

func TestConfigValidateSecretRef(t *testing.T) {
	cfg := Config{
		Upstreams: []UpstreamConfig{{
			Name:    "u1",
			Kind:    "stdio",
			Command: []string{"x"},
			Env:     map[string]string{"A": "{secret:good}"},
		}},
		Secrets: []SecretConfig{{Name: "good"}},
	}
	require.NoError(t, cfg.Validate())
}

func TestConfigValidateSecretRefUnknown(t *testing.T) {
	cfg := Config{
		Upstreams: []UpstreamConfig{{
			Name:    "u1",
			Kind:    "stdio",
			Command: []string{"x"},
			Env:     map[string]string{"A": "{secret:missing}"},
		}},
		Secrets: []SecretConfig{{Name: "other"}},
	}
	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "upstream \"u1\"")
	require.Contains(t, err.Error(), "secret \"missing\"")
}

func TestConfigValidateSecretRefEmptyEnvHeaders(t *testing.T) {
	cfg := Config{
		Upstreams: []UpstreamConfig{{
			Name:    "u1",
			Kind:    "stdio",
			Command: []string{"x"},
		}},
	}
	require.NoError(t, cfg.Validate())
}

func TestConfigValidateAuthSecretRef(t *testing.T) {
	cfg := Config{
		Upstreams: []UpstreamConfig{{Name: "u1", Kind: "stdio", Command: []string{"x"}}},
		Auth: AuthConfig{
			Enabled: true,
			Header:  "Authorization",
			Value:   "Bearer {secret:gateway}",
		},
		Secrets: []SecretConfig{{Name: "gateway"}},
	}
	require.NoError(t, cfg.Validate())
}

func TestConfigValidateAuthSecretRefUnknown(t *testing.T) {
	cfg := Config{
		Upstreams: []UpstreamConfig{{Name: "u1", Kind: "stdio", Command: []string{"x"}}},
		Auth: AuthConfig{
			Enabled: true,
			Header:  "Authorization",
			Value:   "Bearer {secret:missing}",
		},
	}
	err := cfg.Validate()
	require.Error(t, err)
	require.ErrorContains(t, err, "auth")
	require.ErrorContains(t, err, "missing")
}

func TestConfigValidateStripHeaders(t *testing.T) {
	cfg := Config{
		Upstreams: []UpstreamConfig{{
			Name:         "u1",
			Kind:         "http",
			URL:          "http://example.invalid/mcp",
			StripHeaders: []string{"Authorization"},
		}},
	}
	require.NoError(t, cfg.Validate())
}

func TestConfig_Redact_InvalidPattern(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.toml")
	data := `[[upstream]]
name = "u1"
kind = "stdio"
command = ["echo", "hi"]

[redact]
enabled = true
patterns = ["(invalid"]
`
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	_, err := Load(p)
	require.Error(t, err)
	require.ErrorContains(t, err, "compile")
}

func TestConfig_Redact_Disabled_ByDefault(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.toml")
	data := `[[upstream]]
name = "u1"
kind = "stdio"
command = ["echo", "hi"]
`
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	c, err := Load(p)
	require.NoError(t, err)
	require.False(t, c.Redact.Enabled)
	require.Nil(t, c.Redact.Patterns)
}

func TestConfig_Redact_ValidPattern(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.toml")
	data := `[[upstream]]
name = "u1"
kind = "stdio"
command = ["echo", "hi"]

[redact]
enabled = true
patterns = ["(?i)secret"]
min_entropy = 3.0
`
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	c, err := Load(p)
	require.NoError(t, err)
	require.True(t, c.Redact.Enabled)
	require.Equal(t, []string{"(?i)secret"}, c.Redact.Patterns)
	require.Equal(t, 3.0, c.Redact.MinEntropy)
}

func TestConfig_UpstreamRedact_Override(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.toml")
	data := `[[upstream]]
name = "u1"
kind = "stdio"
command = ["echo", "hi"]

[upstream.redact]
enabled = true
patterns = ["(?i)local"]

[redact]
enabled = true
patterns = ["(?i)global"]
`
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	c, err := Load(p)
	require.NoError(t, err)
	require.Equal(t, []string{"(?i)global"}, c.Redact.Patterns)
	require.NotNil(t, c.Upstreams[0].Redact)
	require.Equal(t, []string{"(?i)local"}, c.Upstreams[0].Redact.Patterns)
}

func TestConfig_UpstreamRedact_NilWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.toml")
	data := `[[upstream]]
name = "u1"
kind = "stdio"
command = ["echo", "hi"]
`
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	c, err := Load(p)
	require.NoError(t, err)
	require.Nil(t, c.Upstreams[0].Redact)
}

func TestConfig_UpstreamRedact_InvalidPattern(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.toml")
	data := `[[upstream]]
name = "u1"
kind = "stdio"
command = ["echo", "hi"]

[upstream.redact]
enabled = true
patterns = ["(invalid"]
`
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	_, err := Load(p)
	require.Error(t, err)
	require.ErrorContains(t, err, "u1")
	require.ErrorContains(t, err, "redact")
}

func TestConfig_Telemetry_DisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.toml")
	data := `[[upstream]]
name = "u1"
kind = "stdio"
command = ["echo", "hi"]
`
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	c, err := Load(p)
	require.NoError(t, err)
	require.False(t, c.Telemetry.Enabled)
}

func TestConfig_Telemetry_Enabled_NoEndpoints(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.toml")
	data := `[[upstream]]
name = "u1"
kind = "stdio"
command = ["echo", "hi"]

[telemetry]
enabled = true
`
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	_, err := Load(p)
	require.Error(t, err)
	require.ErrorContains(t, err, "no otlp_endpoint or metrics_addr")
}

func TestConfig_Telemetry_Enabled_InvalidOTLP(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.toml")
	data := `[[upstream]]
name = "u1"
kind = "stdio"
command = ["echo", "hi"]

[telemetry]
enabled = true
otlp_endpoint = "not-a-url"
`
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	_, err := Load(p)
	require.Error(t, err)
	require.ErrorContains(t, err, "otlp_endpoint")
}

func TestConfig_Telemetry_Enabled_ValidOTLP(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.toml")
	data := `[[upstream]]
name = "u1"
kind = "stdio"
command = ["echo", "hi"]

[telemetry]
enabled = true
otlp_endpoint = "http://localhost:4318"
`
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	_, err := Load(p)
	require.NoError(t, err)
}

func TestConfig_Telemetry_Enabled_ValidMetrics(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.toml")
	data := `[[upstream]]
name = "u1"
kind = "stdio"
command = ["echo", "hi"]

[telemetry]
enabled = true
metrics_addr = ":9090"
`
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	_, err := Load(p)
	require.NoError(t, err)
}

func TestConfig_Telemetry_Enabled_InvalidMetrics(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.toml")
	data := `[[upstream]]
name = "u1"
kind = "stdio"
command = ["echo", "hi"]

[telemetry]
enabled = true
metrics_addr = "nope"
`
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	_, err := Load(p)
	require.Error(t, err)
	require.ErrorContains(t, err, "metrics_addr")
}

func TestConfig_Telemetry_Enabled_Both(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.toml")
	data := `[[upstream]]
name = "u1"
kind = "stdio"
command = ["echo", "hi"]

[telemetry]
enabled = true
otlp_endpoint = "http://localhost:4318"
metrics_addr = ":9090"
`
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	_, err := Load(p)
	require.NoError(t, err)
}

func TestConfig_Reconnect_Valid(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.toml")
	data := `[[upstream]]
name = "u1"
kind = "stdio"
command = ["echo", "hi"]

[upstream.reconnect]
keepalive = "10s"
initial_backoff = "500ms"
max_backoff = "30s"
`
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	c, err := Load(p)
	require.NoError(t, err)
	require.NotNil(t, c.Upstreams[0].Reconnect)
	require.Equal(t, "10s", c.Upstreams[0].Reconnect.KeepAlive)
	require.Equal(t, "500ms", c.Upstreams[0].Reconnect.InitialBackoff)
	require.Equal(t, "30s", c.Upstreams[0].Reconnect.MaxBackoff)
}

func TestConfig_Reconnect_InvalidDuration(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.toml")
	data := `[[upstream]]
name = "u1"
kind = "stdio"
command = ["echo", "hi"]

[upstream.reconnect]
keepalive = "nope"
`
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	_, err := Load(p)
	require.Error(t, err)
	require.ErrorContains(t, err, "u1")
	require.ErrorContains(t, err, "reconnect")
	require.ErrorContains(t, err, "keepalive")
}

func TestConfig_Reconnect_InitialGreaterThanMax(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.toml")
	data := `[[upstream]]
name = "u1"
kind = "stdio"
command = ["echo", "hi"]

[upstream.reconnect]
initial_backoff = "30s"
max_backoff = "1s"
`
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	_, err := Load(p)
	require.Error(t, err)
	require.ErrorContains(t, err, "initial_backoff")
}
