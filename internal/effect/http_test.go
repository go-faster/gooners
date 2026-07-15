package effect_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/effect"
)

func TestHTTPPolicyCheckURL(t *testing.T) {
	policy := effect.HTTPPolicy{AllowHosts: []string{"grafana.internal", "*.example.com", "localhost:3000"}}

	tests := []struct {
		name    string
		url     string
		allowed bool
	}{
		{"exact host", "https://grafana.internal/api/health", true},
		{"host entry allows any port", "http://grafana.internal:3000/api/health", true},
		{"host:port entry allows that port", "http://localhost:3000/api/health", true},
		{"host:port entry pins the port", "http://localhost:9090/api/health", false},
		{"case-insensitive", "https://GRAFANA.internal/", true},
		{"trailing dot", "https://grafana.internal./", true},
		{"wildcard subdomain", "https://a.example.com/", true},
		{"wildcard does not match apex", "https://example.com/", false},
		{"other host", "https://evil.test/", false},
		{"suffix confusion", "https://grafana.internal.evil.test/", false},
		{"non-http scheme", "file:///etc/passwd", false},
		{"no host", "http:///path", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.url)
			require.NoError(t, err)

			err = policy.CheckURL(u)
			if tt.allowed {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, effect.ErrDenied)
		})
	}
}

func TestHTTPPolicyZeroValueAllowsNothing(t *testing.T) {
	u, err := url.Parse("https://anything.test/")
	require.NoError(t, err)
	require.ErrorIs(t, effect.HTTPPolicy{}.CheckURL(u), effect.ErrDenied)
}

func TestHTTPPolicyBlocksMetadataIP(t *testing.T) {
	// The allowlist says yes; the address still says no. An SSRF that reaches
	// the metadata service through an allowed name must not get through.
	policy := effect.HTTPPolicy{AllowHosts: []string{"*"}}

	u, err := url.Parse("http://169.254.169.254/latest/meta-data/")
	require.NoError(t, err)
	require.ErrorIs(t, policy.CheckURL(u), effect.ErrDenied)

	allowed := effect.HTTPPolicy{AllowHosts: []string{"*"}, AllowLinkLocal: true}
	require.NoError(t, allowed.CheckURL(u))
}

func TestNewHTTPClientAllowsConfiguredUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := effect.NewHTTPClient(effect.HTTPOptions{
		Policy: effect.HTTPPolicy{AllowHosts: effect.AllowHostOf(srv.URL)},
	})

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestNewHTTPClientDeniesOtherHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := effect.NewHTTPClient(effect.HTTPOptions{
		Policy: effect.HTTPPolicy{AllowHosts: []string{"grafana.internal"}},
	})

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)
	_, err = client.Do(req)
	require.ErrorIs(t, err, effect.ErrDenied)
}

// TestNewHTTPClientDeniesRedirectOffAllowlist covers the hop the request-time
// check alone would miss: an allowed upstream answering 302 to somewhere else.
// Both test servers listen on 127.0.0.1, so this also pins the port matching:
// "the upstream" means its port, not the whole loopback interface.
func TestNewHTTPClientDeniesRedirectOffAllowlist(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("should never be reached"))
	}))
	defer elsewhere.Close()

	var redirected bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer upstream.Close()

	client := effect.NewHTTPClient(effect.HTTPOptions{
		Policy: effect.HTTPPolicy{AllowHosts: effect.AllowHostOf(upstream.URL)},
	})

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, upstream.URL, http.NoBody)
	require.NoError(t, err)
	_, err = client.Do(req)
	require.ErrorIs(t, err, effect.ErrDenied)
	require.True(t, redirected, "upstream should have been reached and issued the redirect")
}

func TestAllowHostOf(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want []string
	}{
		{"https URL keeps the port", "https://grafana.internal:3000/path", []string{"grafana.internal:3000"}},
		{"no port", "https://grafana.internal/path", []string{"grafana.internal"}},
		{"IP literal", "http://10.0.0.1:9093", []string{"10.0.0.1:9093"}},
		{"empty", "", nil},
		{"no host", "not-a-url", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, effect.AllowHostOf(tt.url))
		})
	}
}
