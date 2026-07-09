// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGatewayHTTPMiddlewareDisabled(t *testing.T) {
	g, err := New(&Config{}, Options{})
	require.NoError(t, err)
	require.Nil(t, g.HTTPMiddleware())
}

func TestGatewayHTTPMiddlewareAuth(t *testing.T) {
	g, err := New(&Config{
		Auth: AuthConfig{
			Enabled: true,
			Header:  "Authorization",
			Value:   "Bearer secret",
		},
	}, Options{})
	require.NoError(t, err)
	mw := g.HTTPMiddleware()
	require.NotNil(t, mw)

	var gotHeader string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "/mcp", http.NoBody)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusNoContent, rr.Code)
	require.Empty(t, gotHeader)
}

func TestGatewayHTTPMiddlewareUnauthorized(t *testing.T) {
	g, err := New(&Config{
		Auth: AuthConfig{
			Enabled: true,
			Header:  "X-MCP-Token",
			Value:   "secret",
		},
	}, Options{})
	require.NoError(t, err)

	h := g.HTTPMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", http.NoBody))

	require.Equal(t, http.StatusUnauthorized, rr.Code)
	require.Equal(t, `Bearer realm="mcpgateway"`, rr.Header().Get("WWW-Authenticate"))
}
