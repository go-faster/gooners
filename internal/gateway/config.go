// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/go-faster/errors"
)

// Config is the top-level TOML configuration for the gateway.
type Config struct {
	Server    ServerConfig     `toml:"server"`
	Upstreams []UpstreamConfig `toml:"upstream"`
	Secrets   []SecretConfig   `toml:"secret"`
	Auth      AuthConfig       `toml:"auth"`
	Telemetry TelemetryConfig  `toml:"telemetry"`
	Redact    RedactConfig     `toml:"redact"`
	Blob      BlobConfig       `toml:"blob"`
}

// BlobConfig configures the blob store behind the gateway's blob_share tool.
//
// It exists for upstreams that write their output to a directory and report a
// host path. A path is useless to an agent that does not share that
// filesystem; with the same directory bind-mounted into the gateway, the path
// becomes a URL the agent can fetch.
//
// Either backend serves that: the built-in HTTP one on its own listener, or a
// bucket via [BlobS3Config]. The bucket additionally makes the shared file
// readable by other servers configured against it, which is what lets the file
// go on to one of them without the agent carrying the bytes.
type BlobConfig struct {
	// Addr is the blob store's own listen address, e.g. ":8090". Empty
	// disables blob_share entirely, unless s3 is configured.
	Addr string `toml:"addr"`
	// BaseURL is where the agent reaches Addr, e.g. https://gw.example.com/blob.
	// Required unless Addr is unambiguously local, since only then does the
	// address imply its own URL.
	BaseURL string `toml:"base_url"`
	// Dir holds objects the gateway stores itself. Mounted files are served in
	// place and never land here.
	Dir string `toml:"dir"`
	// TTL is how long a URL keeps working. Empty means [blob.DefaultTTL].
	TTL string `toml:"ttl"`
	// S3 puts the objects in a bucket instead of behind a listener of the
	// gateway's own.
	S3 BlobS3Config `toml:"s3"`
	// Mounts are the directories blob_share may serve from. A path outside
	// every mount is refused: the mount list, not the caller, is the boundary.
	Mounts []BlobMountConfig `toml:"mount"`
}

// BlobS3Config points the blob store at a bucket.
//
// It is the answer when the gateway and the servers that should read what it
// shares are not on one machine: a listener of the gateway's own is reachable
// only from where its address is, whereas anything holding bucket credentials
// can read an object by id.
//
// Credentials are not config. They come from the environment (AWS_ACCESS_KEY_ID
// and friends, or ~/.aws/credentials), so the file can be committed and so the
// gateway's own read of it never has to be treated as secret-bearing.
type BlobS3Config struct {
	// Endpoint is the S3 endpoint, as host[:port] or an http(s) URL. Setting it
	// selects this backend.
	Endpoint string `toml:"endpoint"`
	// Bucket holds the objects and must already exist.
	Bucket string `toml:"bucket"`
	// Prefix is the key prefix this gateway writes under. It is the tenancy
	// boundary: no id can name an object outside it.
	Prefix string `toml:"prefix"`
	// Region is the bucket region. Optional for MinIO and for endpoints that
	// encode it.
	Region string `toml:"region"`
}

// Enabled reports whether the S3 backend was selected.
func (c BlobS3Config) Enabled() bool { return c.Endpoint != "" }

// BlobMountConfig is one directory the gateway may serve files from.
type BlobMountConfig struct {
	// Name identifies the mount in errors and logs. Usually the upstream that
	// writes into it.
	Name string `toml:"name"`
	// Dir is the directory as the gateway sees it.
	Dir string `toml:"dir"`
	// Prefix is the directory as upstreams report it, when a bind mount put it
	// somewhere else in their namespace. Empty means it is the same as Dir.
	//
	// Nothing translates a path automatically: an upstream saying
	// /var/lib/example-mcp/out.png is only understood here because some mount
	// claims that prefix.
	Prefix string `toml:"prefix"`
}

// Enabled reports whether the blob store should be built at all.
func (c BlobConfig) Enabled() bool { return c.Addr != "" || c.S3.Enabled() }

// TTLDuration is the parsed [BlobConfig.TTL]; zero means the store's default.
func (c BlobConfig) TTLDuration() (time.Duration, error) {
	return parseOptionalDuration(c.TTL)
}

// validateBackend rejects a section naming two backends, or a bucket the store
// could not address. Neither is a preference: the store is one or the other,
// and half a bucket configuration would fail at the first blob_share call
// instead of at startup.
func (c BlobConfig) validateBackend() error {
	if !c.S3.Enabled() {
		return nil
	}
	if c.Addr != "" || c.BaseURL != "" || c.Dir != "" {
		return errors.New("blob: s3.endpoint and addr/base_url/dir are two different backends: configure one or the other")
	}
	if c.S3.Bucket == "" {
		return errors.New("blob: s3.endpoint needs s3.bucket")
	}
	return nil
}

// prefix is the path the mount answers to, defaulting to its own directory.
func (m BlobMountConfig) prefix() string {
	if m.Prefix != "" {
		return m.Prefix
	}
	return m.Dir
}

// validate checks the blob section. Mounts without a store are an error rather
// than dead config: they read as "these directories are shared" and would
// silently do nothing.
func (c BlobConfig) validate() error {
	if !c.Enabled() {
		if len(c.Mounts) > 0 {
			return errors.New("blob: mounts are configured but neither addr nor s3.endpoint is, so nothing would serve them")
		}
		return nil
	}
	if _, err := parseOptionalDuration(c.TTL); err != nil {
		return fmt.Errorf("blob: ttl: %w", err)
	}
	if err := c.validateBackend(); err != nil {
		return err
	}

	seen := map[string]bool{}
	for i, m := range c.Mounts {
		if m.Name == "" {
			return fmt.Errorf("blob.mount[%d]: name is required", i)
		}
		if seen[m.Name] {
			return fmt.Errorf("blob.mount name %q duplicated", m.Name)
		}
		seen[m.Name] = true
		if m.Dir == "" {
			return fmt.Errorf("blob.mount %q: dir is required", m.Name)
		}
		// A relative directory would resolve against the gateway's working
		// directory, which is not something an operator can reason about from
		// the config file.
		if !filepath.IsAbs(m.Dir) {
			return fmt.Errorf("blob.mount %q: dir must be absolute, got %q", m.Name, m.Dir)
		}
		if m.Prefix != "" && !path.IsAbs(m.Prefix) {
			return fmt.Errorf("blob.mount %q: prefix must be absolute, got %q", m.Name, m.Prefix)
		}
	}
	return nil
}

// setDefaults applies server name and telemetry defaults, and resolves the
// per-upstream lazy flag against the server-wide default. It is idempotent.
func (c *Config) setDefaults() {
	if c.Server.Name == "" {
		c.Server.Name = "mcpgateway"
	}
	for i := range c.Upstreams {
		if c.Upstreams[i].Tools.Lazy == nil {
			c.Upstreams[i].Tools.Lazy = new(c.Server.LazyTools)
		}
	}
}

// ServerConfig configures the gateway's own MCP server identity.
type ServerConfig struct {
	Name         string `toml:"name"`
	Instructions string `toml:"instructions"`
	// LazyTools is the default for every upstream's tools.lazy, which an
	// upstream may still override in either direction.
	LazyTools bool `toml:"lazy_tools"`
	// DrainTimeout bounds how long a closing upstream waits for its in-flight
	// calls, on both reload and shutdown. Empty uses [defaultDrainTimeout];
	// negative disables draining.
	DrainTimeout string `toml:"drain_timeout"`
}

// UpstreamConfig describes one upstream MCP server to proxy.
type UpstreamConfig struct {
	Name         string            `toml:"name"`
	Kind         string            `toml:"kind"`
	Command      []string          `toml:"command"`
	URL          string            `toml:"url"`
	Headers      map[string]string `toml:"headers"`
	StripHeaders []string          `toml:"strip_headers"`
	Env          map[string]string `toml:"env"`
	Tools        ToolsConfig       `toml:"tools"`
	Route        RouteConfig       `toml:"route"`
	Reconnect    *ReconnectConfig  `toml:"reconnect"`
	// CallTimeout bounds a single request to this upstream. Empty means no
	// limit, which is what an upstream with genuinely long-running tools needs.
	// It is unrelated to [ServerConfig.DrainTimeout]: that one bounds shutdown,
	// and applies however long a call is allowed to run.
	CallTimeout string `toml:"call_timeout"`
	// Redact overrides the global redact config when present; nil inherits the global [redact] section.
	Redact *RedactConfig `toml:"redact"`
}

// AuthConfig configures optional inbound HTTP authentication for the gateway.
type AuthConfig struct {
	Enabled bool        `toml:"enabled"`
	Header  string      `toml:"header"`
	Value   string      `toml:"value"`
	OAuth   OAuthConfig `toml:"oauth"`
}

// OAuthConfig configures an optional local OAuth authorization-code facade for inbound clients.
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

// RouteConfig optionally exposes an upstream as its own MCP server on a host
// and/or URL path prefix handled by the gateway HTTP transport.
type RouteConfig struct {
	Host string `toml:"host"`
	Path string `toml:"path"`
}

// ReconnectConfig configures per-upstream reconnect supervision.
type ReconnectConfig struct {
	KeepAlive      string `toml:"keepalive"`
	InitialBackoff string `toml:"initial_backoff"`
	MaxBackoff     string `toml:"max_backoff"`
}

// ToolsConfig controls tool filtering, namespacing and description trimming for an upstream.
type ToolsConfig struct {
	Allow   []string      `toml:"allow"`
	Deny    []string      `toml:"deny"`
	Prefix  string        `toml:"prefix"`
	DescMax int           `toml:"desc_max"`
	Scopes  []ScopeConfig `toml:"scope"`
	// Lazy omits this upstream's tools from tools/list on the aggregate server
	// without blocking tools/call: a client finds them via the gateway's
	// [searchToolsName]/[describeToolsName] tools and calls them directly.
	// Enabling it anywhere enables those two tools. Routed per-upstream
	// endpoints are unaffected.
	//
	// nil inherits [ServerConfig.LazyTools], which [Config.setDefaults]
	// resolves; use [ToolsConfig.lazy] to read the effective value.
	Lazy *bool `toml:"lazy"`
}

// lazy reports the effective lazy flag, treating an unresolved (nil) one as off.
func (t ToolsConfig) lazy() bool {
	return t.Lazy != nil && *t.Lazy
}

// anyLazy reports whether any upstream ends up with lazy tool listing, which is
// what enables the gateway's discovery tools.
func (c *Config) anyLazy() bool {
	for _, u := range c.Upstreams {
		if u.Tools.lazy() {
			return true
		}
	}
	return false
}

// ScopeConfig defines a named OAuth sub-scope for an upstream, granting access to
// only the tools (matched by their unprefixed name) covered by Match. The upstream's
// base scope "mcp:<upstream>" always grants every tool regardless of ScopeConfig;
// named scopes ("mcp:<upstream>:<name>") are for issuing narrower tokens.
type ScopeConfig struct {
	Name  string   `toml:"name"`
	Match []string `toml:"match"`
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

// Load reads a TOML file and decodes it via [Decode].
func Load(cfgPath string) (*Config, error) {
	data, err := os.ReadFile(cfgPath) //nolint:gosec // G304: operator-controlled config file path
	if err != nil {
		return nil, errors.Wrap(err, "read config")
	}
	return Decode(data)
}

// Decode decodes TOML, applies defaults and validates.
func Decode(data []byte) (*Config, error) {
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
	if _, err := parseOptionalDuration(c.Server.DrainTimeout); err != nil {
		return fmt.Errorf("server: drain_timeout: %w", err)
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
		if _, err := parseOptionalDuration(u.CallTimeout); err != nil {
			return fmt.Errorf("upstream %q: call_timeout: %w", u.Name, err)
		}
		if err := validateRouteConfig(u.Name, u.Route); err != nil {
			return err
		}
		if err := validateScopeConfigs(u.Name, u.Tools.Scopes); err != nil {
			return err
		}
	}
	if err := validateRouteCollisions(c.Upstreams); err != nil {
		return err
	}
	if err := c.Blob.validate(); err != nil {
		return err
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
		for _, h := range u.StripHeaders {
			if h == "" {
				joinErrs = append(joinErrs, fmt.Errorf("upstream %q: strip_headers contains empty header name", u.Name))
			}
		}
		if u.Redact != nil && u.Redact.Enabled && len(u.Redact.Patterns) > 0 {
			if _, err := NewRedactor(u.Redact.Patterns, u.Redact.MinEntropy); err != nil {
				joinErrs = append(joinErrs, errors.Wrapf(err, "upstream %q: compile redact patterns", u.Name))
			}
		}
	}
	if c.Auth.Enabled {
		if c.Auth.Header == "" {
			joinErrs = append(joinErrs, errors.New("auth: header is required when enabled"))
		}
		if c.Auth.Value == "" {
			joinErrs = append(joinErrs, errors.New("auth: value is required when enabled"))
		}
		for name := range extractSecretRefs(c.Auth.Value) {
			if !seenSec[name] {
				joinErrs = append(joinErrs, fmt.Errorf("auth: secret %q referenced in value is not defined", name))
			}
		}
		if c.Auth.OAuth.Enabled {
			if c.Auth.OAuth.Issuer == "" {
				joinErrs = append(joinErrs, errors.New("auth.oauth: issuer is required when enabled"))
			}
			if c.Auth.OAuth.Resource == "" {
				joinErrs = append(joinErrs, errors.New("auth.oauth: resource is required when enabled"))
			}
			if len(c.Auth.OAuth.RedirectURIs) == 0 {
				joinErrs = append(joinErrs, errors.New("auth.oauth: redirect_uris is required when enabled"))
			}
			if _, err := parseOptionalDuration(c.Auth.OAuth.TokenTTL); err != nil {
				joinErrs = append(joinErrs, fmt.Errorf("auth.oauth: token_ttl: %w", err))
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

func validateRouteConfig(upstream string, cfg RouteConfig) error {
	if cfg.Host == "" && cfg.Path == "" {
		return nil
	}
	if strings.Contains(cfg.Host, "://") || strings.Contains(cfg.Host, "/") {
		return fmt.Errorf("upstream %q: route.host must be a host name, not a URL", upstream)
	}
	if strings.Contains(cfg.Host, ":") {
		return fmt.Errorf("upstream %q: route.host must not contain a port", upstream)
	}
	if cfg.Path != "" && !strings.HasPrefix(cfg.Path, "/") {
		return fmt.Errorf("upstream %q: route.path must start with /", upstream)
	}
	return nil
}

func validateScopeConfigs(upstream string, scopes []ScopeConfig) error {
	seen := map[string]bool{}
	for _, sc := range scopes {
		if sc.Name == "" {
			return fmt.Errorf("upstream %q: tools.scope: name is required", upstream)
		}
		if strings.Contains(sc.Name, ":") {
			return fmt.Errorf("upstream %q: tools.scope %q: name must not contain ':'", upstream, sc.Name)
		}
		if seen[sc.Name] {
			return fmt.Errorf("upstream %q: tools.scope %q duplicated", upstream, sc.Name)
		}
		seen[sc.Name] = true
		if len(sc.Match) == 0 {
			return fmt.Errorf("upstream %q: tools.scope %q: match is required", upstream, sc.Name)
		}
		for _, pat := range sc.Match {
			if _, err := path.Match(pat, ""); err != nil {
				return fmt.Errorf("upstream %q: tools.scope %q: invalid match pattern %q: %w", upstream, sc.Name, pat, err)
			}
		}
	}
	return nil
}

func validateRouteCollisions(upstreams []UpstreamConfig) error {
	seen := map[RouteConfig]string{}
	for _, u := range upstreams {
		if u.Route.Host == "" && u.Route.Path == "" {
			continue
		}
		prev, ok := seen[u.Route]
		if ok {
			return fmt.Errorf("upstream %q: route duplicates upstream %q", u.Name, prev)
		}
		seen[u.Route] = u.Name
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
