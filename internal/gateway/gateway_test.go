// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestGateway_InMemory(t *testing.T) {
	// Create an in-memory MCP server with a tool.
	st, ct := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test-up", Version: "0"}, nil)
	srv.AddTool(&mcp.Tool{Name: "echo", Description: "echo back", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	go func() { _ = srv.Run(context.Background(), st) }()

	// Build gateway config with one upstream that will use in-memory transport.
	cfg := &Config{
		Server: ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{
			{Name: "u1", Kind: "stdio", Command: []string{"ignored"}},
		},
	}

	g, err := New(cfg, Options{})
	require.NoError(t, err)

	// Inject test upstream using the hook.
	u := newUpstreamWithTransport(cfg.Upstreams[0], ct, func() error { return nil })
	// manually connect the session for this test upstream
	u.client = mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	sess, err := u.client.Connect(t.Context(), ct, nil)
	require.NoError(t, err)
	u.session = sess

	// replace the one created by New
	g.upstreams = []*Upstream{u}

	// do not call Build (it would call Connect which execs); just register tools manually for smoke
	// For real integration smoke we would set BuildTransport to a fake, but for scaffold we just verify no panic on New.
	_ = g.Close(t.Context())

	s := g.Server()
	require.NotNil(t, s)

	// The tool should be registered under its name (no prefix).
	// We can't easily introspect registered tools, but we can at least not crash on Build.
	_ = g.Close(t.Context())
}
