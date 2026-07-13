package alertmanager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigAllowHosts(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  Config
		want []string
	}{
		{"Empty", Config{}, nil},
		{
			"AlertmanagerOnly",
			Config{AlertmanagerURL: "http://am.invalid:9093"},
			[]string{"am.invalid:9093"},
		},
		{
			"BothUpstreams",
			Config{AlertmanagerURL: "http://am.invalid:9093/api/v2", PrometheusURL: "https://prom.invalid"},
			[]string{"am.invalid:9093", "prom.invalid"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.cfg.allowHosts())
		})
	}
}

// TestNewClient_EgressPolicy proves the client is pinned to its configured
// upstreams: an Alertmanager redirecting to an unrelated host is not followed.
func TestNewClient_EgressPolicy(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer other.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/api/v2/alerts", http.StatusFound)
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	_, _, err = listAlertsHandler(client)(context.Background(), nil, ListAlertsReq{})
	require.ErrorContains(t, err, "not in the egress allowlist")
}

// TestNewClient_InjectedHTTPClient checks the Config.HTTPClient seam is used.
func TestNewClient_InjectedHTTPClient(t *testing.T) {
	calls := 0
	client, err := NewClient(Config{
		AlertmanagerURL: "http://am.invalid",
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       http.NoBody,
				Request:    req,
			}, nil
		})},
	})
	require.NoError(t, err)

	_, res, err := listAlertsHandler(client)(context.Background(), nil, ListAlertsReq{})
	require.NoError(t, err)
	require.Equal(t, 0, res.Count)
	require.Equal(t, 1, calls)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestNewClient_UpstreamTLSInsecureSkipVerify(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v2/alerts", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	}))
	defer server.Close()

	client, err := NewClient(Config{
		AlertmanagerURL:       server.URL,
		TLSInsecureSkipVerify: true,
	})
	require.NoError(t, err)

	handler := listAlertsHandler(client)
	_, res, err := handler(context.Background(), nil, ListAlertsReq{})
	require.NoError(t, err)
	require.Equal(t, 0, res.Count)
}
