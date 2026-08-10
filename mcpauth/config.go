// Package mcpauth guards an MCP HTTP endpoint with a shared secret and, if
// configured, a local OAuth authorization-code facade.
//
// The two credentials are not equivalent. The shared secret is the
// trusted-operator one and grants unrestricted access; OAuth-issued bearer
// tokens carry scopes, and the operator secret is what authorizes issuing them.
// A server with no scope model of its own can enable the first and leave the
// second off.
package mcpauth

import (
	"errors"
	"fmt"
	"time"
)

// Config configures inbound HTTP authentication.
type Config struct {
	Enabled bool        `toml:"enabled"`
	Header  string      `toml:"header"`
	Value   string      `toml:"value"`
	OAuth   OAuthConfig `toml:"oauth"`
}

// OAuthConfig configures an optional local OAuth authorization-code facade for
// inbound clients.
type OAuthConfig struct {
	Enabled  bool     `toml:"enabled"`
	Issuer   string   `toml:"issuer"`
	Resource string   `toml:"resource"`
	Scopes   []string `toml:"scopes"`
	ClientID string   `toml:"client_id"`
	TokenTTL string   `toml:"token_ttl"`
	// RedirectURIs is the allowlist of exact redirect_uri values the authorization
	// endpoint will redirect to. Required when Enabled: without it any caller could
	// redirect authorization codes to an attacker-controlled origin.
	RedirectURIs []string `toml:"redirect_uris"`
}

// Validate reports what is missing from an enabled configuration, joining every
// problem rather than stopping at the first, so one pass over a config file
// reports all of them.
//
// A disabled config is always valid: leaving fields blank is how it is switched
// off.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}

	var errs []error
	if c.Header == "" {
		errs = append(errs, errors.New("auth: header is required when enabled"))
	}
	if c.Value == "" {
		errs = append(errs, errors.New("auth: value is required when enabled"))
	}
	errs = append(errs, c.OAuth.validate()...)

	return errors.Join(errs...)
}

func (c OAuthConfig) validate() []error {
	if !c.Enabled {
		return nil
	}

	var errs []error
	if c.Issuer == "" {
		errs = append(errs, errors.New("auth.oauth: issuer is required when enabled"))
	}
	if c.Resource == "" {
		errs = append(errs, errors.New("auth.oauth: resource is required when enabled"))
	}
	if len(c.RedirectURIs) == 0 {
		errs = append(errs, errors.New("auth.oauth: redirect_uris is required when enabled"))
	}
	if _, err := parseOptionalDuration(c.TokenTTL); err != nil {
		errs = append(errs, fmt.Errorf("auth.oauth: token_ttl: %w", err))
	}

	return errs
}

func parseOptionalDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}

	return time.ParseDuration(s)
}
