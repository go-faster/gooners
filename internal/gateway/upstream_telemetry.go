package gateway

import (
	"context"
	"log/slog"
	"time"

	"github.com/go-faster/sdk/autometric"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
)

// gatewayMeterName is the meter the gateway's own instruments live on,
// matching the one the reloader and the tool middleware use.
const gatewayMeterName = "mcpgateway"

// upstreamMetrics describes an upstream's connection, which the
// mcpgateway.upstreams gauge only summarizes: the gauge says how many are
// down, these say how hard the gateway is working to bring one back.
type upstreamMetrics struct {
	Connects        metric.Int64Counter     `name:"mcpgateway.upstream.connects" description:"Connection attempts to an upstream, by result"`
	ConnectDuration metric.Float64Histogram `name:"mcpgateway.upstream.connect.duration" description:"How long a connection attempt to an upstream took" unit:"s" boundaries:"0.01,0.05,0.1,0.5,1,5,10,30"`
	Reconnects      metric.Int64Counter     `name:"mcpgateway.upstream.reconnects" description:"Reconnect attempts after an upstream dropped, by result"`
}

var newUpstreamMetrics = autometric.Define[upstreamMetrics](autometric.InitOptions{})

// initUpstreamMetrics builds the instruments, falling back to no-ops: an
// upstream that cannot register a counter should still serve tools.
func initUpstreamMetrics(mp metric.MeterProvider, lg *slog.Logger) upstreamMetrics {
	m, err := newUpstreamMetrics(mp.Meter(gatewayMeterName))
	if err != nil {
		lg.Error("could not create upstream metrics", "error", err)
		m, err = newUpstreamMetrics(metricnoop.NewMeterProvider().Meter(gatewayMeterName))
		if err != nil {
			panic(err)
		}
	}
	return m
}

// upstreamAttr names the upstream a signal belongs to. It is operator-chosen
// and there are as many values as configured upstreams, so it is safe on a
// metric.
func upstreamAttr(name string) attribute.KeyValue {
	return attribute.String("mcp.upstream", name)
}

func resultAttr(err error) attribute.KeyValue {
	if err != nil {
		return attribute.String("status", "error")
	}
	return attribute.String("status", "ok")
}

// An Upstream is also assembled as a struct literal — by the in-memory
// constructors here and by tests — so every instrument is reached through an
// accessor that tolerates the zero value. A missing counter must not be the
// reason a tool call panics.

func (u *Upstream) spanTracer() trace.Tracer {
	if u.tracer == nil {
		return otel.GetTracerProvider().Tracer(gatewayMeterName)
	}
	return u.tracer
}

// recordConnect reports one connection attempt and how long it took.
func (m upstreamMetrics) recordConnect(ctx context.Context, upstream string, err error, took time.Duration) {
	if m.Connects == nil {
		return
	}
	attrs := metric.WithAttributes(upstreamAttr(upstream), resultAttr(err))
	m.Connects.Add(ctx, 1, attrs)
	m.ConnectDuration.Record(ctx, took.Seconds(), attrs)
}

// recordReconnect reports an attempt made after the upstream dropped, which is
// the one an operator wants alerting on.
func (m upstreamMetrics) recordReconnect(ctx context.Context, upstream string, err error) {
	if m.Reconnects == nil {
		return
	}
	m.Reconnects.Add(ctx, 1, metric.WithAttributes(upstreamAttr(upstream), resultAttr(err)))
}
