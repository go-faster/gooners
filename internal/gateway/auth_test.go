// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGatewayHTTPMiddlewareDisabled(t *testing.T) {
	g, err := New(&Config{}, Options{})
	require.NoError(t, err)
	require.Nil(t, g.HTTPMiddleware())
}

func TestGatewayOAuthMetadata(t *testing.T) {
	g, err := New(&Config{
		Auth: AuthConfig{
			Enabled: true,
			Header:  "Authorization",
			Value:   "Bearer secret",
			OAuth: OAuthConfig{
				Enabled:  true,
				Issuer:   "https://mcp.example.com",
				Resource: "https://mcp.example.com/mcp",
				Scopes:   []string{"mcp"},
			},
		},
	}, Options{})
	require.NoError(t, err)

	h := g.HTTPMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", http.NoBody))

	require.Equal(t, http.StatusOK, rr.Code)
	require.JSONEq(t, `{
		"resource":"https://mcp.example.com/mcp",
		"authorization_servers":["https://mcp.example.com"],
		"scopes_supported":["mcp"],
		"bearer_methods_supported":["header"],
		"resource_name":"mcpgateway"
	}`, rr.Body.String())
}

func TestGatewayOAuthMetadata_DerivedUpstreamScopes(t *testing.T) {
	g, err := New(&Config{
		Upstreams: []UpstreamConfig{
			{
				Name: "grafana",
				Tools: ToolsConfig{
					Scopes: []ScopeConfig{
						{Name: "read", Match: []string{"get_*", "list_*"}},
						{Name: "write", Match: []string{"add_*"}},
					},
				},
			},
			{Name: "github"},
		},
		Auth: AuthConfig{
			Enabled: true,
			Header:  "Authorization",
			Value:   "Bearer secret",
			OAuth: OAuthConfig{
				Enabled:  true,
				Issuer:   "https://mcp.example.com",
				Resource: "https://mcp.example.com/mcp",
			},
		},
	}, Options{})
	require.NoError(t, err)

	h := g.HTTPMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", http.NoBody))

	require.Equal(t, http.StatusOK, rr.Code)
	var body struct {
		ScopesSupported []string `json:"scopes_supported"`
	}
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	require.ElementsMatch(t, []string{
		"mcp:grafana", "mcp:grafana:read", "mcp:grafana:write", "mcp:github",
	}, body.ScopesSupported)
}

func TestGatewayOAuthAuthorizationCodeFlow(t *testing.T) {
	g, err := New(&Config{
		Auth: AuthConfig{
			Enabled: true,
			Header:  "Authorization",
			Value:   "Bearer secret",
			OAuth: OAuthConfig{
				Enabled:      true,
				Issuer:       "https://mcp.example.com",
				Resource:     "https://mcp.example.com/mcp",
				Scopes:       []string{"mcp"},
				ClientID:     "chatgpt",
				RedirectURIs: []string{"https://chatgpt.com/connector/oauth/test"},
			},
		},
	}, Options{})
	require.NoError(t, err)

	called := false
	h := g.HTTPMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	verifier := "test-verifier"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	authReq := url.Values{
		"response_type":         {"code"},
		"client_id":             {"chatgpt"},
		"redirect_uri":          {"https://chatgpt.com/connector/oauth/test"},
		"state":                 {"state-1"},
		"scope":                 {"mcp"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	form := url.Values{
		"query": {authReq.Encode()},
		"token": {"secret"},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rr, req)
	require.Equal(t, http.StatusFound, rr.Code)
	location := rr.Header().Get("Location")
	redir, err := url.Parse(location)
	require.NoError(t, err)
	code := redir.Query().Get("code")
	require.NotEmpty(t, code)
	require.Equal(t, "state-1", redir.Query().Get("state"))

	form = url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {"chatgpt"},
		"redirect_uri":  {"https://chatgpt.com/connector/oauth/test"},
		"code":          {code},
		"code_verifier": {verifier},
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	var tokenResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&tokenResp))
	require.NotEmpty(t, tokenResp.AccessToken)
	require.Equal(t, "Bearer", tokenResp.TokenType)

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/mcp", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)
	h.ServeHTTP(rr, req)
	require.Equal(t, http.StatusNoContent, rr.Code)
	require.True(t, called)
}

func TestGatewayOAuthAuthorizeRejectsUnregisteredRedirectURI(t *testing.T) {
	g, err := New(&Config{
		Auth: AuthConfig{
			Enabled: true,
			Header:  "Authorization",
			Value:   "Bearer secret",
			OAuth: OAuthConfig{
				Enabled:      true,
				Issuer:       "https://mcp.example.com",
				Resource:     "https://mcp.example.com/mcp",
				Scopes:       []string{"mcp"},
				ClientID:     "chatgpt",
				RedirectURIs: []string{"https://chatgpt.com/connector/oauth/test"},
			},
		},
	}, Options{})
	require.NoError(t, err)

	h := g.HTTPMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	authReq := url.Values{
		"response_type":         {"code"},
		"client_id":             {"chatgpt"},
		"redirect_uri":          {"https://attacker.example.com/callback"},
		"code_challenge":        {"challenge"},
		"code_challenge_method": {"S256"},
	}
	form := url.Values{
		"query": {authReq.Encode()},
		"token": {"secret"},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Empty(t, rr.Header().Get("Location"))
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
