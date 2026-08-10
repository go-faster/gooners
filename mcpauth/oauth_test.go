package mcpauth_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/mcpauth"
)

const registeredRedirect = "https://client.example.com/cb"

func oauthConfig() mcpauth.Config {
	cfg := staticConfig()
	cfg.OAuth = mcpauth.OAuthConfig{
		Enabled:      true,
		Issuer:       "https://gw.example.com",
		Resource:     "https://gw.example.com/mcp",
		RedirectURIs: []string{registeredRedirect},
	}

	return cfg
}

// authorize posts an authorization request with the given query, as the consent
// page does, and returns the response.
func authorize(t *testing.T, query url.Values, token string) *httptest.ResponseRecorder {
	t.Helper()

	h, _ := guard(t, oauthConfig(), mcpauth.Options{Name: "gw"})

	form := url.Values{"query": {query.Encode()}, "token": {token}}
	req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

func authorizeQuery() url.Values {
	return url.Values{
		"response_type":  {"code"},
		"redirect_uri":   {registeredRedirect},
		"client_id":      {"cli"},
		"code_challenge": {"abc"},
	}
}

// An unregistered redirect_uri must never produce a Location header, on the
// success path or on any error path: that redirect is how an authorization code
// leaves for an origin the attacker controls.
func TestAuthorizeRejectsUnregisteredRedirect(t *testing.T) {
	const evil = "https://attacker.example.com/steal"

	for _, tt := range []struct {
		name  string
		mutis func(url.Values)
	}{
		{"valid request", func(url.Values) {}},
		// Each of these reaches a different error path, and every one of them
		// used to reach it holding an unvalidated redirect_uri.
		{"bad response_type", func(q url.Values) { q.Set("response_type", "token") }},
		{"unknown client", func(q url.Values) { q.Set("client_id", "other") }},
		{"no pkce", func(q url.Values) { q.Del("code_challenge") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := authorizeQuery()
			q.Set("redirect_uri", evil)
			tt.mutis(q)

			rec := authorize(t, q, "sekret")

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Empty(t, rec.Header().Get("Location"))
			require.NotContains(t, rec.Body.String(), evil)
		})
	}
}

// A registered redirect_uri still gets its error reported by redirect, which is
// what an OAuth client expects.
func TestAuthorizeErrorRedirectsToRegistered(t *testing.T) {
	q := authorizeQuery()
	q.Set("response_type", "token")
	q.Set("state", "xyz")

	rec := authorize(t, q, "sekret")

	require.Equal(t, http.StatusFound, rec.Code)

	loc, err := url.Parse(rec.Header().Get("Location"))
	require.NoError(t, err)
	require.Equal(t, "client.example.com", loc.Host)
	require.Equal(t, "unsupported_response_type", loc.Query().Get("error"))
	require.Equal(t, "xyz", loc.Query().Get("state"), "state must come back for the client to match")
}

// The gateway credential is what authorizes issuing a token, so a wrong one
// re-renders the consent page rather than handing out a code.
func TestAuthorizeWrongToken(t *testing.T) {
	rec := authorize(t, authorizeQuery(), "wrong")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Header().Get("Location"))
	require.Contains(t, rec.Body.String(), "Invalid token")
}

func TestAuthorizeIssuesCode(t *testing.T) {
	rec := authorize(t, authorizeQuery(), "sekret")

	require.Equal(t, http.StatusFound, rec.Code)

	loc, err := url.Parse(rec.Header().Get("Location"))
	require.NoError(t, err)
	require.Equal(t, "client.example.com", loc.Host)
	require.NotEmpty(t, loc.Query().Get("code"))
	require.Empty(t, loc.Query().Get("error"))
}

// Metadata is what an MCP client reads to discover the flow, and it must be
// reachable without the credential it is trying to obtain.
func TestProtectedResourceMetadata(t *testing.T) {
	h, _ := guard(t, oauthConfig(), mcpauth.Options{Name: "gw", Scopes: []string{"mcp:tgmcp"}})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", http.NoBody))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "https://gw.example.com/mcp")
	require.Contains(t, rec.Body.String(), "mcp:tgmcp", "caller-derived scopes are advertised")
	require.Contains(t, rec.Body.String(), `"resource_name":"gw"`)
}
