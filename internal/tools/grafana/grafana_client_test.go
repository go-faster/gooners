package grafana

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/effect"
)

// doerFunc is a fake [effect.Doer]: it lets doRequest be exercised without a
// live Grafana.
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func jsonResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}
}

func TestGrafanaClientDoRequest_FakeDoer(t *testing.T) {
	ctx := context.Background()

	for _, tt := range []struct {
		name     string
		opts     GrafanaClientOptions
		wantAuth func(t *testing.T, req *http.Request)
	}{
		{
			name: "BearerToken",
			opts: GrafanaClientOptions{URL: "http://grafana.invalid/", Token: "tok"},
			wantAuth: func(t *testing.T, req *http.Request) {
				require.Equal(t, "Bearer tok", req.Header.Get("Authorization"))
			},
		},
		{
			name: "BasicAuth",
			opts: GrafanaClientOptions{URL: "http://grafana.invalid", User: "u", Password: "p"},
			wantAuth: func(t *testing.T, req *http.Request) {
				user, pass, ok := req.BasicAuth()
				require.True(t, ok)
				require.Equal(t, "u", user)
				require.Equal(t, "p", pass)
			},
		},
		{
			name: "NoAuth",
			opts: GrafanaClientOptions{URL: "http://grafana.invalid"},
			wantAuth: func(t *testing.T, req *http.Request) {
				require.Empty(t, req.Header.Get("Authorization"))
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var seen *http.Request
			opts := tt.opts
			opts.HTTP = doerFunc(func(req *http.Request) (*http.Response, error) {
				seen = req
				return jsonResponse(req, http.StatusOK, `{"uid":"prom","type":"prometheus","name":"Prometheus"}`), nil
			})

			info, err := NewGrafanaClient(opts).GetDatasourceByUID(ctx, "prom")
			require.NoError(t, err)
			require.Equal(t, &DatasourceInfo{UID: "prom", Type: "prometheus", Name: "Prometheus"}, info)

			require.NotNil(t, seen)
			require.Equal(t, http.MethodGet, seen.Method)
			// The trailing slash of the configured URL must not double up.
			require.Equal(t, "http://grafana.invalid/api/datasources/uid/prom", seen.URL.String())
			require.Equal(t, "application/json", seen.Header.Get("Content-Type"))
			tt.wantAuth(t, seen)
		})
	}
}

func TestGrafanaClientDoRequest_NoURL(t *testing.T) {
	_, err := NewGrafanaClient(GrafanaClientOptions{}).GetDatasourceByUID(context.Background(), "prom")
	require.ErrorContains(t, err, "base URL")
}

// TestGrafanaClientEgressPolicy proves the default client is pinned to the
// Grafana it was configured with: an unrelated host is denied.
func TestGrafanaClientEgressPolicy(t *testing.T) {
	ctx := context.Background()

	grafanaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"uid":"prom","type":"prometheus","name":"Prometheus"}`)
	}))
	t.Cleanup(grafanaSrv.Close)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(other.Close)

	c := NewGrafanaClient(GrafanaClientOptions{URL: grafanaSrv.URL})
	_, err := c.GetDatasourceByUID(ctx, "prom")
	require.NoError(t, err)

	// Point the same client at an unrelated host: the policy, not the call
	// site, is what refuses it.
	c.URL = other.URL
	_, err = c.GetDatasourceByUID(ctx, "prom")
	require.ErrorIs(t, err, effect.ErrDenied)
}
