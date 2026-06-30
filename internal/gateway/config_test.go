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
