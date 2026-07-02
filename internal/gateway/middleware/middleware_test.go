// Package middleware provides tool call middleware for the gateway.
package middleware

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestChain_Order(t *testing.T) {
	var order []string
	m1 := func(next mcp.ToolHandler) mcp.ToolHandler {
		return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			order = append(order, "m1")
			return next(ctx, req)
		}
	}
	m2 := func(next mcp.ToolHandler) mcp.ToolHandler {
		return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			order = append(order, "m2")
			return next(ctx, req)
		}
	}
	h := Chain(m1, m2)(func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		order = append(order, "core")
		return &mcp.CallToolResult{}, nil
	})
	_, _ = h(context.Background(), &mcp.CallToolRequest{})
	require.Equal(t, []string{"m1", "m2", "core"}, order)
}

func TestRedact_Middleware(t *testing.T) {
	red := func(s string) string { return "[REDACTED]" }
	h := Redact(red)(func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "password=secret"}}}, nil
	})
	res, _ := h(context.Background(), &mcp.CallToolRequest{})
	tc := res.Content[0].(*mcp.TextContent)
	require.Contains(t, tc.Text, "[REDACTED]")
}

func TestTelemetry_RecordsSpanAndMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(t.Context()) })
	tp := tracenoop.NewTracerProvider()

	hOK := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	}
	hErr := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, context.Canceled
	}

	// ok path
	mwOK, err := NewTelemetry(hOK, TelemetryOptions{Upstream: "u1", MeterProvider: mp, TracerProvider: tp})
	require.NoError(t, err)
	_, err = mwOK(t.Context(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "echo"}})
	require.NoError(t, err)

	// error path (same meter, NewTelemetry called only once to avoid global schema panic)
	mwErr, err := NewTelemetry(hErr, TelemetryOptions{Upstream: "u2", MeterProvider: mp, TracerProvider: tp})
	require.NoError(t, err)

	_, err = mwErr(t.Context(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "echo"}})
	require.Error(t, err)

	// two more calls to reach counts >1
	_, _ = mwOK(t.Context(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "ok2"}})
	_, _ = mwErr(t.Context(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "err2"}})

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	assertCounter(t, rm, "ok", 2)
	assertCounter(t, rm, "error", 2)
}

func assertCounter(t *testing.T, rm metricdata.ResourceMetrics, status string, want int64) {
	t.Helper()
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "mcpgateway.tool_calls.total" {
				continue
			}
			data := m.Data.(metricdata.Sum[int64])
			for _, dp := range data.DataPoints {
				for _, a := range dp.Attributes.ToSlice() {
					if a.Key == "status" && a.Value.AsString() == status {
						require.Equal(t, want, dp.Value)
						found = true
					}
				}
			}
		}
	}
	require.True(t, found, "expected status=%s", status)
}
