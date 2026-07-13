package gatewaytransport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/effect"
)

func TestBuild_Stdio(t *testing.T) {
	tr, _, err := Build(context.Background(), Options{Kind: "stdio", Command: []string{"true"}})
	require.NoError(t, err)
	require.NotNil(t, tr)
}

func TestBuild_BadKind(t *testing.T) {
	_, _, err := Build(context.Background(), Options{Kind: "nope"})
	require.Error(t, err)
}

func TestBuild_Interpolate(t *testing.T) {
	interp := func(s string) (string, error) {
		if s == "{secret:k}" {
			return "v", nil
		}
		return s, nil
	}
	tr, _, err := Build(context.Background(), Options{
		Kind:        "stdio",
		Command:     []string{"true"},
		Env:         map[string]string{"X": "{secret:k}"},
		Interpolate: interp,
	})
	require.NoError(t, err)
	require.NotNil(t, tr)
}

func TestBuild_StdioEnvInheritance(t *testing.T) {
	tr, _, err := Build(context.Background(), Options{
		Kind:    "stdio",
		Command: []string{"true"},
		Env:     map[string]string{"FOO": "bar"},
	})
	require.NoError(t, err)
	require.NotNil(t, tr)
	ct, ok := tr.(*mcp.CommandTransport)
	require.True(t, ok)
	require.NotNil(t, ct.Command.Env)
	env := ct.Command.Env
	foundFoo := false
	foundPath := false
	for _, kv := range env {
		if kv == "FOO=bar" {
			foundFoo = true
		}
		if len(kv) >= 5 && kv[:5] == "PATH=" {
			foundPath = true
		}
	}
	require.True(t, foundFoo, "FOO=bar must be present")
	if _, ok := os.LookupEnv("PATH"); ok {
		require.True(t, foundPath, "PATH= must be present when PATH in parent")
	}
}

func TestBuild_HTTPMultiTokenInterpolation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	interp := func(s string) (string, error) {
		if s == "Bearer {secret:t}" {
			return "Bearer tok", nil
		}
		return s, nil
	}
	tr, _, err := Build(context.Background(), Options{
		Kind:        "http",
		URL:         srv.URL,
		Headers:     map[string]string{"Authorization": "Bearer {secret:t}"},
		Interpolate: interp,
	})
	require.NoError(t, err)
	require.NotNil(t, tr)
	sct, ok := tr.(*mcp.StreamableClientTransport)
	require.True(t, ok)
	require.NotNil(t, sct.HTTPClient)
	cl := sct.HTTPClient
	require.NotNil(t, cl.Transport)
	require.Zero(t, cl.Timeout)
	hrt, ok := cl.Transport.(*headerRT)
	require.True(t, ok)

	req := httptest.NewRequest(http.MethodGet, srv.URL, http.NoBody)
	resp, err := hrt.RoundTrip(req)
	require.NoError(t, err)
	require.Equal(t, "Bearer tok", resp.Request.Header.Get("Authorization"))
	resp.Body.Close()
}

func TestBuild_SSENoTimeout(t *testing.T) {
	tr, _, err := Build(context.Background(), Options{Kind: "sse", URL: "http://example.invalid"})
	require.NoError(t, err)
	sct, ok := tr.(*mcp.SSEClientTransport)
	require.True(t, ok)
	require.NotNil(t, sct.HTTPClient)
	require.Zero(t, sct.HTTPClient.Timeout)
}

func TestBuild_HTTPStripHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	tr, _, err := Build(context.Background(), Options{
		Kind:         "http",
		URL:          srv.URL,
		Headers:      map[string]string{"X-Upstream": "ok"},
		StripHeaders: []string{"Authorization"},
	})
	require.NoError(t, err)
	sct, ok := tr.(*mcp.StreamableClientTransport)
	require.True(t, ok)
	hrt, ok := sct.HTTPClient.Transport.(*headerRT)
	require.True(t, ok)

	req := httptest.NewRequest(http.MethodGet, srv.URL, http.NoBody)
	req.Header.Set("Authorization", "Bearer gateway")
	req.Header.Set("X-Upstream", "client")
	resp, err := hrt.RoundTrip(req)
	require.NoError(t, err)
	require.Empty(t, resp.Request.Header.Get("Authorization"))
	require.Equal(t, "ok", resp.Request.Header.Get("X-Upstream"))
	resp.Body.Close()
}

// TestBuild_HTTPEgressPolicy proves the default client is pinned to its own
// upstream: a request to another host is denied before it leaves the process.
func TestBuild_HTTPEgressPolicy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(other.Close)

	tr, _, err := Build(context.Background(), Options{Kind: "http", URL: upstream.URL})
	require.NoError(t, err)
	sct, ok := tr.(*mcp.StreamableClientTransport)
	require.True(t, ok)
	cl := sct.HTTPClient

	ctx := context.Background()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, http.NoBody)
	require.NoError(t, err)
	resp, err := cl.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, other.URL, http.NoBody)
	require.NoError(t, err)
	_, err = cl.Do(req) //nolint:bodyclose // Request is denied, there is no body.
	require.ErrorIs(t, err, effect.ErrDenied)
}

// TestBuild_HTTPInjectedClient checks the caller's client is used as-is, with
// header injection composed on top of its transport rather than replacing it.
func TestBuild_HTTPInjectedClient(t *testing.T) {
	seen := 0
	base := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		seen++
		require.Equal(t, "ok", req.Header.Get("X-Upstream"))
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
	})
	cl := &http.Client{Transport: base}

	tr, _, err := Build(context.Background(), Options{
		Kind:       "http",
		URL:        "http://example.invalid",
		Headers:    map[string]string{"X-Upstream": "ok"},
		HTTPClient: cl,
	})
	require.NoError(t, err)
	sct, ok := tr.(*mcp.StreamableClientTransport)
	require.True(t, ok)
	require.NotSame(t, cl, sct.HTTPClient, "caller's client must not be mutated")
	require.IsType(t, roundTripperFunc(nil), cl.Transport, "caller's transport must not be replaced")

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.invalid", http.NoBody)
	require.NoError(t, err)
	resp, err := sct.HTTPClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 1, seen)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
