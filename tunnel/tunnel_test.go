package tunnel_test

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/tunnel"
	_ "github.com/go-faster/gooners/tunnel/all"
)

// The providers must be reachable through Listen once imported, which is the
// contract the tunnel/all package and every binary's blank import rely on.
func TestProviders(t *testing.T) {
	require.Equal(t, []string{"cloudflare", "cloudflared", "ngrok"}, tunnel.Providers())
}

// A provider nobody registered has to name what is linked in: "unknown
// provider" alone reads as a typo when the real cause is a missing import.
func TestListenUnknownProvider(t *testing.T) {
	ln, err := tunnel.Listen(t.Context(), "wireguard", tunnel.Options{})
	require.Nil(t, ln)
	require.ErrorContains(t, err, `"wireguard"`)
	require.ErrorContains(t, err, "cloudflared")
}

func TestListenDefaults(t *testing.T) {
	var got tunnel.Options
	tunnel.Register(func(_ context.Context, opts tunnel.Options) (net.Listener, error) {
		got = opts

		return nil, nil //nolint:nilnil // the listener is irrelevant to what this asserts
	}, "recorder")

	_, err := tunnel.Listen(t.Context(), "recorder", tunnel.Options{})
	require.NoError(t, err)
	require.Equal(t, "http", got.Type, "type defaults to http")
	require.NotNil(t, got.Logger, "a provider must never have to nil-check the logger")
}
