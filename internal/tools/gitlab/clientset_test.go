package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// newTestClientSet builds a set against a GitLab that records the credential of
// every request it serves.
func newTestClientSet(t *testing.T, cfg Config) (cs *ClientSet, tokens *[]string) {
	t.Helper()

	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("PRIVATE-TOKEN"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id": 1, "iid": 1, "title": "t"}`))
	}))
	t.Cleanup(srv.Close)

	cfg.BaseURL = srv.URL
	cfg.HTTPClient = srv.Client()

	cs, err := NewClientSet(cfg)
	require.NoError(t, err)
	return cs, &seen
}

// call makes any request through c, so the token it authenticates with lands in
// the recorder.
func call(t *testing.T, c *Client) {
	t.Helper()
	_, _, err := viewIssueHandler(c)(context.Background(), &mcp.CallToolRequest{}, ViewIssueArgs{Project: "g/p", IID: 1})
	require.NoError(t, err)
}

func TestClientSet(t *testing.T) {
	t.Run("rejects a malformed base URL up front", func(t *testing.T) {
		_, err := NewClientSet(Config{BaseURL: "not-a-url"})
		require.Error(t, err)
	})

	t.Run("reuses a client per token", func(t *testing.T) {
		cs, _ := newTestClientSet(t, Config{})

		a, err := cs.For("glpat-a")
		require.NoError(t, err)
		b, err := cs.For("glpat-a")
		require.NoError(t, err)
		require.Same(t, a, b)

		other, err := cs.For("glpat-b")
		require.NoError(t, err)
		require.NotSame(t, a, other)
	})

	t.Run("shares one HTTP client across tokens", func(t *testing.T) {
		cs, _ := newTestClientSet(t, Config{})

		a, err := cs.For("glpat-a")
		require.NoError(t, err)
		b, err := cs.For("glpat-b")
		require.NoError(t, err)
		require.Same(t, a.http, b.http)
	})

	t.Run("each client sends its own token", func(t *testing.T) {
		cs, seen := newTestClientSet(t, Config{})

		a, err := cs.For("glpat-a")
		require.NoError(t, err)
		b, err := cs.For("glpat-b")
		require.NoError(t, err)

		call(t, a)
		call(t, b)
		call(t, a)
		require.Equal(t, []string{"glpat-a", "glpat-b", "glpat-a"}, *seen)
	})

	t.Run("drops the cache past the bound", func(t *testing.T) {
		cs, _ := newTestClientSet(t, Config{})

		first, err := cs.For("glpat-0")
		require.NoError(t, err)
		for i := range maxCachedClients {
			_, err := cs.For(string(rune('a'+i%26)) + string(rune('a'+i/26)))
			require.NoError(t, err)
		}
		require.LessOrEqual(t, len(cs.clients), maxCachedClients)

		// Evicted, so rebuilt rather than returned from the cache.
		again, err := cs.For("glpat-0")
		require.NoError(t, err)
		require.NotSame(t, first, again)
	})
}
