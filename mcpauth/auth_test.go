package mcpauth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/mcpauth"
)

func staticConfig() mcpauth.Config {
	return mcpauth.Config{
		Enabled: true,
		Header:  "Authorization",
		Value:   "Bearer sekret",
	}
}

// seen records what the guarded handler was reached with, so a test can assert
// both that a request got through and what it looked like when it did.
type seen struct {
	called bool
	header string
}

func guard(t *testing.T, cfg mcpauth.Config, opts mcpauth.Options) (http.Handler, *seen) {
	t.Helper()

	mw := mcpauth.Middleware(cfg, opts)
	require.NotNil(t, mw, "middleware for an enabled config")

	var got seen

	return mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got.called = true
		got.header = r.Header.Get(cfg.Header)
	})), &got
}

func TestMiddlewareDisabled(t *testing.T) {
	// Nil rather than a pass-through, so a caller that means to require auth
	// can tell the difference.
	require.Nil(t, mcpauth.Middleware(mcpauth.Config{}, mcpauth.Options{}))
}

func TestMiddlewareStaticSecret(t *testing.T) {
	for _, tt := range []struct {
		name   string
		header string
		want   int
	}{
		{"correct", "Bearer sekret", http.StatusOK},
		{"wrong", "Bearer nope", http.StatusUnauthorized},
		{"missing", "", http.StatusUnauthorized},
		// A prefix of the real credential must not pass: the comparison is over
		// digests, not a truncated match.
		{"prefix", "Bearer sek", http.StatusUnauthorized},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, got := guard(t, staticConfig(), mcpauth.Options{Name: "tgmcp"})

			req := httptest.NewRequest(http.MethodPost, "/", http.NoBody)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			require.Equal(t, tt.want, rec.Code)
			require.Equal(t, tt.want == http.StatusOK, got.called)
		})
	}
}

// The credential must not reach the handler behind the middleware: it grants
// full access, and anything downstream that logs headers would publish it.
func TestMiddlewareStripsCredential(t *testing.T) {
	h, got := guard(t, staticConfig(), mcpauth.Options{})

	req := httptest.NewRequest(http.MethodPost, "/", http.NoBody)
	req.Header.Set("Authorization", "Bearer sekret")
	h.ServeHTTP(httptest.NewRecorder(), req)

	require.True(t, got.called)
	require.Empty(t, got.header, "credential reached the guarded handler")
}

func TestMiddlewareRealm(t *testing.T) {
	h, _ := guard(t, staticConfig(), mcpauth.Options{Name: "tgmcp"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", http.NoBody))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, `Bearer realm="tgmcp"`, rec.Header().Get("WWW-Authenticate"))
}

// An unreachable secret store must fail closed, and must not read as a wrong
// credential either: 503 says the server cannot decide, 401 says it decided no.
func TestMiddlewareSecretUnavailable(t *testing.T) {
	h, got := guard(t, staticConfig(), mcpauth.Options{
		Secret: func(context.Context) (string, error) {
			return "", errors.New("vault is down")
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/", http.NoBody)
	req.Header.Set("Authorization", "Bearer sekret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.False(t, got.called)
}

// The resolver is consulted per request, so a rotated secret takes effect
// without rebuilding the middleware.
func TestMiddlewareSecretPerRequest(t *testing.T) {
	current := "Bearer first"
	h, _ := guard(t, staticConfig(), mcpauth.Options{
		Secret: func(context.Context) (string, error) { return current, nil },
	})

	do := func(header string) int {
		req := httptest.NewRequest(http.MethodPost, "/", http.NoBody)
		req.Header.Set("Authorization", header)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		return rec.Code
	}

	require.Equal(t, http.StatusOK, do("Bearer first"))

	current = "Bearer second"
	require.Equal(t, http.StatusUnauthorized, do("Bearer first"))
	require.Equal(t, http.StatusOK, do("Bearer second"))
}

func TestConfigValidate(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  mcpauth.Config
		want []string
	}{
		{
			name: "disabled is always valid",
			cfg:  mcpauth.Config{Header: "", Value: ""},
		},
		{
			name: "enabled needs a header and a value",
			cfg:  mcpauth.Config{Enabled: true},
			want: []string{"auth: header is required", "auth: value is required"},
		},
		{
			name: "static secret alone is enough",
			cfg:  staticConfig(),
		},
		{
			name: "oauth needs an issuer, a resource and redirects",
			cfg: mcpauth.Config{
				Enabled: true, Header: "Authorization", Value: "x",
				OAuth: mcpauth.OAuthConfig{Enabled: true},
			},
			want: []string{"issuer is required", "resource is required", "redirect_uris is required"},
		},
		{
			name: "oauth token_ttl must parse",
			cfg: mcpauth.Config{
				Enabled: true, Header: "Authorization", Value: "x",
				OAuth: mcpauth.OAuthConfig{
					Enabled:      true,
					Issuer:       "https://example.com",
					Resource:     "https://example.com/mcp",
					RedirectURIs: []string{"https://example.com/cb"},
					TokenTTL:     "half an hour",
				},
			},
			want: []string{"token_ttl"},
		},
		{
			name: "disabled oauth ignores its own blanks",
			cfg: mcpauth.Config{
				Enabled: true, Header: "Authorization", Value: "x",
				OAuth: mcpauth.OAuthConfig{TokenTTL: "nonsense"},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if len(tt.want) == 0 {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			for _, want := range tt.want {
				require.ErrorContains(t, err, want)
			}
		})
	}
}
