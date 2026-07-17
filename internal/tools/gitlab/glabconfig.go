package gitlab

import (
	"os"
	"path/filepath"

	"github.com/go-faster/errors"
	"github.com/go-faster/yaml"
)

// GlabConfig is the subset of the glab CLI's config.yml that this server reads:
// enough to reuse a token the operator already has, so `glab auth login` is the
// only setup step.
//
// Reading it is an operator-controlled startup path — no agent influences it —
// which is why it uses os.ReadFile rather than an [effect.FS].
type GlabConfig struct {
	// Host is the default instance hostname.
	Host string `yaml:"host"`
	// Hosts maps a hostname to its credentials.
	Hosts map[string]GlabHost `yaml:"hosts"`
}

// GlabHost holds one instance's entry in [GlabConfig].
type GlabHost struct {
	Token       string `yaml:"token"`
	APIHost     string `yaml:"api_host"`
	APIProtocol string `yaml:"api_protocol"`
}

// GlabConfigDir mirrors glab's own resolution order: GLAB_CONFIG_DIR wins
// outright, then XDG_CONFIG_HOME/glab-cli, then ~/.config/glab-cli.
func GlabConfigDir() string {
	if dir := os.Getenv("GLAB_CONFIG_DIR"); dir != "" {
		return dir
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "glab-cli")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "glab-cli")
}

// LoadGlabConfig reads glab's config.yml. A missing file is not an error: it
// yields a zero config, and the caller falls back to its own flags.
func LoadGlabConfig(dir string) (*GlabConfig, error) {
	if dir == "" {
		return &GlabConfig{}, nil
	}
	// The path comes from the operator's flag or glab's own location, never
	// from an agent, which is why a raw os.ReadFile is correct here.
	data, err := os.ReadFile(filepath.Join(dir, "config.yml")) //nolint:gosec // G304: operator-controlled startup path.
	if err != nil {
		if os.IsNotExist(err) {
			return &GlabConfig{}, nil
		}
		return nil, errors.Wrap(err, "read glab config")
	}
	var cfg GlabConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, errors.Wrap(err, "parse glab config")
	}
	return &cfg, nil
}

// Resolve returns the base URL and token for host, or for the config's default
// host when host is empty. It returns empty strings when the config has nothing
// to say, leaving the caller's own defaults in place.
func (c *GlabConfig) Resolve(host string) (baseURL, token string) {
	if host == "" {
		host = c.Host
	}
	if host == "" {
		return "", ""
	}

	h, ok := c.Hosts[host]
	if !ok {
		// A known default host with no credentials entry still tells us which
		// instance the operator means.
		return "https://" + host, ""
	}

	scheme := h.APIProtocol
	if scheme == "" {
		scheme = "https"
	}
	name := h.APIHost
	if name == "" {
		name = host
	}
	return scheme + "://" + name, h.Token
}
