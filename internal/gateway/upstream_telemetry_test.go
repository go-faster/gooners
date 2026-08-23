package gateway

import (
	"context"
	"log/slog"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func counterValue(t *testing.T, m metricdata.Metrics, want ...attribute.KeyValue) int64 {
	t.Helper()

	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "got %T", m.Data)
	set := attribute.NewSet(want...)
	for _, dp := range sum.DataPoints {
		if dp.Attributes.Equals(&set) {
			return dp.Value
		}
	}
	t.Fatalf("no data point with %v in %v", want, sum.DataPoints)
	return 0
}

// A forwarded call is a span of its own, so the time an upstream took is
// separable from the time the gateway spent reaching it.
func TestUpstreamTracesForwardedCalls(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })

	ct, st := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "srv", Version: "0"}, nil)
	srv.AddTool(&mcp.Tool{Name: "hello", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hi"}}}, nil
		})
	go func() { _ = srv.Run(context.Background(), st) }()

	u := &Upstream{
		cfg:          UpstreamConfig{Name: "u1"},
		logger:       slog.New(slog.DiscardHandler),
		drainTimeout: defaultDrainTimeout,
		tracer:       tp.Tracer("test"),
	}
	u.client = mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	sess, err := u.client.Connect(t.Context(), ct, nil)
	require.NoError(t, err)
	u.session = sess
	t.Cleanup(func() { _ = u.Close(context.Background()) })

	_, err = u.CallTool(t.Context(), &mcp.CallToolParams{Name: "hello"})
	require.NoError(t, err)

	ended := spans.Ended()
	require.Len(t, ended, 1)
	require.Equal(t, "upstream.call_tool", ended[0].Name())
	require.Equal(t, []attribute.KeyValue{
		attribute.String("mcp.upstream", "u1"),
		attribute.String("mcp.tool.name", "hello"),
	}, ended[0].Attributes())
}

// A call to an upstream that is not connected is a failed span, which is what
// distinguishes "the tool failed" from "the gateway could not reach it".
func TestUpstreamTracesUnreachableCall(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })

	u := &Upstream{
		cfg:          UpstreamConfig{Name: "u1"},
		logger:       slog.New(slog.DiscardHandler),
		drainTimeout: defaultDrainTimeout,
		tracer:       tp.Tracer("test"),
	}

	_, err := u.CallTool(t.Context(), &mcp.CallToolParams{Name: "hello"})
	require.Error(t, err)

	ended := spans.Ended()
	require.Len(t, ended, 1)
	require.Equal(t, codes.Error, ended[0].Status().Code)
}

// An upstream that cannot be dialed is counted, so a gateway reconnecting in a
// loop is visible without reading its log.
func TestUpstreamCountsFailedConnects(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, mp.Shutdown(context.Background())) })

	u, err := NewUpstream(UpstreamConfig{
		Name:    "u1",
		Command: []string{"/nonexistent/mcp-server"},
	}, UpstreamOptions{
		Logger:        slog.New(slog.DiscardHandler),
		MeterProvider: mp,
	})
	require.NoError(t, err)

	require.Error(t, u.connectOnce(t.Context()))

	m := collectMetrics(t, reader)
	require.Equal(t, int64(1), counterValue(t, m["mcpgateway.upstream.connects"],
		attribute.String("mcp.upstream", "u1"), attribute.String("status", "error")))

	h, ok := m["mcpgateway.upstream.connect.duration"].Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, h.DataPoints, 1)
	require.Equal(t, uint64(1), h.DataPoints[0].Count)
}

// An upstream that was already connected did not dial, so it is not an attempt.
func TestUpstreamDoesNotCountANoopConnect(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, mp.Shutdown(context.Background())) })

	ct, st := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "srv", Version: "0"}, nil)
	go func() { _ = srv.Run(context.Background(), st) }()

	u, err := NewUpstream(UpstreamConfig{Name: "u1"}, UpstreamOptions{
		Logger:        slog.New(slog.DiscardHandler),
		MeterProvider: mp,
	})
	require.NoError(t, err)
	u.client = mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	sess, err := u.client.Connect(t.Context(), ct, nil)
	require.NoError(t, err)
	u.session = sess
	t.Cleanup(func() { _ = u.Close(context.Background()) })

	require.NoError(t, u.connectOnce(t.Context()))

	require.NotContains(t, collectMetrics(t, reader), "mcpgateway.upstream.connects")
}
