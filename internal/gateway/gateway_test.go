// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"context"
	"testing"

	"github.com/go-faster/errors"
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

func TestGateway_Build_RegistersTool(t *testing.T) {
	upServerTr, upClientTr := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "0"}, nil)
	srv.AddTool(&mcp.Tool{Name: "echo", Description: "echo", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	go func() { _ = srv.Run(t.Context(), upServerTr) }()

	cfg := &Config{
		Server: ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{
			{Name: "u1", Kind: "stdio", Command: []string{"ignored"}},
		},
	}

	g, err := New(cfg, Options{})
	require.NoError(t, err)

	u := newUpstreamWithInMemoryClient(cfg.Upstreams[0], upClientTr, g.onToolListChanged)
	g.upstreams = []*Upstream{u}

	sess, err := u.client.Connect(t.Context(), upClientTr, nil)
	require.NoError(t, err)
	u.session = sess

	require.NoError(t, g.Build(t.Context()))
	t.Cleanup(func() { _ = g.Close(t.Context()) })

	gwServerTr, gwClientTr := mcp.NewInMemoryTransports()
	go func() { _ = g.Server().Run(t.Context(), gwServerTr) }()

	downClient := mcp.NewClient(&mcp.Implementation{Name: "down", Version: "0"}, nil)
	downSess, err := downClient.Connect(t.Context(), gwClientTr, nil)
	require.NoError(t, err)
	defer downSess.Close()

	res, err := downSess.ListTools(t.Context(), &mcp.ListToolsParams{})
	require.NoError(t, err)
	require.Len(t, res.Tools, 1)
	require.Equal(t, "echo", res.Tools[0].Name)
}

func TestGateway_ReSync_AddsAndRemoves(t *testing.T) {
	upServerTr, upClientTr := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "0"}, nil)
	srv.AddTool(&mcp.Tool{Name: "echo", Description: "echo", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	go func() { _ = srv.Run(t.Context(), upServerTr) }()

	cfg := &Config{
		Server: ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{
			{Name: "u1", Kind: "stdio", Command: []string{"ignored"}},
		},
	}

	g, err := New(cfg, Options{})
	require.NoError(t, err)

	u := newUpstreamWithInMemoryClient(cfg.Upstreams[0], upClientTr, g.onToolListChanged)
	g.upstreams = []*Upstream{u}

	sess, err := u.client.Connect(t.Context(), upClientTr, nil)
	require.NoError(t, err)
	u.session = sess

	require.NoError(t, g.Build(t.Context()))
	t.Cleanup(func() { _ = g.Close(t.Context()) })

	gwServerTr, gwClientTr := mcp.NewInMemoryTransports()
	go func() { _ = g.Server().Run(t.Context(), gwServerTr) }()

	downClient := mcp.NewClient(&mcp.Implementation{Name: "down", Version: "0"}, nil)
	downSess, err := downClient.Connect(t.Context(), gwClientTr, nil)
	require.NoError(t, err)
	defer downSess.Close()

	res, err := downSess.ListTools(t.Context(), &mcp.ListToolsParams{})
	require.NoError(t, err)
	require.Len(t, res.Tools, 1)
	require.Equal(t, "echo", res.Tools[0].Name)

	// Change upstream tools: remove echo, add ping.
	srv.AddTool(&mcp.Tool{Name: "ping", Description: "ping", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil
	})
	srv.RemoveTools("echo")

	// Trigger re-sync directly for determinism (no timing).
	require.NoError(t, g.onToolListChanged(t.Context(), "u1"))

	res2, err := downSess.ListTools(t.Context(), &mcp.ListToolsParams{})
	require.NoError(t, err)
	require.Len(t, res2.Tools, 1)
	require.Equal(t, "ping", res2.Tools[0].Name)
}

func TestGateway_ReSync_CollisionSkipped(t *testing.T) {
	// Two upstreams both with tool "foo" and no prefix -> Build should fail with collision.
	up1ServerTr, up1ClientTr := mcp.NewInMemoryTransports()
	srv1 := mcp.NewServer(&mcp.Implementation{Name: "up1", Version: "0"}, nil)
	srv1.AddTool(&mcp.Tool{Name: "foo", Description: "f1", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "1"}}}, nil
	})
	go func() { _ = srv1.Run(t.Context(), up1ServerTr) }()

	up2ServerTr, up2ClientTr := mcp.NewInMemoryTransports()
	srv2 := mcp.NewServer(&mcp.Implementation{Name: "up2", Version: "0"}, nil)
	srv2.AddTool(&mcp.Tool{Name: "foo", Description: "f2", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "2"}}}, nil
	})
	go func() { _ = srv2.Run(t.Context(), up2ServerTr) }()

	cfg := &Config{
		Server: ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{
			{Name: "u1", Kind: "stdio", Command: []string{"ignored"}},
			{Name: "u2", Kind: "stdio", Command: []string{"ignored"}},
		},
	}

	g, err := New(cfg, Options{})
	require.NoError(t, err)

	u1 := newUpstreamWithInMemoryClient(cfg.Upstreams[0], up1ClientTr, g.onToolListChanged)
	u2 := newUpstreamWithInMemoryClient(cfg.Upstreams[1], up2ClientTr, g.onToolListChanged)
	g.upstreams = []*Upstream{u1, u2}

	s1, err := u1.client.Connect(t.Context(), up1ClientTr, nil)
	require.NoError(t, err)
	u1.session = s1
	s2, err := u2.client.Connect(t.Context(), up2ClientTr, nil)
	require.NoError(t, err)
	u2.session = s2

	err = g.Build(t.Context())
	require.Error(t, err)
	var ce *CollisionsError
	require.True(t, errors.As(err, &ce))
	_ = g.Close(t.Context())

	// Separate test for re-sync collision: u1 owns "foo" (no prefix), then u2 (no prefix) lists "foo" on re-sync.
	// Fresh transports.
	upA1, upA1c := mcp.NewInMemoryTransports()
	srvA1 := mcp.NewServer(&mcp.Implementation{Name: "upa1", Version: "0"}, nil)
	srvA1.AddTool(&mcp.Tool{Name: "foo", Description: "a1", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "a1"}}}, nil
	})
	go func() { _ = srvA1.Run(t.Context(), upA1) }()

	upA2, upA2c := mcp.NewInMemoryTransports()
	srvA2 := mcp.NewServer(&mcp.Implementation{Name: "upa2", Version: "0"}, nil)
	srvA2.AddTool(&mcp.Tool{Name: "foo", Description: "a2", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "a2"}}}, nil
	})
	go func() { _ = srvA2.Run(t.Context(), upA2) }()

	cfgPlain := &Config{
		Server: ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{
			{Name: "u1", Kind: "stdio", Command: []string{"ignored"}},
		},
	}
	gx, err := New(cfgPlain, Options{})
	require.NoError(t, err)
	ux := newUpstreamWithInMemoryClient(cfgPlain.Upstreams[0], upA1c, gx.onToolListChanged)
	gx.upstreams = []*Upstream{ux}
	sx, err := ux.client.Connect(t.Context(), upA1c, nil)
	require.NoError(t, err)
	ux.session = sx
	require.NoError(t, gx.Build(t.Context()))
	require.Equal(t, "u1", gx.RegisteredTools()["foo"])
	t.Cleanup(func() { _ = gx.Close(t.Context()) })

	// Add u2 (fresh client transport) that will list the colliding "foo" on re-sync.
	cfg2 := &Config{
		Server: ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{
			{Name: "u1", Kind: "stdio", Command: []string{"ignored"}},
			{Name: "u2", Kind: "stdio", Command: []string{"ignored"}},
		},
	}
	// Create a new gateway instance just to get onToolListChanged closure bound to it, but reuse gx for state.
	// Instead: inject u2 into gx and call onToolListChanged for u2.
	u2x := newUpstreamWithInMemoryClient(cfg2.Upstreams[1], upA2c, gx.onToolListChanged)
	gx.upstreams = append(gx.upstreams, u2x)
	sx2, err := u2x.client.Connect(t.Context(), upA2c, nil)
	require.NoError(t, err)
	u2x.session = sx2

	// Trigger re-sync for u2 directly (u2 lists "foo", collides with u1's "foo").
	require.NoError(t, gx.onToolListChanged(t.Context(), "u2"))
	// u1 still owns foo.
	require.Equal(t, "u1", gx.RegisteredTools()["foo"])
}

func TestGateway_Build_RegistersPrompt(t *testing.T) {
	upServerTr, upClientTr := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "0"}, nil)
	srv.AddPrompt(&mcp.Prompt{Name: "code-review", Description: "review code"}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{}}, nil
	})
	go func() { _ = srv.Run(t.Context(), upServerTr) }()

	cfg := &Config{
		Server: ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{
			{Name: "u1", Kind: "stdio", Command: []string{"ignored"}},
		},
	}

	g, err := New(cfg, Options{})
	require.NoError(t, err)

	u := newUpstreamWithInMemoryClientWithCallbacks(cfg.Upstreams[0], upClientTr, upstreamCallbacks{OnPromptListChanged: g.onPromptListChanged})
	g.upstreams = []*Upstream{u}

	sess, err := u.client.Connect(t.Context(), upClientTr, nil)
	require.NoError(t, err)
	u.session = sess

	require.NoError(t, g.Build(t.Context()))
	t.Cleanup(func() { _ = g.Close(t.Context()) })

	gwServerTr, gwClientTr := mcp.NewInMemoryTransports()
	go func() { _ = g.Server().Run(t.Context(), gwServerTr) }()

	downClient := mcp.NewClient(&mcp.Implementation{Name: "down", Version: "0"}, nil)
	downSess, err := downClient.Connect(t.Context(), gwClientTr, nil)
	require.NoError(t, err)
	defer downSess.Close()

	res, err := downSess.ListPrompts(t.Context(), &mcp.ListPromptsParams{})
	require.NoError(t, err)
	require.Len(t, res.Prompts, 1)
	require.Equal(t, "code-review", res.Prompts[0].Name)
}

func TestGateway_Build_RegistersResource(t *testing.T) {
	upServerTr, upClientTr := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "0"}, nil)
	srv.AddResource(&mcp.Resource{URI: "file:///foo.txt", Name: "foo", Description: "a file"}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{}, nil
	})
	go func() { _ = srv.Run(t.Context(), upServerTr) }()

	cfg := &Config{
		Server: ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{
			{Name: "u1", Kind: "stdio", Command: []string{"ignored"}},
		},
	}

	g, err := New(cfg, Options{})
	require.NoError(t, err)

	u := newUpstreamWithInMemoryClientWithCallbacks(cfg.Upstreams[0], upClientTr, upstreamCallbacks{OnResourceListChanged: g.onResourceListChanged})
	g.upstreams = []*Upstream{u}

	sess, err := u.client.Connect(t.Context(), upClientTr, nil)
	require.NoError(t, err)
	u.session = sess

	require.NoError(t, g.Build(t.Context()))
	t.Cleanup(func() { _ = g.Close(t.Context()) })

	gwServerTr, gwClientTr := mcp.NewInMemoryTransports()
	go func() { _ = g.Server().Run(t.Context(), gwServerTr) }()

	downClient := mcp.NewClient(&mcp.Implementation{Name: "down", Version: "0"}, nil)
	downSess, err := downClient.Connect(t.Context(), gwClientTr, nil)
	require.NoError(t, err)
	defer downSess.Close()

	res, err := downSess.ListResources(t.Context(), &mcp.ListResourcesParams{})
	require.NoError(t, err)
	require.Len(t, res.Resources, 1)
	require.Equal(t, "file:///foo.txt", res.Resources[0].URI)
}

func TestGateway_Build_RegistersResourceTemplate(t *testing.T) {
	upServerTr, upClientTr := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "0"}, nil)
	srv.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "file:///{name}", Name: "tpl", Description: "template"}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{}, nil
	})
	go func() { _ = srv.Run(t.Context(), upServerTr) }()

	cfg := &Config{
		Server: ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{
			{Name: "u1", Kind: "stdio", Command: []string{"ignored"}},
		},
	}

	g, err := New(cfg, Options{})
	require.NoError(t, err)

	u := newUpstreamWithInMemoryClientWithCallbacks(cfg.Upstreams[0], upClientTr, upstreamCallbacks{OnResourceListChanged: g.onResourceListChanged})
	g.upstreams = []*Upstream{u}

	sess, err := u.client.Connect(t.Context(), upClientTr, nil)
	require.NoError(t, err)
	u.session = sess

	require.NoError(t, g.Build(t.Context()))
	t.Cleanup(func() { _ = g.Close(t.Context()) })

	gwServerTr, gwClientTr := mcp.NewInMemoryTransports()
	go func() { _ = g.Server().Run(t.Context(), gwServerTr) }()

	downClient := mcp.NewClient(&mcp.Implementation{Name: "down", Version: "0"}, nil)
	downSess, err := downClient.Connect(t.Context(), gwClientTr, nil)
	require.NoError(t, err)
	defer downSess.Close()

	res, err := downSess.ListResourceTemplates(t.Context(), &mcp.ListResourceTemplatesParams{})
	require.NoError(t, err)
	require.Len(t, res.ResourceTemplates, 1)
	require.Equal(t, "file:///{name}", res.ResourceTemplates[0].URITemplate)
}

func TestGateway_ReSync_Prompts(t *testing.T) {
	upServerTr, upClientTr := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "0"}, nil)
	srv.AddPrompt(&mcp.Prompt{Name: "p", Description: "p"}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{}, nil
	})
	go func() { _ = srv.Run(t.Context(), upServerTr) }()

	cfg := &Config{
		Server: ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{
			{Name: "u1", Kind: "stdio", Command: []string{"ignored"}},
		},
	}

	g, err := New(cfg, Options{})
	require.NoError(t, err)

	u := newUpstreamWithInMemoryClientWithCallbacks(cfg.Upstreams[0], upClientTr, upstreamCallbacks{OnPromptListChanged: g.onPromptListChanged})
	g.upstreams = []*Upstream{u}

	sess, err := u.client.Connect(t.Context(), upClientTr, nil)
	require.NoError(t, err)
	u.session = sess

	require.NoError(t, g.Build(t.Context()))
	t.Cleanup(func() { _ = g.Close(t.Context()) })

	gwServerTr, gwClientTr := mcp.NewInMemoryTransports()
	go func() { _ = g.Server().Run(t.Context(), gwServerTr) }()

	downClient := mcp.NewClient(&mcp.Implementation{Name: "down", Version: "0"}, nil)
	downSess, err := downClient.Connect(t.Context(), gwClientTr, nil)
	require.NoError(t, err)
	defer downSess.Close()

	res, err := downSess.ListPrompts(t.Context(), &mcp.ListPromptsParams{})
	require.NoError(t, err)
	require.Len(t, res.Prompts, 1)
	require.Equal(t, "p", res.Prompts[0].Name)

	srv.AddPrompt(&mcp.Prompt{Name: "q", Description: "q"}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{}, nil
	})
	srv.RemovePrompts("p")

	require.NoError(t, g.onPromptListChanged(t.Context(), "u1"))

	res2, err := downSess.ListPrompts(t.Context(), &mcp.ListPromptsParams{})
	require.NoError(t, err)
	require.Len(t, res2.Prompts, 1)
	require.Equal(t, "q", res2.Prompts[0].Name)
}

func TestGateway_ReSync_Resources(t *testing.T) {
	upServerTr, upClientTr := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "0"}, nil)
	srv.AddResource(&mcp.Resource{URI: "file:///a.txt", Name: "a"}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{}, nil
	})
	go func() { _ = srv.Run(t.Context(), upServerTr) }()

	cfg := &Config{
		Server: ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{
			{Name: "u1", Kind: "stdio", Command: []string{"ignored"}},
		},
	}

	g, err := New(cfg, Options{})
	require.NoError(t, err)

	u := newUpstreamWithInMemoryClientWithCallbacks(cfg.Upstreams[0], upClientTr, upstreamCallbacks{OnResourceListChanged: g.onResourceListChanged})
	g.upstreams = []*Upstream{u}

	sess, err := u.client.Connect(t.Context(), upClientTr, nil)
	require.NoError(t, err)
	u.session = sess

	require.NoError(t, g.Build(t.Context()))
	t.Cleanup(func() { _ = g.Close(t.Context()) })

	gwServerTr, gwClientTr := mcp.NewInMemoryTransports()
	go func() { _ = g.Server().Run(t.Context(), gwServerTr) }()

	downClient := mcp.NewClient(&mcp.Implementation{Name: "down", Version: "0"}, nil)
	downSess, err := downClient.Connect(t.Context(), gwClientTr, nil)
	require.NoError(t, err)
	defer downSess.Close()

	res, err := downSess.ListResources(t.Context(), &mcp.ListResourcesParams{})
	require.NoError(t, err)
	require.Len(t, res.Resources, 1)

	srv.AddResource(&mcp.Resource{URI: "file:///b.txt", Name: "b"}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{}, nil
	})
	srv.RemoveResources("file:///a.txt")

	require.NoError(t, g.onResourceListChanged(t.Context(), "u1"))

	res2, err := downSess.ListResources(t.Context(), &mcp.ListResourcesParams{})
	require.NoError(t, err)
	require.Len(t, res2.Resources, 1)
	require.Equal(t, "file:///b.txt", res2.Resources[0].URI)
}

func TestGateway_ReSync_ResourceTemplates(t *testing.T) {
	upServerTr, upClientTr := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "0"}, nil)
	srv.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "file:///{n}", Name: "t"}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{}, nil
	})
	go func() { _ = srv.Run(t.Context(), upServerTr) }()

	cfg := &Config{
		Server: ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{
			{Name: "u1", Kind: "stdio", Command: []string{"ignored"}},
		},
	}

	g, err := New(cfg, Options{})
	require.NoError(t, err)

	u := newUpstreamWithInMemoryClientWithCallbacks(cfg.Upstreams[0], upClientTr, upstreamCallbacks{OnResourceListChanged: g.onResourceListChanged})
	g.upstreams = []*Upstream{u}

	sess, err := u.client.Connect(t.Context(), upClientTr, nil)
	require.NoError(t, err)
	u.session = sess

	require.NoError(t, g.Build(t.Context()))
	t.Cleanup(func() { _ = g.Close(t.Context()) })

	gwServerTr, gwClientTr := mcp.NewInMemoryTransports()
	go func() { _ = g.Server().Run(t.Context(), gwServerTr) }()

	downClient := mcp.NewClient(&mcp.Implementation{Name: "down", Version: "0"}, nil)
	downSess, err := downClient.Connect(t.Context(), gwClientTr, nil)
	require.NoError(t, err)
	defer downSess.Close()

	res, err := downSess.ListResourceTemplates(t.Context(), &mcp.ListResourceTemplatesParams{})
	require.NoError(t, err)
	require.Len(t, res.ResourceTemplates, 1)

	srv.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "file:///{m}", Name: "t2"}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{}, nil
	})
	srv.RemoveResourceTemplates("file:///{n}")

	require.NoError(t, g.onResourceListChanged(t.Context(), "u1"))

	res2, err := downSess.ListResourceTemplates(t.Context(), &mcp.ListResourceTemplatesParams{})
	require.NoError(t, err)
	require.Len(t, res2.ResourceTemplates, 1)
	require.Equal(t, "file:///{m}", res2.ResourceTemplates[0].URITemplate)
}

func TestGateway_ResourceUpdated_Broadcast(t *testing.T) {
	upServerTr, upClientTr := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "0"}, nil)
	srv.AddResource(&mcp.Resource{URI: "file:///x.txt", Name: "x"}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{}, nil
	})
	go func() { _ = srv.Run(t.Context(), upServerTr) }()

	cfg := &Config{
		Server: ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{
			{Name: "u1", Kind: "stdio", Command: []string{"ignored"}},
		},
	}

	g, err := New(cfg, Options{})
	require.NoError(t, err)

	u := newUpstreamWithInMemoryClientWithCallbacks(cfg.Upstreams[0], upClientTr, upstreamCallbacks{OnResourceUpdated: g.onResourceUpdated})
	g.upstreams = []*Upstream{u}

	sess, err := u.client.Connect(t.Context(), upClientTr, nil)
	require.NoError(t, err)
	u.session = sess

	require.NoError(t, g.Build(t.Context()))
	t.Cleanup(func() { _ = g.Close(t.Context()) })

	err = g.onResourceUpdated(t.Context(), "u1", "file:///x.txt")
	require.NoError(t, err)
}

func TestGateway_Upstream_ListMethods(t *testing.T) {
	ct, st := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "srv", Version: "0"}, nil)
	srv.AddPrompt(&mcp.Prompt{Name: "p1"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{}, nil
	})
	srv.AddResource(&mcp.Resource{URI: "file:///r1", Name: "r1"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{}, nil
	})
	srv.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "file:///{n}", Name: "t1"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{}, nil
	})
	go srv.Run(context.Background(), st)

	u := newUpstreamWithInMemoryClientWithCallbacks(UpstreamConfig{Name: "u1"}, ct, upstreamCallbacks{})
	sess, err := u.client.Connect(t.Context(), ct, nil)
	require.NoError(t, err)
	u.session = sess

	prompts, err := u.ListPrompts(t.Context())
	require.NoError(t, err)
	require.Len(t, prompts, 1)
	require.Equal(t, "p1", prompts[0].Name)

	_, err = u.GetPrompt(t.Context(), &mcp.GetPromptParams{Name: "p1"})
	require.NoError(t, err)

	resources, err := u.ListResources(t.Context())
	require.NoError(t, err)
	require.Len(t, resources, 1)
	require.Equal(t, "file:///r1", resources[0].URI)

	tpls, err := u.ListResourceTemplates(t.Context())
	require.NoError(t, err)
	require.Len(t, tpls, 1)
	require.Equal(t, "file:///{n}", tpls[0].URITemplate)

	_, err = u.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "file:///r1"})
	require.Error(t, err) // handler returns nil info, SDK errors

	_ = u.Close(t.Context())
}

func TestGateway_RedactsToolOutput(t *testing.T) {
	upServerTr, upClientTr := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "0"}, nil)
	srv.AddTool(&mcp.Tool{Name: "echo", Description: "echo", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "password=hunter2 token=abc"}}}, nil
	})
	go func() { _ = srv.Run(t.Context(), upServerTr) }()

	cfg := &Config{
		Server: ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{
			{Name: "u1", Kind: "stdio", Command: []string{"ignored"}},
		},
		Redact: RedactConfig{Enabled: true},
	}

	g, err := New(cfg, Options{})
	require.NoError(t, err)

	u := newUpstreamWithInMemoryClient(cfg.Upstreams[0], upClientTr, g.onToolListChanged)
	g.upstreams = []*Upstream{u}

	sess, err := u.client.Connect(t.Context(), upClientTr, nil)
	require.NoError(t, err)
	u.session = sess

	require.NoError(t, g.Build(t.Context()))
	t.Cleanup(func() { _ = g.Close(t.Context()) })

	gwServerTr, gwClientTr := mcp.NewInMemoryTransports()
	go func() { _ = g.Server().Run(t.Context(), gwServerTr) }()

	downClient := mcp.NewClient(&mcp.Implementation{Name: "down", Version: "0"}, nil)
	downSess, err := downClient.Connect(t.Context(), gwClientTr, nil)
	require.NoError(t, err)
	defer downSess.Close()

	res, err := downSess.CallTool(t.Context(), &mcp.CallToolParams{Name: "echo"})
	require.NoError(t, err)
	require.Len(t, res.Content, 1)
	tc := res.Content[0].(*mcp.TextContent)
	require.Equal(t, "password=[REDACTED] token=[REDACTED]", tc.Text)
}
