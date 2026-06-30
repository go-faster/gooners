// Package middleware provides tool call middleware for the gateway.
package middleware

import (
	"context"
	"time"

	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/autometric"
	"github.com/go-faster/sdk/zctx"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// InjectLogger injects logger into request context.
func InjectLogger(lg *zap.Logger) ToolMiddleware {
	return func(next mcp.ToolHandler) mcp.ToolHandler {
		return func(ctx context.Context, req *mcp.CallToolRequest) (_ *mcp.CallToolResult, rerr error) {
			ctx = zctx.WithOpenTelemetryZap(ctx)
			ctx = zctx.Base(ctx, lg)
			return next(ctx, req)
		}
	}
}

// TelemetryOptions configures the telemetry middleware.
type TelemetryOptions struct {
	Upstream       string
	MeterProvider  metric.MeterProvider
	TracerProvider trace.TracerProvider
	Logger         *zap.Logger
}

func (opts *TelemetryOptions) setDefaults() {
	if opts.MeterProvider == nil {
		opts.MeterProvider = otel.GetMeterProvider()
	}
	if opts.TracerProvider == nil {
		opts.TracerProvider = otel.GetTracerProvider()
	}
	if opts.Logger == nil {
		opts.Logger = zap.L()
	}
}

// toolMetrics defines the metrics produced by tool telemetry.
type toolMetrics struct {
	Calls        metric.Int64Counter     `name:"mcpgateway.tool_calls.total" description:"Total number of MCP tool calls forwarded by the gateway"`
	CallDuration metric.Float64Histogram `name:"mcpgateway.tool_call.duration" description:"Duration of MCP tool calls forwarded by the gateway" unit:"s" boundaries:"0.005,0.01,0.025,0.05,0.1,0.25,0.5,1,2.5,5,10"`
}

var newToolMetrics = autometric.Define[toolMetrics](autometric.InitOptions{})

// NewTelemetry returns a constructor that, given TelemetryOptions, produces a ToolMiddleware.
// The constructor must be obtained once; calling NewTelemetry multiple times will panic on duplicate schema registration.
func NewTelemetry(next mcp.ToolHandler, opts TelemetryOptions) (mcp.ToolHandler, error) {
	opts.setDefaults()

	tr := opts.TracerProvider.Tracer("mcpgateway")
	m, err := newToolMetrics(opts.MeterProvider.Meter("mcpgateway"))
	if err != nil {
		return nil, errors.Wrap(err, "init tool metrics")
	}

	h := func(ctx context.Context, req *mcp.CallToolRequest) (_ *mcp.CallToolResult, rerr error) {
		lg := zctx.From(ctx)

		ctx, span := tr.Start(ctx, "tool.call", trace.WithAttributes(
			attribute.String("mcp.tool.name", req.Params.Name),
		))
		defer func() {
			if rerr != nil {
				span.RecordError(rerr)
				span.SetStatus(codes.Error, rerr.Error())
			} else {
				span.SetStatus(codes.Ok, "")
			}
			span.End()
		}()
		if opts.Upstream != "" {
			span.SetAttributes(attribute.String("mcp.upstream", opts.Upstream))
		}

		start := time.Now()
		res, err := next(ctx, req)
		lg.Info("tool call",
			zap.String("name", req.Params.Name),
			zap.Duration("took", time.Since(start)),
			zap.Error(err),
		)
		status := statusAttr(err)
		m.Calls.Add(ctx, 1, metric.WithAttributes(status))
		m.CallDuration.Record(ctx, time.Since(start).Seconds())

		return res, err
	}
	return h, nil
}

func statusAttr(err error) attribute.KeyValue {
	if err != nil {
		return attribute.String("status", "error")
	}
	return attribute.String("status", "ok")
}
