package session

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type recorded struct {
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
}

func newRecordedPool(t *testing.T) (*Pool, *recorded) {
	t.Helper()

	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		require.NoError(t, tp.Shutdown(context.Background()))
		require.NoError(t, mp.Shutdown(context.Background()))
	})

	p := NewPool(PoolOptions{
		Logger:         slog.New(slog.DiscardHandler),
		TracerProvider: tp,
		MeterProvider:  mp,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.RunLoop(ctx)

	return p, &recorded{spans: spans, reader: reader}
}

func (r *recorded) spanNamed(t *testing.T, name string) sdktrace.ReadOnlySpan {
	t.Helper()

	for _, s := range r.spans.Ended() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("no span named %q, got %v", name, spanNames(r.spans.Ended()))
	return nil
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name()
	}
	return names
}

func (r *recorded) collect(t *testing.T) map[string]metricdata.Aggregation {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, r.reader.Collect(context.Background(), &rm))

	out := make(map[string]metricdata.Aggregation)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m.Data
		}
	}
	return out
}

func attrOf(t *testing.T, span sdktrace.ReadOnlySpan, key attribute.Key) attribute.Value {
	t.Helper()

	for _, kv := range span.Attributes() {
		if kv.Key == key {
			return kv.Value
		}
	}
	t.Fatalf("span %q has no attribute %q, got %v", span.Name(), key, span.Attributes())
	return attribute.Value{}
}

func gaugeValue(t *testing.T, data metricdata.Aggregation) int64 {
	t.Helper()

	g, ok := data.(metricdata.Gauge[int64])
	require.True(t, ok, "got %T", data)
	require.Len(t, g.DataPoints, 1)
	return g.DataPoints[0].Value
}

func sumValue(t *testing.T, data metricdata.Aggregation, want ...attribute.KeyValue) int64 {
	t.Helper()

	s, ok := data.(metricdata.Sum[int64])
	require.True(t, ok, "got %T", data)
	set := attribute.NewSet(want...)
	for _, dp := range s.DataPoints {
		if dp.Attributes.Equals(&set) {
			return dp.Value
		}
	}
	t.Fatalf("no data point with %v in %v", want, s.DataPoints)
	return 0
}

func histogramCount(t *testing.T, data metricdata.Aggregation) uint64 {
	t.Helper()

	h, ok := data.(metricdata.Histogram[float64])
	require.True(t, ok, "got %T", data)
	var total uint64
	for _, dp := range h.DataPoints {
		total += dp.Count
	}
	return total
}

// A session and the commands it runs are one trace, and the command itself
// appears on none of it.
func TestPoolTracesSessionLifecycle(t *testing.T) {
	p, rec := newRecordedPool(t)
	srv := newTestServer(t)
	ctx := context.Background()

	res, err := p.OpenCfg(ctx, dialInsecure(t, srv.addr))
	require.NoError(t, err)

	const secret = "printf 'hunter2' | sudo -S systemctl restart nginx"
	exec := p.Exec(ctx, ExecRequest{SessionID: res.ID, Command: secret})
	require.NoError(t, exec.Err)

	require.NoError(t, p.Close(ctx, res.ID))

	open := rec.spanNamed(t, "ssh.open")
	require.Equal(t, res.ID, attrOf(t, open, "ssh.session.id").AsString())
	require.Equal(t, codes.Unset, open.Status().Code)

	run := rec.spanNamed(t, "ssh.exec")
	require.Equal(t, res.ID, attrOf(t, run, "ssh.session.id").AsString())
	require.Equal(t, int64(0), attrOf(t, run, "ssh.exit_code").AsInt64())
	require.False(t, attrOf(t, run, "ssh.spooled").AsBool())

	// The command is a credential carrier: it must appear on no span this
	// package produces.
	for _, span := range rec.spans.Ended() {
		for _, kv := range span.Attributes() {
			require.NotContains(t, kv.Value.String(), "hunter2")
			require.NotContains(t, kv.Value.String(), "systemctl")
		}
	}

	rec.spanNamed(t, "ssh.close")
}

func TestPoolRecordsSessionMetrics(t *testing.T) {
	p, rec := newRecordedPool(t)
	srv := newTestServer(t)
	ctx := context.Background()

	res, err := p.OpenCfg(ctx, dialInsecure(t, srv.addr))
	require.NoError(t, err)

	require.Equal(t, int64(1), gaugeValue(t, rec.collect(t)["ssh_mcp.sessions"]))

	exec := p.Exec(ctx, ExecRequest{SessionID: res.ID, Command: "echo hi"})
	require.NoError(t, exec.Err)
	require.NoError(t, p.Close(ctx, res.ID))

	m := rec.collect(t)
	require.Equal(t, int64(0), gaugeValue(t, m["ssh_mcp.sessions"]), "the closed session is gone")
	require.Equal(t, int64(1), sumValue(t, m["ssh_mcp.session.opens"], attribute.String("status", "ok")))
	require.Equal(t, uint64(1), histogramCount(t, m["ssh_mcp.session.duration"]))
	require.Equal(t, uint64(1), histogramCount(t, m["ssh_mcp.exec.duration"]))
	require.Equal(t, exec.StdoutSize,
		sumValue(t, m["ssh_mcp.exec.output_bytes"], attribute.String("stream", "stdout")))
}

// A session that never connects is still counted, or a fleet of unreachable
// machines looks like no traffic at all.
func TestPoolRecordsFailedOpen(t *testing.T) {
	p, rec := newRecordedPool(t)

	cfg := dialInsecure(t, "127.0.0.1:1")
	_, err := p.OpenCfg(context.Background(), cfg)
	require.Error(t, err)

	require.Equal(t, int64(1),
		sumValue(t, rec.collect(t)["ssh_mcp.session.opens"], attribute.String("status", "error")))

	open := rec.spanNamed(t, "ssh.open")
	require.Equal(t, codes.Error, open.Status().Code)
	require.Equal(t, int64(0), gaugeValue(t, rec.collect(t)["ssh_mcp.sessions"]))
}
