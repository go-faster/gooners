package alertmanager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetStatus_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v2/status", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		jsonBody := `{
  "versionInfo": {
    "version": "0.27.0",
    "revision": "abc123def456",
    "goVersion": "go1.21.0"
  },
  "uptime": "2024-01-01T00:00:00Z",
  "cluster": {
    "name": "am-1",
    "status": "ready",
    "peers": [
      {"name": "am-1", "address": "10.0.0.1:9094"},
      {"name": "am-2", "address": "10.0.0.2:9094"}
    ]
  },
  "config": {"original": "route:\n  receiver: default\n"}
}`
		w.Write([]byte(jsonBody))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := getStatusHandler(client)
	_, res, err := handler(context.Background(), nil, GetStatusReq{})
	require.NoError(t, err)

	// Check version info
	require.Equal(t, "0.27.0", res.Version)
	require.Equal(t, "abc123def456", res.Revision)
	require.Equal(t, "go1.21.0", res.GoVersion)

	// Check uptime is RFC3339 formatted
	require.NotEmpty(t, res.Uptime)
	// Verify it's a valid RFC3339 timestamp
	_, err = parseTimeOrNow(res.Uptime)
	require.NoError(t, err)

	// Check cluster status
	require.Equal(t, "am-1", res.Cluster.Name)
	require.Equal(t, "ready", res.Cluster.Status)
	require.Len(t, res.Cluster.Peers, 2)

	// Check peers
	require.Equal(t, "am-1", res.Cluster.Peers[0].Name)
	require.Equal(t, "10.0.0.1:9094", res.Cluster.Peers[0].Address)
	require.Equal(t, "am-2", res.Cluster.Peers[1].Name)
	require.Equal(t, "10.0.0.2:9094", res.Cluster.Peers[1].Address)

	// Check config
	require.NotEmpty(t, res.ConfigYAML)
	require.Contains(t, res.ConfigYAML, "route:")
	require.Contains(t, res.ConfigYAML, "default")
}

func TestGetStatus_NilPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Simulating an empty response
		w.Write([]byte("null"))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := getStatusHandler(client)
	_, res, err := handler(context.Background(), nil, GetStatusReq{})
	require.NoError(t, err)
	// Should return empty status without error
	require.Empty(t, res.Version)
	require.Empty(t, res.Revision)
}

func TestGetStatus_NilVersionInfo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		jsonBody := `{"cluster": {"name": "am-1"}}`
		w.Write([]byte(jsonBody))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := getStatusHandler(client)
	_, res, err := handler(context.Background(), nil, GetStatusReq{})
	require.NoError(t, err)
	require.Empty(t, res.Version)
	require.Empty(t, res.Revision)
	require.Empty(t, res.GoVersion)
}

func TestGetStatus_NilCluster(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		jsonBody := `{"versionInfo": {"version": "0.27.0"}}`
		w.Write([]byte(jsonBody))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := getStatusHandler(client)
	_, res, err := handler(context.Background(), nil, GetStatusReq{})
	require.NoError(t, err)
	require.Equal(t, "0.27.0", res.Version)
	require.Empty(t, res.Cluster.Peers)
}

func TestGetStatus_NilPeers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		jsonBody := `{"cluster": {"name": "am-1", "status": "ready"}}`
		w.Write([]byte(jsonBody))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := getStatusHandler(client)
	_, res, err := handler(context.Background(), nil, GetStatusReq{})
	require.NoError(t, err)
	require.Equal(t, "am-1", res.Cluster.Name)
	require.Empty(t, res.Cluster.Peers)
}

func TestGetStatus_SkipsNilPeers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		jsonBody := `{
  "cluster": {
    "name": "am-1",
    "status": "ready",
    "peers": [
      {"name": "am-1", "address": "10.0.0.1:9094"},
      null,
      {"name": "am-3", "address": "10.0.0.3:9094"}
    ]
  }
}`
		w.Write([]byte(jsonBody))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := getStatusHandler(client)
	_, res, err := handler(context.Background(), nil, GetStatusReq{})
	require.NoError(t, err)
	require.Len(t, res.Cluster.Peers, 2)
	require.Equal(t, "am-1", res.Cluster.Peers[0].Name)
	require.Equal(t, "am-3", res.Cluster.Peers[1].Name)
}

func TestGetStatus_NilConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		jsonBody := `{"versionInfo": {"version": "0.27.0"}}`
		w.Write([]byte(jsonBody))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := getStatusHandler(client)
	_, res, err := handler(context.Background(), nil, GetStatusReq{})
	require.NoError(t, err)
	require.Empty(t, res.ConfigYAML)
}

func TestGetStatus_EmptyConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		jsonBody := `{"config": {}}`
		w.Write([]byte(jsonBody))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := getStatusHandler(client)
	_, res, err := handler(context.Background(), nil, GetStatusReq{})
	require.NoError(t, err)
	require.Empty(t, res.ConfigYAML)
}
