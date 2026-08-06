package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// newStallingServer returns a transport to an upstream that completes the MCP
// handshake and then never answers the given method. It is the failure mode
// that the connect timeout does not cover: the session is established, so
// nothing about connecting is wrong.
func newStallingServer(t *testing.T, method string) mcp.Transport {
	t.Helper()

	serverTr, clientTr := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "stalling", Version: "0"}, nil)
	// Every feature is declared, or the gateway short-circuits the listing on a
	// missing capability and never reaches the stall.
	srv.AddTool(&mcp.Tool{Name: "hello", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{}, nil
		})
	srv.AddPrompt(&mcp.Prompt{Name: "p"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{}, nil
	})
	srv.AddResource(&mcp.Resource{URI: "file:///x", Name: "x"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{}, nil
	})
	srv.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "file:///{n}", Name: "t"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{}, nil
	})
	srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, m string, req mcp.Request) (mcp.Result, error) {
			if m == method {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return next(ctx, m, req)
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	go func() { _ = srv.Run(ctx, serverTr) }()
	t.Cleanup(cancel)
	return clientTr
}

// newTestUpstream wires an upstream to an in-memory transport. Connect builds
// its transport rather than using an injected one, so the builder is what a
// test has to replace.
func newTestUpstream(t *testing.T, cfg UpstreamConfig, tr mcp.Transport) *Upstream {
	t.Helper()

	u := newUpstreamWithInMemoryClient(cfg, tr, nil)
	u.buildTransport = func(context.Context, UpstreamConfig, SecretResolver) (mcp.Transport, func() error, error) {
		return tr, func() error { return nil }, nil
	}
	t.Cleanup(func() { _ = u.Close(context.Background()) })
	return u
}

// newStalledUpstream connects an upstream to such a server, with short
// timeouts so the test does not wait out the production defaults.
func newStalledUpstream(t *testing.T, method string, callTimeout time.Duration) *Upstream {
	t.Helper()

	u := newTestUpstream(t,
		UpstreamConfig{Name: "stalled", Kind: "stdio", Command: []string{"ignored"}},
		newStallingServer(t, method))
	u.connectTimeout = 200 * time.Millisecond
	u.callTimeout = callTimeout

	require.NoError(t, u.Connect(t.Context()))
	return u
}

// TestListTimeoutBoundsListing is the regression test for a gateway that could
// not start: call_timeout defaults to no limit, and listing used to inherit
// that, so an upstream stalling on tools/list parked Build forever.
func TestListTimeoutBoundsListing(t *testing.T) {
	for _, tt := range []struct {
		name string
		list func(*Upstream, context.Context) error
	}{
		{name: "Tools", list: func(u *Upstream, ctx context.Context) error {
			_, err := u.ListTools(ctx)
			return err
		}},
		{name: "Prompts", list: func(u *Upstream, ctx context.Context) error {
			_, err := u.ListPrompts(ctx)
			return err
		}},
		{name: "Resources", list: func(u *Upstream, ctx context.Context) error {
			_, err := u.ListResources(ctx)
			return err
		}},
		{name: "ResourceTemplates", list: func(u *Upstream, ctx context.Context) error {
			_, err := u.ListResourceTemplates(ctx)
			return err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			method := map[string]string{
				"Tools":             "tools/list",
				"Prompts":           "prompts/list",
				"Resources":         "resources/list",
				"ResourceTemplates": "resources/templates/list",
			}[tt.name]

			u := newStalledUpstream(t, method, 0) // no call_timeout: the default
			require.Error(t, tt.list(u, t.Context()), "listing must not hang without a call_timeout")
		})
	}
}

// TestListTimeoutPrefersCallTimeout: an operator who set call_timeout gets it
// for listing too, rather than a larger bound chosen behind their back.
func TestListTimeoutPrefersCallTimeout(t *testing.T) {
	for _, tt := range []struct {
		name        string
		callTimeout time.Duration
		connect     time.Duration
		want        time.Duration
	}{
		{name: "CallTimeoutWins", callTimeout: time.Second, connect: time.Minute, want: time.Second},
		{name: "FallsBackToConnect", connect: 5 * time.Second, want: 5 * time.Second},
		{name: "FallsBackToDefault", want: defaultConnectTimeout},
	} {
		t.Run(tt.name, func(t *testing.T) {
			u := &Upstream{callTimeout: tt.callTimeout, connectTimeout: tt.connect}

			ctx, cancel := u.withListTimeout(t.Context())
			defer cancel()

			deadline, ok := ctx.Deadline()
			require.True(t, ok, "listing is always bounded")
			require.WithinDuration(t, time.Now().Add(tt.want), deadline, time.Second)
		})
	}
}

// TestBuildSkipsStalledUpstream: one upstream that will not answer must not
// keep the gateway from serving the others.
func TestBuildSkipsStalledUpstream(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{
			{Name: "stalled", Kind: "stdio", Command: []string{"ignored"}},
			{Name: "healthy", Kind: "stdio", Command: []string{"ignored"}},
		},
	}
	g, err := New(cfg, Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close(context.Background()) })

	stalled := newStalledUpstream(t, "tools/list", 0)
	healthyTr, cancel := newToolServer(t, "healthy")
	t.Cleanup(cancel)
	healthy := newTestUpstream(t, cfg.Upstreams[1], healthyTr)
	g.upstreams = []*Upstream{stalled, healthy}

	done := make(chan error, 1)
	go func() { done <- g.Build(t.Context()) }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("Build did not return: a stalled upstream is blocking startup")
	}

	// The healthy upstream's tools are registered even though its neighbor
	// never answered.
	require.Contains(t, g.registry.upstreamRegistered, "healthy")
	require.NotEmpty(t, g.registry.upstreamRegistered["healthy"])
}

// TestReadyReportsBuild: readiness is what separates "listening" from
// "usable", now that the transport starts first.
func TestReadyReportsBuild(t *testing.T) {
	cfg := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{{Name: "u1", Kind: "stdio", Command: []string{"ignored"}}},
	}
	g, err := New(cfg, Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close(context.Background()) })

	require.ErrorContains(t, g.Ready(), "initial build has not finished")

	tr, cancel := newToolServer(t, "u1")
	t.Cleanup(cancel)
	g.upstreams = []*Upstream{newTestUpstream(t, cfg.Upstreams[0], tr)}
	require.NoError(t, g.Build(t.Context()))

	require.NoError(t, g.Ready())
}

// TestReadyWithUnreachableUpstream: a gateway with a broken dependency is
// still ready. Its supervisor retries, and failing readiness would take a
// working gateway out of rotation over one upstream.
func TestReadyWithUnreachableUpstream(t *testing.T) {
	cfg := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{{Name: "stalled", Kind: "stdio", Command: []string{"ignored"}}},
	}
	g, err := New(cfg, Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close(context.Background()) })

	g.upstreams = []*Upstream{newStalledUpstream(t, "tools/list", 0)}
	require.NoError(t, g.Build(t.Context()))

	require.NoError(t, g.Ready())
}
