// Package alertmanager registers MCP tools for reading Alertmanager alerts,
// silences, receivers and status, plus a guarded silence-creation workflow
// and PromQL validate/evaluate helpers.
package alertmanager

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	httptransport "github.com/go-openapi/runtime/client"
	"github.com/go-openapi/strfmt"
	amclient "github.com/prometheus/alertmanager/api/v2/client"
	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"

	"github.com/go-faster/gooners/internal/effect"
)

const (
	defaultAlertmanagerBasePath = "/api/v2"
	// DefaultMaxSilenceDuration is used when Config.MaxSilenceDuration is unset.
	DefaultMaxSilenceDuration = 24 * time.Hour
)

// Config configures the Alertmanager and (optionally) Prometheus API clients.
type Config struct {
	AlertmanagerURL      string
	AlertmanagerToken    string
	AlertmanagerUser     string
	AlertmanagerPassword string

	// PrometheusURL is optional; only evaluate_promql_query needs it.
	PrometheusURL      string
	PrometheusToken    string
	PrometheusUser     string
	PrometheusPassword string

	// MaxSilenceDuration caps how long a single create_silence call may
	// request. Zero means DefaultMaxSilenceDuration.
	MaxSilenceDuration time.Duration

	Timeout time.Duration

	TLSCAFile             string
	TLSCertFile           string
	TLSKeyFile            string
	TLSInsecureSkipVerify bool

	// HTTPClient performs every Alertmanager and Prometheus request. If nil, it
	// is an [effect.NewHTTPClient] whose egress allowlist is exactly the two
	// configured upstreams, so neither API client can reach anything else.
	//
	// It exists as a seam for tests; production code leaves it nil.
	HTTPClient *http.Client
}

func (c *Config) setDefaults() {
	if c.MaxSilenceDuration <= 0 {
		c.MaxSilenceDuration = DefaultMaxSilenceDuration
	}
	if c.Timeout <= 0 {
		c.Timeout = 15 * time.Second
	}
}

// Client wraps the generated Alertmanager API v2 client and an optional
// Prometheus v1 API client used for PromQL evaluation.
type Client struct {
	am  *amclient.AlertmanagerAPI
	cfg Config

	// prom is nil when Config.PrometheusURL is empty.
	prom promv1.API
}

// NewClient builds a Client from cfg. It returns an error if AlertmanagerURL
// is missing or malformed, or if PrometheusURL is set but malformed.
func NewClient(cfg Config) (*Client, error) {
	cfg.setDefaults()

	if cfg.AlertmanagerURL == "" {
		return nil, fmt.Errorf("alertmanager URL is not configured")
	}
	u, err := url.Parse(cfg.AlertmanagerURL)
	if err != nil {
		return nil, fmt.Errorf("parse alertmanager URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("alertmanager URL must be absolute (e.g. http://localhost:9093), got %q", cfg.AlertmanagerURL)
	}

	basePath := strings.TrimSuffix(u.Path, "/")
	if basePath == "" {
		basePath = defaultAlertmanagerBasePath
	}

	tlsConfig, err := cfg.tlsConfig()
	if err != nil {
		return nil, err
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = effect.NewHTTPClient(effect.HTTPOptions{
			Policy:          effect.HTTPPolicy{AllowHosts: cfg.allowHosts()},
			Timeout:         cfg.Timeout,
			TLSClientConfig: tlsConfig,
		})
	}
	rt := httptransport.NewWithClient(u.Host, basePath+"/", []string{u.Scheme}, httpClient)
	switch {
	case cfg.AlertmanagerToken != "":
		rt.DefaultAuthentication = httptransport.BearerToken(cfg.AlertmanagerToken)
	case cfg.AlertmanagerUser != "" || cfg.AlertmanagerPassword != "":
		rt.DefaultAuthentication = httptransport.BasicAuth(cfg.AlertmanagerUser, cfg.AlertmanagerPassword)
	}

	c := &Client{
		am:  amclient.New(rt, strfmt.Default),
		cfg: cfg,
	}

	if cfg.PrometheusURL != "" {
		promClient, err := promapi.NewClient(promapi.Config{
			Address:      cfg.PrometheusURL,
			RoundTripper: promRoundTripper(cfg, httpClient.Transport),
		})
		if err != nil {
			return nil, fmt.Errorf("create prometheus client: %w", err)
		}
		c.prom = promv1.NewAPI(promClient)
	}

	return c, nil
}

func (c Config) tlsConfig() (*tls.Config, error) {
	if c.TLSCAFile == "" && c.TLSCertFile == "" && c.TLSKeyFile == "" && !c.TLSInsecureSkipVerify {
		return nil, nil
	}

	cfg := &tls.Config{InsecureSkipVerify: c.TLSInsecureSkipVerify} //nolint:gosec // Explicit user-controlled upstream option.
	if c.TLSCAFile != "" {
		pem, err := os.ReadFile(c.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read upstream CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("parse upstream CA file: no certificates found")
		}
		cfg.RootCAs = pool
	}
	if c.TLSCertFile != "" || c.TLSKeyFile != "" {
		if c.TLSCertFile == "" || c.TLSKeyFile == "" {
			return nil, fmt.Errorf("both upstream client certificate and key files must be configured")
		}
		cert, err := tls.LoadX509KeyPair(c.TLSCertFile, c.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load upstream client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// allowHosts is the egress allowlist: the two upstreams this client is
// configured for, and nothing else. An unset PrometheusURL contributes no
// entry.
func (c Config) allowHosts() []string {
	return append(effect.AllowHostOf(c.AlertmanagerURL), effect.AllowHostOf(c.PrometheusURL)...)
}

// promRoundTripper adds Prometheus auth on top of base, which is the policed
// transport of the shared HTTP client: the Prometheus API client is subject to
// the same egress allowlist as the Alertmanager one.
func promRoundTripper(cfg Config, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	switch {
	case cfg.PrometheusToken != "":
		return &bearerAuthTransport{token: cfg.PrometheusToken, base: base}
	case cfg.PrometheusUser != "" || cfg.PrometheusPassword != "":
		return &basicAuthTransport{username: cfg.PrometheusUser, password: cfg.PrometheusPassword, base: base}
	default:
		return base
	}
}

type basicAuthTransport struct {
	username, password string
	base               http.RoundTripper
}

func (t *basicAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.SetBasicAuth(t.username, t.password)
	return t.base.RoundTrip(r)
}

type bearerAuthTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}

// HasPrometheus reports whether evaluate_promql_query can run.
func (c *Client) HasPrometheus() bool {
	return c.prom != nil
}

// Prometheus returns the Prometheus v1 API client, if configured.
func (c *Client) Prometheus() promv1.API {
	return c.prom
}
