// Package middleware provides tool call middleware for the gateway.
package middleware

import (
	"context"
	"encoding/json"

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
				redactContent(c, redact)
			}
			if res.StructuredContent != nil {
				res.StructuredContent = redactStructuredContent(res.StructuredContent, redact)
			}
			return res, err
		}
	}
}

func redactContent(c mcp.Content, redact func(string) string) {
	switch v := c.(type) {
	case *mcp.TextContent:
		v.Text = redact(v.Text)
	case *mcp.EmbeddedResource:
		if v.Resource != nil {
			v.Resource.Text = redact(v.Resource.Text)
		}
	}
}

func redactStructuredContent(v any, redact func(string) string) any {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return v
	}
	return redactJSONValue(decoded, redact)
}

func redactJSONValue(v any, redact func(string) string) any {
	switch x := v.(type) {
	case string:
		return redact(x)
	case []any:
		for i := range x {
			x[i] = redactJSONValue(x[i], redact)
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = redactJSONValue(x[k], redact)
		}
		return x
	default:
		return v
	}
}
