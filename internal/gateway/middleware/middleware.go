// Package middleware provides tool call middleware for the gateway.
package middleware

import (
	"slices"

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
