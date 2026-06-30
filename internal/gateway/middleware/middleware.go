// Package middleware provides tool call middleware for the gateway.
package middleware

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolMiddleware wraps a tool handler.
type ToolMiddleware func(mcp.ToolHandler) mcp.ToolHandler

// Chain applies middlewares right-to-left so the leftmost is outermost.
func Chain(mws ...ToolMiddleware) ToolMiddleware {
	return func(next mcp.ToolHandler) mcp.ToolHandler {
		for _, mw := range slices.Backward(mws) {
			next = mw(next)
		}
		return next
	}
}

// Use is a trivial helper to collect variadic middlewares.
func Use(mws ...ToolMiddleware) []ToolMiddleware { return mws }

// Logging is an audit middleware that logs tool name and duration.
func Logging(lg *slog.Logger) ToolMiddleware {
	return func(next mcp.ToolHandler) mcp.ToolHandler {
		return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			start := time.Now()
			res, err := next(ctx, req)
			lg.Info("tool call", "name", req.Params.Name, "dur", time.Since(start), "err", err)
			return res, err
		}
	}
}
