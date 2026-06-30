// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestUpstream_BuildTools(t *testing.T) {
	u := &Upstream{cfg: UpstreamConfig{Tools: ToolsConfig{Prefix: "p."}}}
	tools := []*mcp.Tool{{Name: "x", Description: "d"}}
	got := u.BuildTools(tools)
	require.Equal(t, "p.x", got[0].Name)
}

func TestUpstream_Filter(t *testing.T) {
	u := &Upstream{cfg: UpstreamConfig{Tools: ToolsConfig{Allow: []string{"a*"}, Deny: []string{"ab*"}}}}
	require.True(t, u.allowed("ac"))
	require.False(t, u.allowed("ab"))
	require.False(t, u.allowed("x"))
}

func TestUpstream_Trim(t *testing.T) {
	require.Equal(t, "abc…", TrimDescription("abcdef", 3))
}

func TestUpstream_InMemory(t *testing.T) {
	ct, st := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "srv", Version: "0"}, nil)
	srv.AddTool(&mcp.Tool{Name: "hello", Description: "say hi", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hi"}}}, nil
	})
	go srv.Run(context.Background(), st)

	u := &Upstream{cfg: UpstreamConfig{Name: "u1"}}
	// wire in-memory directly
	u.client = mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	sess, err := u.client.Connect(t.Context(), ct, nil)
	require.NoError(t, err)
	u.session = sess

	tools, err := u.ListTools(t.Context())
	require.NoError(t, err)
	require.Len(t, tools, 1)

	res, err := u.CallTool(t.Context(), &mcp.CallToolParams{Name: "hello"})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Len(t, res.Content, 1)

	_ = u.Close(t.Context())
}
