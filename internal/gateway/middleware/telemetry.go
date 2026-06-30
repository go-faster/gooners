// Package middleware provides tool call middleware for the gateway.
package middleware

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
)

// TelemetryOptions configures the telemetry middleware (currently minimal).
type TelemetryOptions struct{}

// Telemetry is a stub middleware that starts/ends a span for the tool call.
// TODO: wire real OTLP exporter + attributes when [telemetry] enabled in config.
func Telemetry(_ TelemetryOptions) ToolMiddleware {
	tr := otel.Tracer("mcpgateway")
	return func(next mcp.ToolHandler) mcp.ToolHandler {
		return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ctx, span := tr.Start(ctx, "tool.call")
			defer span.End()
			span.SetAttributes() // TODO: add tool name etc.
			return next(ctx, req)
		}
	}
}
