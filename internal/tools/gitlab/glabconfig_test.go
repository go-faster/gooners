package gitlab

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadGlabConfig(t *testing.T) {
	t.Run("missing file yields empty config", func(t *testing.T) {
		cfg, err := LoadGlabConfig(t.TempDir())
		require.NoError(t, err)
		require.Empty(t, cfg.Host)
		require.Empty(t, cfg.Hosts)
	})

	t.Run("empty dir yields empty config", func(t *testing.T) {
		cfg, err := LoadGlabConfig("")
		require.NoError(t, err)
		require.Empty(t, cfg.Host)
	})

	t.Run("parses hosts", func(t *testing.T) {
		dir := t.TempDir()
		writeGlabConfig(t, dir, `
git_protocol: ssh
host: gitlab.example.com
hosts:
    gitlab.example.com:
        token: secret-token
    gitlab.com:
        token: other-token
        api_protocol: https
`)
		cfg, err := LoadGlabConfig(dir)
		require.NoError(t, err)
		require.Equal(t, "gitlab.example.com", cfg.Host)
		require.Equal(t, "secret-token", cfg.Hosts["gitlab.example.com"].Token)
		require.Equal(t, "other-token", cfg.Hosts["gitlab.com"].Token)
	})

	t.Run("malformed yaml errors", func(t *testing.T) {
		dir := t.TempDir()
		writeGlabConfig(t, dir, "hosts:\n\t- not: a mapping\n  bad")
		_, err := LoadGlabConfig(dir)
		require.Error(t, err)
	})
}

func TestGlabConfigResolve(t *testing.T) {
	cfg := &GlabConfig{
		Host: "gitlab.example.com",
		Hosts: map[string]GlabHost{
			"gitlab.example.com": {Token: "secret-token"},
			"gitlab.com":         {Token: "com-token"},
			"self.example.com": {
				Token:       "self-token",
				APIHost:     "api.self.example.com",
				APIProtocol: "http",
			},
		},
	}

	for _, tt := range []struct {
		name      string
		host      string
		wantURL   string
		wantToken string
	}{
		{"empty host uses the config default", "", "https://gitlab.example.com", "secret-token"},
		{"explicit host", "gitlab.com", "https://gitlab.com", "com-token"},
		{"api_host and api_protocol override", "self.example.com", "http://api.self.example.com", "self-token"},
		{"unknown host still names the instance", "other.example.com", "https://other.example.com", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gotURL, gotToken := cfg.Resolve(tt.host)
			require.Equal(t, tt.wantURL, gotURL)
			require.Equal(t, tt.wantToken, gotToken)
		})
	}

	t.Run("no default host and no argument yields nothing", func(t *testing.T) {
		gotURL, gotToken := (&GlabConfig{}).Resolve("")
		require.Empty(t, gotURL)
		require.Empty(t, gotToken)
	})
}

func TestGlabConfigDir(t *testing.T) {
	t.Run("GLAB_CONFIG_DIR wins", func(t *testing.T) {
		t.Setenv("GLAB_CONFIG_DIR", "/custom/dir")
		t.Setenv("XDG_CONFIG_HOME", "/xdg")
		require.Equal(t, "/custom/dir", GlabConfigDir())
	})

	t.Run("XDG_CONFIG_HOME is next", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("GLAB_CONFIG_DIR", "")
		t.Setenv("XDG_CONFIG_HOME", xdg)
		require.Equal(t, filepath.Join(xdg, "glab-cli"), GlabConfigDir())
	})
}

func writeGlabConfig(t *testing.T, dir, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yml"), []byte(content), 0o600))
}
