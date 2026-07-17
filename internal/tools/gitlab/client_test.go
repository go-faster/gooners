package gitlab

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// decodeJSON reads a request body into v.
func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// baseOf reconstructs the test server's own base URL from a request it served,
// so a fixture can hand back URLs that point back at the same server.
func baseOf(r *http.Request) string {
	return "http://" + r.Host
}

// newTestClient builds a Client against h, bypassing the egress policy the way
// Config.HTTPClient is meant to be used in tests.
func newTestClient(t *testing.T, cfg Config, h http.Handler) *Client {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	cfg.BaseURL = srv.URL
	cfg.HTTPClient = srv.Client()

	c, err := NewClient(cfg)
	require.NoError(t, err)
	return c
}

func TestNewClient(t *testing.T) {
	t.Run("defaults to gitlab.com", func(t *testing.T) {
		c, err := NewClient(Config{})
		require.NoError(t, err)
		require.Equal(t, DefaultBaseURL, c.cfg.BaseURL)
	})

	t.Run("rejects a relative URL", func(t *testing.T) {
		_, err := NewClient(Config{BaseURL: "gitlab.example.com"})
		require.ErrorContains(t, err, "must be absolute")
	})

	t.Run("denies the filesystem by default", func(t *testing.T) {
		c, err := NewClient(Config{})
		require.NoError(t, err)
		_, err = c.cfg.FS.ReadFile("anything")
		require.Error(t, err)
	})
}

func TestClientProject(t *testing.T) {
	t.Run("argument wins over the default", func(t *testing.T) {
		c := &Client{cfg: Config{DefaultProject: "group/default"}}
		got, err := c.project("group/other")
		require.NoError(t, err)
		require.Equal(t, "group/other", got)
	})

	t.Run("falls back to the default", func(t *testing.T) {
		c := &Client{cfg: Config{DefaultProject: "group/default"}}
		got, err := c.project("")
		require.NoError(t, err)
		require.Equal(t, "group/default", got)
	})

	t.Run("blank argument falls back to the default", func(t *testing.T) {
		c := &Client{cfg: Config{DefaultProject: "group/default"}}
		got, err := c.project("   ")
		require.NoError(t, err)
		require.Equal(t, "group/default", got)
	})

	t.Run("no argument and no default is an error", func(t *testing.T) {
		c := &Client{cfg: Config{}}
		_, err := c.project("")
		require.ErrorContains(t, err, "project is required")
	})
}

func TestClientWebURL(t *testing.T) {
	for _, tt := range []struct {
		name    string
		baseURL string
		path    string
		want    string
	}{
		{"leading slash on path", "https://gitlab.example.com", "/uploads/a/b.zip", "https://gitlab.example.com/uploads/a/b.zip"},
		{"no leading slash", "https://gitlab.example.com", "uploads/a/b.zip", "https://gitlab.example.com/uploads/a/b.zip"},
		{"trailing slash on base", "https://gitlab.example.com/", "/uploads/a/b.zip", "https://gitlab.example.com/uploads/a/b.zip"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{cfg: Config{BaseURL: tt.baseURL}}
			require.Equal(t, tt.want, c.webURL(tt.path))
		})
	}
}
