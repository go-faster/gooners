package mcpcmd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func handler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
}

func TestRunOptionsMux(t *testing.T) {
	opts := RunOptions{
		Name:   "test",
		Routes: map[string]http.Handler{"/blob/": handler("blob")},
	}

	mux, err := opts.mux(handler("mcp"))
	require.NoError(t, err)

	for _, tt := range []struct {
		path string
		want string
	}{
		{"/blob/x.png", "blob"},
		{"/", "mcp"},
		// An unclaimed path still belongs to the MCP handler, which owns the
		// root: extra routes carve out prefixes, they do not take over routing.
		{"/anything", "mcp"},
	} {
		t.Run(tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, http.NoBody))
			require.Equal(t, tt.want, rec.Body.String())
		})
	}
}

// ServeMux panics on a duplicate pattern, so a route colliding with one Run
// mounts itself has to be caught before it is registered.
func TestRunOptionsMuxReserved(t *testing.T) {
	for _, pattern := range reservedRoutes {
		t.Run(pattern, func(t *testing.T) {
			opts := RunOptions{
				Name:   "test",
				Routes: map[string]http.Handler{pattern: handler("mine")},
			}

			_, err := opts.mux(handler("mcp"))
			require.ErrorContains(t, err, pattern)
		})
	}
}

func TestRunOptionsMuxHealth(t *testing.T) {
	opts := RunOptions{Name: "test"}
	mux, err := opts.mux(handler("mcp"))
	require.NoError(t, err)

	for _, path := range []string{"/health", "/readyz"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
		require.Equal(t, http.StatusOK, rec.Code)
		require.NotEqual(t, "mcp", rec.Body.String(), "%s fell through to the MCP handler", path)
	}
}
