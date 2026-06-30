// Package middleware provides tool call middleware for the gateway.
package middleware

import (
	"context"
	"log/slog"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/gateway"
)

func TestChain_Order(t *testing.T) {
	var order []string
	m1 := func(next mcp.ToolHandler) mcp.ToolHandler {
		return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			order = append(order, "m1")
			return next(ctx, req)
		}
	}
	m2 := func(next mcp.ToolHandler) mcp.ToolHandler {
		return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			order = append(order, "m2")
			return next(ctx, req)
		}
	}
	h := Chain(m1, m2)(func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		order = append(order, "core")
		return &mcp.CallToolResult{}, nil
	})
	_, _ = h(context.Background(), &mcp.CallToolRequest{})
	require.Equal(t, []string{"m1", "m2", "core"}, order)
}

func TestLogging(t *testing.T) {
	lg := slog.Default()
	h := Logging(lg)(func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	_, _ = h(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "t1"}})
}

func TestRedact_Middleware(t *testing.T) {
	r, _ := gateway.NewRedactor(nil, 0)
	red := func(s string) string { return r.Redact(s) }
	h := Redact(red)(func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "password=secret"}}}, nil
	})
	res, _ := h(context.Background(), &mcp.CallToolRequest{})
	tc := res.Content[0].(*mcp.TextContent)
	require.Contains(t, tc.Text, "[REDACTED]")
}
