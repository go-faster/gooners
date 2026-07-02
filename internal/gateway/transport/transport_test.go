// Package gatewaytransport builds mcp.Transport implementations from gateway config.
package gatewaytransport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestBuild_Stdio(t *testing.T) {
	tr, _, err := Build(context.Background(), "stdio", []string{"true"}, "", nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, tr)
}

func TestBuild_BadKind(t *testing.T) {
	_, _, err := Build(context.Background(), "nope", nil, "", nil, nil, nil)
	require.Error(t, err)
}

func TestBuild_Interpolate(t *testing.T) {
	interp := func(s string) (string, error) {
		if s == "{secret:k}" {
			return "v", nil
		}
		return s, nil
	}
	tr, _, err := Build(context.Background(), "stdio", []string{"true"}, "", map[string]string{"X": "{secret:k}"}, nil, interp)
	require.NoError(t, err)
	require.NotNil(t, tr)
}

func TestBuild_StdioEnvInheritance(t *testing.T) {
	tr, _, err := Build(context.Background(), "stdio", []string{"true"}, "", map[string]string{"FOO": "bar"}, nil, nil)
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
	interp := func(s string) (string, error) {
		if s == "Bearer {secret:t}" {
			return "Bearer tok", nil
		}
		return s, nil
	}
	tr, _, err := Build(context.Background(), "http", nil, "http://example.invalid", nil, map[string]string{"Authorization": "Bearer {secret:t}"}, interp)
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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	req := httptest.NewRequest(http.MethodGet, srv.URL, http.NoBody)
	resp, err := hrt.RoundTrip(req)
	require.NoError(t, err)
	require.Equal(t, "Bearer tok", resp.Request.Header.Get("Authorization"))
	resp.Body.Close()
}

func TestBuild_SSENoTimeout(t *testing.T) {
	tr, _, err := Build(context.Background(), "sse", nil, "http://example.invalid", nil, nil, nil)
	require.NoError(t, err)
	sct, ok := tr.(*mcp.SSEClientTransport)
	require.True(t, ok)
	require.NotNil(t, sct.HTTPClient)
	require.Zero(t, sct.HTTPClient.Timeout)
}
