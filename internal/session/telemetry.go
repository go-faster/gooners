package session

import (
	"context"
	"log/slog"

	"github.com/go-faster/sdk/autometric"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
)

// instrumentationName identifies this package as the instrumentation, not the
// binary it runs in.
const instrumentationName = "github.com/go-faster/gooners/internal/session"

// poolMetrics is what an operator watches: how many sessions exist, how long
// they live, and what commands cost. Nothing here is keyed by machine or
// command, so the series count does not grow with the fleet or with what an
// agent decides to run.
type poolMetrics struct {
	SessionOpens    metric.Int64Counter     `name:"ssh_mcp.session.opens" description:"SSH sessions opened, by result"`
	SessionDuration metric.Float64Histogram `name:"ssh_mcp.session.duration" description:"How long an SSH session stayed open" unit:"s" boundaries:"1,10,60,300,900,3600,14400"`
	ExecDuration    metric.Float64Histogram `name:"ssh_mcp.exec.duration" description:"Duration of a command executed over SSH" unit:"s" boundaries:"0.01,0.05,0.1,0.5,1,5,10,30,60"`
	ExecOutput      metric.Int64Counter     `name:"ssh_mcp.exec.output_bytes" description:"Bytes of command output produced, by stream" unit:"By"`
	ExecSpooled     metric.Int64Counter     `name:"ssh_mcp.exec.spooled_bytes" description:"Bytes of command output too large to hold in memory, written to the spool" unit:"By"`
}

var newPoolMetrics = autometric.Define[poolMetrics](autometric.InitOptions{})

// initMetrics builds the instruments, falling back to no-ops. A pool that
// cannot register a counter is still a pool: refusing to run an SSH session
// because a metric could not be created would be the wrong trade.
func initMetrics(mp metric.MeterProvider, lg *slog.Logger) poolMetrics {
	m, err := newPoolMetrics(mp.Meter(instrumentationName))
	if err != nil {
		lg.Error("could not create session metrics", "err", err)
		m, err = newPoolMetrics(metricnoop.NewMeterProvider().Meter(instrumentationName))
		if err != nil {
			// A no-op meter cannot fail; if it somehow does, an unusable pool
			// is better than a nil dereference on the first command.
			panic(err)
		}
	}
	return m
}

// liveSessions registers the gauge of currently open sessions. The count is an
// atomic rather than the event loop's map, because the callback runs on the
// collector's goroutine and the map belongs to the loop.
func (p *Pool) registerLiveSessions(mp metric.MeterProvider, lg *slog.Logger) {
	_, err := mp.Meter(instrumentationName).Int64ObservableGauge(
		"ssh_mcp.sessions",
		metric.WithDescription("SSH sessions currently open"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(p.liveSessions.Load())
			return nil
		}),
	)
	if err != nil {
		lg.Error("could not create the live sessions gauge", "err", err)
	}
}

// statusAttr is the one dimension every operation reports.
func statusAttr(err error) attribute.KeyValue {
	if err != nil {
		return attribute.String("status", "error")
	}
	return attribute.String("status", "ok")
}

// endSpan closes a span, recording err as its status. The command a session ran
// is deliberately absent from every span this package starts: an argument list
// carries passwords, tokens and file contents, and a trace backend is not where
// those belong. The session, the machine and the exit code identify the call.
func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}
