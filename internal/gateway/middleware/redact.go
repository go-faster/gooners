// Package middleware provides tool call middleware for the gateway.
package middleware

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Redact wraps the handler and redacts TextContent after the call using provided fn.
func Redact(redact func(string) string) ToolMiddleware {
	if redact == nil {
		return func(next mcp.ToolHandler) mcp.ToolHandler { return next }
	}
	return func(next mcp.ToolHandler) mcp.ToolHandler {
		return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			res, err := next(ctx, req)
			if res == nil {
				return nil, err
			}
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					tc.Text = redact(tc.Text)
				}
			}
			return res, err
		}
	}
}
