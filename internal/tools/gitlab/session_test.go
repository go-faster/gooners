package gitlab

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/mcputil"
)

func newTestSessionServer(t *testing.T, mode AuthMode, serverToken string) (opts SessionServerOptions, tokens *[]string) {
	t.Helper()

	cs, seen := newTestClientSet(t, Config{Token: serverToken})
	return SessionServerOptions{
		Clients: cs,
		Mode:    mode,
		Server:  mcputil.ServerConfig{Name: "gitlab-mcp"},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, seen
}

func request(token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", http.NoBody)
	if token != "" {
		r.Header.Set("PRIVATE-TOKEN", token)
	}
	return r
}

func TestSessionServerAuth(t *testing.T) {
	for _, tt := range []struct {
		name        string
		mode        AuthMode
		serverToken string
		clientToken string
		want        string // credential the session authenticates with
		wantRefused bool
	}{
		{
			name: "server mode ignores the caller's token",
			mode: AuthServer, serverToken: "srv", clientToken: "cli", want: "srv",
		},
		{
			name: "server mode with no caller token",
			mode: AuthServer, serverToken: "srv", want: "srv",
		},
		{
			name: "optional prefers the caller's token",
			mode: AuthClientOptional, serverToken: "srv", clientToken: "cli", want: "cli",
		},
		{
			name: "optional falls back to the server's",
			mode: AuthClientOptional, serverToken: "srv", want: "srv",
		},
		{
			name: "required uses the caller's token",
			mode: AuthClientRequired, clientToken: "cli", want: "cli",
		},
		{
			name: "required refuses a session with none",
			mode: AuthClientRequired, wantRefused: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts, seen := newTestSessionServer(t, tt.mode, tt.serverToken)

			c, ok := opts.clientFor(request(tt.clientToken))
			if tt.wantRefused {
				require.False(t, ok)
				require.Nil(t, c)
				return
			}
			require.True(t, ok)

			call(t, c)
			require.Equal(t, []string{tt.want}, *seen)
		})
	}

	t.Run("required refuses stdio, which has no headers", func(t *testing.T) {
		opts, _ := newTestSessionServer(t, AuthClientRequired, "")
		_, ok := opts.clientFor(nil)
		require.False(t, ok)
	})

	t.Run("builds a server per session", func(t *testing.T) {
		opts, _ := newTestSessionServer(t, AuthClientRequired, "")
		h := NewSessionServer(opts)

		require.Nil(t, h(request("")), "a refused session yields no server")

		a, b := h(request("cli")), h(request("cli"))
		require.NotNil(t, a)
		require.NotNil(t, b)
		require.NotSame(t, a, b)
	})
}
