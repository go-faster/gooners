// Package main contains the mcpgateway binary.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func setupGateway(t *testing.T, toml string) *mcp.ClientSession {
	t.Helper()
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "gw.toml")
	require.NoError(t, os.WriteFile(tomlPath, []byte(toml), 0o600))
	cfg, err := gateway.Load(tomlPath)
	require.NoError(t, err)
	gw, err := gateway.New(cfg, gateway.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = gw.Close(t.Context()) })
	require.NoError(t, gw.Build(t.Context()))
	ct, st := mcp.NewInMemoryTransports()
	go func() { _ = gw.Server().Run(t.Context(), st) }()
	cli := mcp.NewClient(&mcp.Implementation{Name: "tester", Version: "0"}, nil)
	sess, err := cli.Connect(t.Context(), ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func TestE2E_MultiUpstream_Namespacing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E in -short")
	}
	upImpl1 := &mcp.Implementation{Name: "up1", Version: "0"}
	upSrv1 := mcp.NewServer(upImpl1, nil)
	upSrv1.AddTool(&mcp.Tool{Name: "query", Description: "q", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "prod-result"}}}, nil
	})
	h1 := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upSrv1 }, nil)
	ts1 := httptest.NewServer(h1)
	t.Cleanup(ts1.Close)
	upImpl2 := &mcp.Implementation{Name: "up2", Version: "0"}
	upSrv2 := mcp.NewServer(upImpl2, nil)
	upSrv2.AddTool(&mcp.Tool{Name: "query", Description: "q", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "staging-result"}}}, nil
	})
	h2 := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upSrv2 }, nil)
	ts2 := httptest.NewServer(h2)
	t.Cleanup(ts2.Close)
	toml := `[[upstream]]
name = "prod"
kind = "http"
url = "` + ts1.URL + `"
[upstream.tools]
prefix = "prod."
[[upstream]]
name = "staging"
kind = "http"
url = "` + ts2.URL + `"
[upstream.tools]
prefix = "staging."
`
	sess := setupGateway(t, toml)
	lt, err := sess.ListTools(t.Context(), nil)
	require.NoError(t, err)
	var names []string
	for _, tl := range lt.Tools {
		names = append(names, tl.Name)
	}
	require.ElementsMatch(t, []string{"prod.query", "staging.query"}, names)
	res1, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "prod.query"})
	require.NoError(t, err)
	require.False(t, res1.IsError)
	require.Len(t, res1.Content, 1)
	tc1 := res1.Content[0].(*mcp.TextContent)
	require.Equal(t, "prod-result", tc1.Text)
	res2, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "staging.query"})
	require.NoError(t, err)
	require.False(t, res2.IsError)
	require.Len(t, res2.Content, 1)
	tc2 := res2.Content[0].(*mcp.TextContent)
	require.Equal(t, "staging-result", tc2.Text)
}

func TestE2E_ToolFiltering(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E in -short")
	}
	upImpl := &mcp.Implementation{Name: "up", Version: "0"}
	upSrv := mcp.NewServer(upImpl, nil)
	upSrv.AddTool(&mcp.Tool{Name: "keep", Description: "k", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	upSrv.AddTool(&mcp.Tool{Name: "deny_me", Description: "d", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no"}}}, nil
	})
	upSrv.AddTool(&mcp.Tool{Name: "other", Description: "o", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no"}}}, nil
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upSrv }, nil)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	toml := `[[upstream]]
name = "u"
kind = "http"
url = "` + ts.URL + `"
[upstream.tools]
allow = ["keep", "other"]
deny = ["other"]
`
	sess := setupGateway(t, toml)
	lt, err := sess.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, lt.Tools, 1)
	require.Equal(t, "keep", lt.Tools[0].Name)
	res, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "keep"})
	require.NoError(t, err)
	require.False(t, res.IsError)
	tc := res.Content[0].(*mcp.TextContent)
	require.Equal(t, "ok", tc.Text)
}

func TestE2E_DescriptionTrim(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E in -short")
	}
	upImpl := &mcp.Implementation{Name: "up", Version: "0"}
	upSrv := mcp.NewServer(upImpl, nil)
	long := strings.Repeat("x", 200)
	upSrv.AddTool(&mcp.Tool{Name: "t", Description: long, InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "r"}}}, nil
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upSrv }, nil)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	toml := `[[upstream]]
name = "u"
kind = "http"
url = "` + ts.URL + `"
[upstream.tools]
desc_max = 50
`
	sess := setupGateway(t, toml)
	lt, err := sess.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, lt.Tools, 1)
	desc := lt.Tools[0].Description
	require.Len(t, []rune(desc), 51)
	runes := []rune(desc)
	require.Equal(t, "…", string(runes[len(runes)-1]))
}

func TestE2E_Redaction(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E in -short")
	}
	upImpl1 := &mcp.Implementation{Name: "up1", Version: "0"}
	upSrv1 := mcp.NewServer(upImpl1, nil)
	upSrv1.AddTool(&mcp.Tool{Name: "t1", Description: "1", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "password=hunter2 hello"}}}, nil
	})
	h1 := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upSrv1 }, nil)
	ts1 := httptest.NewServer(h1)
	t.Cleanup(ts1.Close)
	upImpl2 := &mcp.Implementation{Name: "up2", Version: "0"}
	upSrv2 := mcp.NewServer(upImpl2, nil)
	upSrv2.AddTool(&mcp.Tool{Name: "t2", Description: "2", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "token=abc123 plain"}}}, nil
	})
	h2 := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upSrv2 }, nil)
	ts2 := httptest.NewServer(h2)
	t.Cleanup(ts2.Close)
	toml := `[[upstream]]
name = "u1"
kind = "http"
url = "` + ts1.URL + `"
[[upstream]]
name = "u2"
kind = "http"
url = "` + ts2.URL + `"
[redact]
enabled = true
`
	sess := setupGateway(t, toml)
	res1, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "t1"})
	require.NoError(t, err)
	require.False(t, res1.IsError)
	tc1 := res1.Content[0].(*mcp.TextContent)
	require.Equal(t, "password=[REDACTED] hello", tc1.Text)
	res2, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "t2"})
	require.NoError(t, err)
	require.False(t, res2.IsError)
	tc2 := res2.Content[0].(*mcp.TextContent)
	require.Equal(t, "token=[REDACTED] plain", tc2.Text)
}

func TestE2E_PromptForwarding(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E in -short")
	}
	upImpl := &mcp.Implementation{Name: "up", Version: "0"}
	upSrv := mcp.NewServer(upImpl, nil)
	upSrv.AddPrompt(&mcp.Prompt{Name: "greet", Description: "g"}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Description: "g", Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "hello from upstream"}}}}, nil
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upSrv }, nil)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	toml := `[[upstream]]
name = "u"
kind = "http"
url = "` + ts.URL + `"
`
	sess := setupGateway(t, toml)
	lp, err := sess.ListPrompts(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, lp.Prompts, 1)
	require.Equal(t, "greet", lp.Prompts[0].Name)
	gp, err := sess.GetPrompt(t.Context(), &mcp.GetPromptParams{Name: "greet"})
	require.NoError(t, err)
	require.Len(t, gp.Messages, 1)
	tc := gp.Messages[0].Content.(*mcp.TextContent)
	require.Equal(t, "hello from upstream", tc.Text)
}

func TestE2E_ResourceForwarding(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E in -short")
	}
	upImpl := &mcp.Implementation{Name: "up", Version: "0"}
	upSrv := mcp.NewServer(upImpl, nil)
	upSrv.AddResource(&mcp.Resource{URI: "file:///foo", Name: "foo", Description: "f", MIMEType: "text/plain"}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "file:///foo", Text: "hello world"}}}, nil
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upSrv }, nil)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	toml := `[[upstream]]
name = "u"
kind = "http"
url = "` + ts.URL + `"
`
	sess := setupGateway(t, toml)
	lr, err := sess.ListResources(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, lr.Resources, 1)
	require.Equal(t, "file:///foo", lr.Resources[0].URI)
	rr, err := sess.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "file:///foo"})
	require.NoError(t, err)
	require.Len(t, rr.Contents, 1)
	require.Equal(t, "hello world", rr.Contents[0].Text)
}

func TestE2E_ReSync_LiveToolChange(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E in -short")
	}
	upImpl := &mcp.Implementation{Name: "up", Version: "0"}
	upSrv := mcp.NewServer(upImpl, nil)
	upSrv.AddTool(&mcp.Tool{Name: "t1", Description: "1", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "t1"}}}, nil
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upSrv }, nil)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	toml := `[[upstream]]
name = "u"
kind = "http"
url = "` + ts.URL + `"
`
	sess := setupGateway(t, toml)
	lt, err := sess.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, lt.Tools, 1)
	require.Equal(t, "t1", lt.Tools[0].Name)
	upSrv.AddTool(&mcp.Tool{Name: "t2", Description: "2", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "t2"}}}, nil
	})
	upSrv.RemoveTools("t1")
	require.Eventually(t, func() bool {
		lt2, err := sess.ListTools(t.Context(), nil)
		if err != nil {
			return false
		}
		return len(lt2.Tools) == 1 && lt2.Tools[0].Name == "t2"
	}, 2*time.Second, 10*time.Millisecond)
}
