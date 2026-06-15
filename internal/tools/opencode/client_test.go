package opencode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	client, err := NewClient(cfg, 5*time.Second)
	require.NoError(t, err)
	return client
}

func TestClientTimeout(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-done
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(done) })
	client := newTestClient(t, Config{BaseURL: server.URL})
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	_, err := client.Health(ctx)
	require.Error(t, err)
}
