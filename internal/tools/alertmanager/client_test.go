package alertmanager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewClient_UpstreamTLSInsecureSkipVerify(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v2/alerts", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	}))
	defer server.Close()

	client, err := NewClient(Config{
		AlertmanagerURL:       server.URL,
		TLSInsecureSkipVerify: true,
	})
	require.NoError(t, err)

	handler := listAlertsHandler(client)
	_, res, err := handler(context.Background(), nil, ListAlertsReq{})
	require.NoError(t, err)
	require.Equal(t, 0, res.Count)
}
