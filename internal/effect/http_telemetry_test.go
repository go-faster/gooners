package effect_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/go-faster/gooners/internal/effect"
)

type recorders struct {
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
	opts   effect.HTTPOptions
}

func newRecorders(t *testing.T, policy effect.HTTPPolicy) *recorders {
	t.Helper()

	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		require.NoError(t, tp.Shutdown(context.Background()))
		require.NoError(t, mp.Shutdown(context.Background()))
	})

	return &recorders{
		spans:  spans,
		reader: reader,
		opts: effect.HTTPOptions{
			Policy:         policy,
			TracerProvider: tp,
			MeterProvider:  mp,
		},
	}
}

func (r *recorders) instruments(t *testing.T) []string {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, r.reader.Collect(context.Background(), &rm))

	var names []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names = append(names, m.Name)
		}
	}
	return names
}

func attrValue(t *testing.T, span sdktrace.ReadOnlySpan, key attribute.Key) attribute.Value {
	t.Helper()

	for _, kv := range span.Attributes() {
		if kv.Key == key {
			return kv.Value
		}
	}
	t.Fatalf("span %q has no attribute %q, got %v", span.Name(), key, span.Attributes())
	return attribute.Value{}
}

func TestNewHTTPClientTracesRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)

	rec := newRecorders(t, effect.HTTPPolicy{AllowHosts: effect.AllowHostOf(srv.URL)})
	client := effect.NewHTTPClient(rec.opts)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/health", http.NoBody)
	require.NoError(t, err)
	res, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())

	ended := rec.spans.Ended()
	require.Len(t, ended, 1)
	span := ended[0]
	require.Equal(t, "HTTP GET", span.Name())
	require.Equal(t, int64(http.StatusTeapot), attrValue(t, span, "http.response.status_code").AsInt64())
	require.Equal(t, "127.0.0.1", attrValue(t, span, "server.address").AsString())

	// The path and query are deliberately absent: they carry project ids,
	// blob keys and presigned signatures.
	require.Equal(t, srv.URL, attrValue(t, span, "url.full").AsString())

	require.Contains(t, rec.instruments(t), "http.client.request.duration")
}

func TestNewHTTPClientTracesDeniedRequests(t *testing.T) {
	rec := newRecorders(t, effect.HTTPPolicy{AllowHosts: []string{"allowed.test"}})
	client := effect.NewHTTPClient(rec.opts)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://denied.test/", http.NoBody)
	require.NoError(t, err)
	_, err = client.Do(req) //nolint:bodyclose // the request never leaves.
	require.ErrorIs(t, err, effect.ErrDenied)

	ended := rec.spans.Ended()
	require.Len(t, ended, 1)
	require.Equal(t, codes.Error, ended[0].Status().Code)
}

func TestNewHTTPClientPropagatesTraceContext(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("traceparent")
	}))
	t.Cleanup(srv.Close)

	rec := newRecorders(t, effect.HTTPPolicy{AllowHosts: effect.AllowHostOf(srv.URL)})
	rec.opts.Propagator = propagation.TraceContext{}
	client := effect.NewHTTPClient(rec.opts)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)
	res, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())

	require.NotEmpty(t, got, "upstream received no trace context")
	require.Len(t, rec.spans.Ended(), 1)
	require.Contains(t, got, rec.spans.Ended()[0].SpanContext().TraceID().String())
}

// A presigned S3 URL or an API token in a query string must not reach the
// trace backend, while the request itself goes out untouched.
func TestNewHTTPClientRedactsURLAttribute(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
	}))
	t.Cleanup(srv.Close)

	rec := newRecorders(t, effect.HTTPPolicy{AllowHosts: effect.AllowHostOf(srv.URL)})
	client := effect.NewHTTPClient(rec.opts)

	const target = "/blob/secret-key?X-Amz-Signature=deadbeef"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+target, http.NoBody)
	require.NoError(t, err)
	res, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())

	require.Equal(t, target, gotURL, "the redaction must not change the request")

	span := rec.spans.Ended()[0]
	require.Equal(t, srv.URL, attrValue(t, span, "url.full").AsString())
	for _, kv := range span.Attributes() {
		require.NotContains(t, kv.Value.String(), "deadbeef")
		require.NotContains(t, kv.Value.String(), "secret-key")
	}
}
