// Package middleware provides tool call middleware for the gateway.
package middleware

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TrimDescription is the pure helper; description trimming is static and happens at Build time.
// TruncateDescription middleware is intentionally not provided because descriptions are not call-time.
func TrimDescription(s string, maxLen int) string {
	if maxLen > 0 && len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}

// TruncateDescription is a no-op placeholder. Description trimming is performed at Build time in upstream.BuildTools.
func TruncateDescription(_ int) ToolMiddleware {
	return func(next mcp.ToolHandler) mcp.ToolHandler { return next }
}
