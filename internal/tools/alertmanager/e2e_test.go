package alertmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

// startAlertmanagerContainer starts a real Alertmanager server for e2e testing.
// The image ships a default config with a single "web.hook" receiver and no
// alerts, which is enough to exercise status/receivers/silence CRUD.
func startAlertmanagerContainer(ctx context.Context, t *testing.T) string {
	t.Helper()

	req := testcontainers.ContainerRequest{
		Image:        "prom/alertmanager:v0.28.1",
		ExposedPorts: []string{"9093/tcp"},
		WaitingFor:   wait.ForHTTP("/-/ready").WithPort("9093/tcp").WithStartupTimeout(2 * time.Minute),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Skipf("skipping: could not start alertmanager container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	host, err := ctr.Host(ctx)
	require.NoError(t, err)
	port, err := ctr.MappedPort(ctx, "9093/tcp")
	require.NoError(t, err)

	return fmt.Sprintf("http://%s:%s", host, port.Port())
}

// startPrometheusContainer starts a real Prometheus server for e2e testing.
// Prometheus self-scrapes by default, so the "up" metric is always available
// for evaluate_promql_query without any extra config.
func startPrometheusContainer(ctx context.Context, t *testing.T) string {
	t.Helper()

	req := testcontainers.ContainerRequest{
		Image:        "prom/prometheus:v2.55.1",
		ExposedPorts: []string{"9090/tcp"},
		WaitingFor:   wait.ForHTTP("/-/ready").WithPort("9090/tcp").WithStartupTimeout(2 * time.Minute),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Skipf("skipping: could not start prometheus container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	host, err := ctr.Host(ctx)
	require.NoError(t, err)
	port, err := ctr.MappedPort(ctx, "9090/tcp")
	require.NoError(t, err)

	return fmt.Sprintf("http://%s:%s", host, port.Port())
}

// callTool invokes an MCP tool and decodes its JSON text content into T.
func callTool[T any](t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) T {
	t.Helper()

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.NotEmpty(t, res.Content)
	tc, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok, "expected TextContent")
	if res.IsError {
		t.Fatalf("tool %s returned error result: %s", name, tc.Text)
	}

	var out T
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &out), "text=%s", tc.Text)
	return out
}

// TestE2E_AlertmanagerAndPrometheus exercises the alertmanager-mcp tools
// against real Alertmanager and Prometheus containers: status/receivers/alerts
// reads, a full create/list/get/expire silence round trip, and a live PromQL
// evaluation. Skipped under -short, and skipped (not failed) if Docker is
// unavailable, matching the rest of this repo's e2e tests.
func TestE2E_AlertmanagerAndPrometheus(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	ctx := context.Background()

	amURL := startAlertmanagerContainer(ctx, t)
	promURL := startPrometheusContainer(ctx, t)

	c, err := NewClient(Config{
		AlertmanagerURL:    amURL,
		PrometheusURL:      promURL,
		MaxSilenceDuration: time.Hour,
	})
	require.NoError(t, err)

	s := mcp.NewServer(&mcp.Implementation{Name: "alertmanager-mcp-e2e", Version: "test"}, nil)
	Register(s, c)

	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })

	t.Run("get_status", func(t *testing.T) {
		// Alertmanager's cluster gossip settles for a short grace period even
		// for a single node with no peers, so /-/ready can pass before the
		// cluster status flips from "settling" to "ready".
		var status StatusResult
		require.Eventually(t, func() bool {
			status = callTool[StatusResult](t, cs, "get_status", nil)
			return status.Cluster.Status == "ready"
		}, 30*time.Second, 500*time.Millisecond, "expected cluster status to settle to ready")

		require.NotEmpty(t, status.Version)
	})

	t.Run("list_receivers", func(t *testing.T) {
		receivers := callTool[ListReceiversRes](t, cs, "list_receivers", nil)
		require.NotEmpty(t, receivers.Receivers)
	})

	t.Run("list_alerts_empty", func(t *testing.T) {
		alerts := callTool[ListAlertsRes](t, cs, "list_alerts", nil)
		require.Equal(t, 0, alerts.Count)
	})

	t.Run("silence_round_trip", func(t *testing.T) {
		matchers := `alertname="E2ETestAlert"`

		preview := callTool[PreviewSilenceRes](t, cs, "preview_silence", map[string]any{"matchers": matchers})
		require.Equal(t, 0, preview.Count)

		created := callTool[CreateSilenceRes](t, cs, "create_silence", map[string]any{
			"matchers":   matchers,
			"duration":   "5m",
			"created_by": "e2e-test",
			"comment":    "e2e round trip, matches no real alert",
		})
		require.NotEmpty(t, created.ID)
		require.Equal(t, 0, created.MatchingAlerts)

		listed := callTool[ListSilencesRes](t, cs, "list_silences", map[string]any{"filter": matchers})
		require.Equal(t, 1, listed.Count)
		require.Equal(t, created.ID, listed.Silences[0].ID)
		require.Equal(t, matchers, listed.Silences[0].Matchers[0].Raw)

		got := callTool[SilenceSummary](t, cs, "get_silence", map[string]any{"id": created.ID})
		require.Equal(t, created.ID, got.ID)
		require.Contains(t, []string{"active", "pending"}, got.State)

		expired := callTool[mcputil.SuccessResult](t, cs, "expire_silence", map[string]any{"id": created.ID})
		require.True(t, expired.OK)

		gotAfter := callTool[SilenceSummary](t, cs, "get_silence", map[string]any{"id": created.ID})
		require.Equal(t, "expired", gotAfter.State)
	})

	t.Run("evaluate_promql_query", func(t *testing.T) {
		// Prometheus's first self-scrape may not have happened yet right after
		// /-/ready reports healthy, so poll until the "up" series appears.
		var result EvaluatePromQLQueryRes
		require.Eventually(t, func() bool {
			result = callTool[EvaluatePromQLQueryRes](t, cs, "evaluate_promql_query", map[string]any{"expr": "up"})
			return result.SeriesCount >= 1
		}, 30*time.Second, 500*time.Millisecond, "expected at least one 'up' series after Prometheus self-scrapes")

		require.Equal(t, "vector", result.ResultType)
		require.NotNil(t, result.Values)
	})
}
