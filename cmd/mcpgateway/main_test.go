// Package main contains the mcpgateway binary.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/gateway"
)

func TestMain_Smoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E in -short")
	}

	// Start a real upstream MCP server on streamable-http.
	upImpl := &mcp.Implementation{Name: "up", Version: "0"}
	upSrv := mcp.NewServer(upImpl, nil)
	upSrv.AddTool(&mcp.Tool{Name: "ping", Description: "ping", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil
	})

	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upSrv }, nil)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	// Write minimal config pointing at the test server.
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "gw.toml")
	toml := `[[upstream]]
name = "testup"
kind = "http"
url = "` + ts.URL + `"
`
	require.NoError(t, os.WriteFile(tomlPath, []byte(toml), 0o600))

	cfg, err := gateway.Load(tomlPath)
	require.NoError(t, err)

	gw, err := gateway.New(cfg, gateway.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = gw.Close(context.Background()) })

	require.NoError(t, gw.Build(context.Background()))

	// To round-trip we create an in-memory client against the gateway's server.
	// But gateway.Server() is served via transport; for smoke we directly invoke the registered handler via the mcp server internals is hard.
	// Instead, use NewInMemoryTransports to connect a client to the gateway server.
	ct, st := mcp.NewInMemoryTransports()
	go func() { _ = gw.Server().Run(context.Background(), st) }()

	cli := mcp.NewClient(&mcp.Implementation{Name: "tester", Version: "0"}, nil)
	sess, err := cli.Connect(context.Background(), ct, nil)
	require.NoError(t, err)
	defer sess.Close()

	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "ping"})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Len(t, res.Content, 1)
	tc := res.Content[0].(*mcp.TextContent)
	require.Equal(t, "pong", tc.Text)
}
