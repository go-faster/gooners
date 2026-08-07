package gateway

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// fakeUpstreams builds a TransportBuilder serving an in-memory MCP server per
// upstream name, so a reload can connect new upstreams without exec or sockets.
func fakeUpstreams(tools map[string][]string) TransportBuilder {
	return func(_ context.Context, cfg UpstreamConfig, _ SecretResolver) (mcp.Transport, func() error, error) {
		names, ok := tools[cfg.Name]
		if !ok {
			return nil, nil, errors.Errorf("no fake upstream %q", cfg.Name)
		}
		serverTr, clientTr := mcp.NewInMemoryTransports()
		srv := mcp.NewServer(&mcp.Implementation{Name: cfg.Name, Version: "0"}, nil)
		for _, name := range names {
			srv.AddTool(
				&mcp.Tool{Name: name, Description: name, InputSchema: map[string]any{"type": "object"}},
				func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
				},
			)
		}
		go func() { _ = srv.Run(context.Background(), serverTr) }()
		return clientTr, nil, nil
	}
}

func upstreamCfg(name string) UpstreamConfig {
	return UpstreamConfig{Name: name, Kind: "stdio", Command: []string{"ignored"}}
}

func newTestGateway(t *testing.T, cfg *Config, tools map[string][]string) *Gateway {
	t.Helper()
	g, err := New(cfg, Options{TransportBuilder: fakeUpstreams(tools)})
	require.NoError(t, err)
	require.NoError(t, g.Build(t.Context()))
	t.Cleanup(func() { _ = g.Close(context.Background()) })
	return g
}

func registeredNames(g *Gateway) []string {
	return slices.Sorted(maps.Keys(g.RegisteredTools()))
}

func TestGateway_Reload_AddRemoveUpstream(t *testing.T) {
	cfg := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{upstreamCfg("u1"), upstreamCfg("u2")},
	}
	tools := map[string][]string{"u1": {"a"}, "u2": {"b"}, "u3": {"c"}}
	g := newTestGateway(t, cfg, tools)
	require.Equal(t, []string{"a", "b"}, registeredNames(g))

	next := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{upstreamCfg("u1"), upstreamCfg("u3")},
	}
	res, err := g.Reload(t.Context(), next)
	require.NoError(t, err)
	require.Equal(t, []string{"u3"}, res.Added)
	require.Equal(t, []string{"u2"}, res.Removed)
	require.Equal(t, []string{"u1"}, res.Unchanged)
	require.Empty(t, res.Restarted)
	require.Empty(t, res.RestartRequired)

	require.Equal(t, []string{"a", "c"}, registeredNames(g))
	require.Equal(t, map[string]string{"a": "u1", "c": "u3"}, g.RegisteredTools())
}

// An unchanged upstream must keep its live session: reload is not a restart.
func TestGateway_Reload_KeepsUnchangedUpstreamSession(t *testing.T) {
	cfg := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{upstreamCfg("u1"), upstreamCfg("u2")},
	}
	g := newTestGateway(t, cfg, map[string][]string{"u1": {"a"}, "u2": {"b"}})

	before := g.upstreamByName("u1")
	require.NotNil(t, before)
	beforeSession := before.currentSession()

	next := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{upstreamCfg("u1")},
	}
	_, err := g.Reload(t.Context(), next)
	require.NoError(t, err)

	after := g.upstreamByName("u1")
	require.Same(t, before, after)
	require.Same(t, beforeSession, after.currentSession())
}

func TestGateway_Reload_RestartsChangedUpstream(t *testing.T) {
	cfg := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{upstreamCfg("u1")},
	}
	g := newTestGateway(t, cfg, map[string][]string{"u1": {"a"}})
	before := g.upstreamByName("u1")

	changed := upstreamCfg("u1")
	changed.Tools.Prefix = "p"
	next := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{changed},
	}
	res, err := g.Reload(t.Context(), next)
	require.NoError(t, err)
	require.Equal(t, []string{"u1"}, res.Restarted)

	require.NotSame(t, before, g.upstreamByName("u1"))
	require.Equal(t, map[string]string{"pa": "u1"}, g.RegisteredTools())
}

// A config that does not validate must leave the running one in place.
func TestGateway_Reload_InvalidConfigKeepsOld(t *testing.T) {
	cfg := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{upstreamCfg("u1")},
	}
	g := newTestGateway(t, cfg, map[string][]string{"u1": {"a"}})

	_, err := g.Reload(t.Context(), &Config{Server: ServerConfig{Name: "gw"}})
	require.Error(t, err)
	require.Equal(t, map[string]string{"a": "u1"}, g.RegisteredTools())
	require.Same(t, cfg, g.config())
}

// An upstream that is unreachable at reload time is still adopted, so its
// supervisor can bring it up later, but it must not take the reload down.
func TestGateway_Reload_UnreachableUpstream(t *testing.T) {
	cfg := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{upstreamCfg("u1")},
	}
	g := newTestGateway(t, cfg, map[string][]string{"u1": {"a"}})

	next := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{upstreamCfg("u1"), upstreamCfg("missing")},
	}
	res, err := g.Reload(t.Context(), next)
	require.NoError(t, err)
	require.Equal(t, []string{"missing"}, res.Added)
	require.NotNil(t, g.upstreamByName("missing"))
	require.Equal(t, map[string]string{"a": "u1"}, g.RegisteredTools())
}

func TestGateway_Reload_DropsRoute(t *testing.T) {
	routed := upstreamCfg("u1")
	routed.Route = RouteConfig{Path: "/u1"}
	cfg := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{routed, upstreamCfg("u2")},
	}
	g := newTestGateway(t, cfg, map[string][]string{"u1": {"a"}, "u2": {"b"}})
	require.Len(t, g.routes, 1)

	next := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{upstreamCfg("u1"), upstreamCfg("u2")},
	}
	_, err := g.Reload(t.Context(), next)
	require.NoError(t, err)
	require.Empty(t, g.routes)
}

func TestPlanUpstreams(t *testing.T) {
	withEnv := func(name, value string) UpstreamConfig {
		uc := upstreamCfg(name)
		uc.Env = map[string]string{"TOKEN": value}
		return uc
	}

	tests := []struct {
		name                            string
		old, next                       *Config
		add, restart, remove, unchanged []string
	}{
		{
			name:      "identical",
			old:       &Config{Upstreams: []UpstreamConfig{upstreamCfg("u1")}},
			next:      &Config{Upstreams: []UpstreamConfig{upstreamCfg("u1")}},
			unchanged: []string{"u1"},
		},
		{
			name:   "added and removed",
			old:    &Config{Upstreams: []UpstreamConfig{upstreamCfg("u1")}},
			next:   &Config{Upstreams: []UpstreamConfig{upstreamCfg("u2")}},
			add:    []string{"u2"},
			remove: []string{"u1"},
		},
		{
			name:    "upstream section changed",
			old:     &Config{Upstreams: []UpstreamConfig{upstreamCfg("u1")}},
			next:    &Config{Upstreams: []UpstreamConfig{withEnv("u1", "static")}},
			restart: []string{"u1"},
		},
		{
			name: "referenced secret changed",
			old: &Config{
				Secrets:   []SecretConfig{{Name: "tok", Value: "a"}},
				Upstreams: []UpstreamConfig{withEnv("u1", "{secret:tok}")},
			},
			next: &Config{
				Secrets:   []SecretConfig{{Name: "tok", Value: "b"}},
				Upstreams: []UpstreamConfig{withEnv("u1", "{secret:tok}")},
			},
			restart: []string{"u1"},
		},
		{
			name: "unreferenced secret changed",
			old: &Config{
				Secrets:   []SecretConfig{{Name: "tok", Value: "a"}, {Name: "other", Value: "x"}},
				Upstreams: []UpstreamConfig{withEnv("u1", "{secret:tok}")},
			},
			next: &Config{
				Secrets:   []SecretConfig{{Name: "tok", Value: "a"}, {Name: "other", Value: "y"}},
				Upstreams: []UpstreamConfig{withEnv("u1", "{secret:tok}")},
			},
			unchanged: []string{"u1"},
		},
		{
			name:    "inherited redact changed",
			old:     &Config{Upstreams: []UpstreamConfig{upstreamCfg("u1")}},
			next:    &Config{Redact: RedactConfig{Enabled: true}, Upstreams: []UpstreamConfig{upstreamCfg("u1")}},
			restart: []string{"u1"},
		},
		{
			name: "overridden redact ignores global change",
			old: &Config{Upstreams: []UpstreamConfig{func() UpstreamConfig {
				uc := upstreamCfg("u1")
				uc.Redact = &RedactConfig{}
				return uc
			}()}},
			next: &Config{Redact: RedactConfig{Enabled: true}, Upstreams: []UpstreamConfig{func() UpstreamConfig {
				uc := upstreamCfg("u1")
				uc.Redact = &RedactConfig{}
				return uc
			}()}},
			unchanged: []string{"u1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := planUpstreams(tt.old, tt.next)
			require.Equal(t, tt.add, names(plan.add))
			require.Equal(t, tt.restart, names(plan.restart))
			require.Equal(t, tt.remove, names(plan.remove))
			require.Equal(t, tt.unchanged, plan.unchanged)
		})
	}
}

func names(ucs []UpstreamConfig) []string {
	var out []string
	for _, uc := range ucs {
		out = append(out, uc.Name)
	}
	return out
}

func TestRestartRequired(t *testing.T) {
	// Each case builds its config fresh: setDefaults writes through the
	// Upstreams slice, so a shared one would leak resolved lazy flags between
	// the two configs under comparison.
	base := func() *Config {
		return &Config{
			Server:    ServerConfig{Name: "gw"},
			Upstreams: []UpstreamConfig{upstreamCfg("u1")},
		}
	}
	tests := []struct {
		name string
		next func() *Config
		want []string
	}{
		{name: "same", next: base},
		{
			name: "server name",
			next: func() *Config { c := base(); c.Server.Name = "other"; return c },
			want: []string{"server"},
		},
		{
			name: "auth",
			next: func() *Config { c := base(); c.Auth.Enabled = true; return c },
			want: []string{"auth"},
		},
		{
			name: "lazy toggled on",
			next: func() *Config { c := base(); c.Server.LazyTools = true; return c },
			want: []string{"server", "tools.lazy"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old, next := base(), tt.next()
			old.setDefaults()
			next.setDefaults()
			require.Equal(t, tt.want, restartRequired(old, next))
		})
	}
}

// Reloading concurrently with traffic must not race on the shared state.
func TestGateway_Reload_Concurrent(t *testing.T) {
	cfg := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{upstreamCfg("u1")},
	}
	g := newTestGateway(t, cfg, map[string][]string{"u1": {"a"}, "u2": {"b"}})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 50 {
			g.RegisteredTools()
			g.ServerForRequest(nil)
		}
	}()
	for i := range 4 {
		next := &Config{Server: ServerConfig{Name: "gw"}, Upstreams: []UpstreamConfig{upstreamCfg("u1")}}
		if i%2 == 0 {
			next.Upstreams = append(next.Upstreams, upstreamCfg("u2"))
		}
		_, err := g.Reload(t.Context(), next)
		require.NoError(t, err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("readers did not finish")
	}
}
