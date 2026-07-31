// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestSearchScore(t *testing.T) {
	tests := []struct {
		name  string
		terms []string
		tool  string
		desc  string
		want  int
		match bool
	}{
		{"empty query matches", nil, "get_dashboard", "get a dashboard", 1, true},
		{"exact name wins", []string{"get_dashboard"}, "get_dashboard", "", 0, true},
		{"name substring", []string{"dash"}, "get_dashboard", "unrelated", 1, true},
		{"description only", []string{"loki"}, "query_range", "run a Loki query", 2, true},
		{"mixed name and description", []string{"query", "loki"}, "query_range", "run a Loki query", 2, true},
		{"all terms must match", []string{"query", "grafana"}, "query_range", "run a Loki query", 0, false},
		{"case insensitive", []string{"loki"}, "LokiQuery", "", 1, true},
		{"no match", []string{"zzz"}, "get_dashboard", "get a dashboard", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := searchScore(tt.terms, tt.tool, tt.desc)
			require.Equal(t, tt.match, ok)
			require.Equal(t, tt.want, got)
		})
	}
}

// newDiscoveryTestGateway builds a gateway with one lazy upstream ("grafana",
// scoped) and one eager upstream ("ssh"), without connecting anything.
func newDiscoveryTestGateway() *Gateway {
	grafana := &Upstream{cfg: UpstreamConfig{
		Name:  "grafana",
		Tools: ToolsConfig{Prefix: "grafana.", Scopes: grafanaScopes, Lazy: true},
	}}
	ssh := &Upstream{cfg: UpstreamConfig{Name: "ssh"}}
	tools := map[string]*mcp.Tool{
		"grafana.get_dashboard": {Name: "grafana.get_dashboard", Description: "get a dashboard", InputSchema: map[string]any{"type": "object"}},
		"grafana.add_dashboard": {Name: "grafana.add_dashboard", Description: "add a dashboard", InputSchema: map[string]any{"type": "object"}},
		"ssh_exec":              {Name: "ssh_exec", Description: "run a command over ssh", InputSchema: map[string]any{"type": "object"}},
	}
	return &Gateway{
		discovery: true,
		upstreams: []*Upstream{grafana, ssh},
		registry: upstreamRegistry{
			finalToUpstream: map[string]string{
				"grafana.get_dashboard": "grafana",
				"grafana.add_dashboard": "grafana",
				"ssh_exec":              "ssh",
			},
			registeredTools: tools,
		},
	}
}

func callToolReq(scopes []string) *mcp.CallToolRequest {
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{}}
	if scopes != nil {
		req.Extra = &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{Scopes: scopes}}
	}
	return req
}

func TestGateway_SearchTools(t *testing.T) {
	g := newDiscoveryTestGateway()

	_, out, err := g.handleSearchTools(t.Context(), callToolReq(nil), searchToolsInput{Query: "dashboard"})
	require.NoError(t, err)
	require.Equal(t, 2, out.Total)
	require.False(t, out.Truncated)
	require.Equal(t, []toolMatch{
		{Name: "grafana.add_dashboard", Upstream: "grafana", Description: "add a dashboard", Hidden: true},
		{Name: "grafana.get_dashboard", Upstream: "grafana", Description: "get a dashboard", Hidden: true},
	}, out.Tools)

	// A non-lazy upstream's tools are visible in tools/list, so they are not hidden.
	_, out, err = g.handleSearchTools(t.Context(), callToolReq(nil), searchToolsInput{Query: "ssh"})
	require.NoError(t, err)
	require.Equal(t, []toolMatch{
		{Name: "ssh_exec", Upstream: "ssh", Description: "run a command over ssh"},
	}, out.Tools)

	// Empty query lists the whole catalog.
	_, out, err = g.handleSearchTools(t.Context(), callToolReq(nil), searchToolsInput{})
	require.NoError(t, err)
	require.Equal(t, 3, out.Total)
	require.Len(t, out.Tools, 3)
}

func TestGateway_SearchTools_Limit(t *testing.T) {
	g := newDiscoveryTestGateway()

	_, out, err := g.handleSearchTools(t.Context(), callToolReq(nil), searchToolsInput{Limit: 1})
	require.NoError(t, err)
	require.Equal(t, 3, out.Total)
	require.True(t, out.Truncated)
	require.Len(t, out.Tools, 1)
}

func TestGateway_SearchTools_RespectsScopes(t *testing.T) {
	g := newDiscoveryTestGateway()

	// The read sub-scope covers get_* but not add_*, and grants nothing on ssh.
	_, out, err := g.handleSearchTools(t.Context(), callToolReq([]string{"mcp:grafana:read"}), searchToolsInput{})
	require.NoError(t, err)
	require.Len(t, out.Tools, 1)
	require.Equal(t, "grafana.get_dashboard", out.Tools[0].Name)

	// An empty scope set hides everything rather than falling back to full access.
	_, out, err = g.handleSearchTools(t.Context(), callToolReq([]string{}), searchToolsInput{})
	require.NoError(t, err)
	require.Empty(t, out.Tools)
	require.Equal(t, 0, out.Total)
}

func TestGateway_DescribeTools(t *testing.T) {
	g := newDiscoveryTestGateway()

	res, _, err := g.handleDescribeTools(t.Context(), callToolReq(nil), describeToolsInput{
		Names: []string{"grafana.get_dashboard", "nope"},
	})
	require.NoError(t, err)
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)

	var got struct {
		Tools    []*mcp.Tool `json:"tools"`
		NotFound []string    `json:"not_found"`
	}
	require.NoError(t, json.Unmarshal([]byte(text.Text), &got))
	require.Equal(t, []string{"nope"}, got.NotFound)
	require.Len(t, got.Tools, 1)

	// The definition must be byte-identical to what tools/list would report.
	want, err := json.Marshal(g.registry.registeredTools["grafana.get_dashboard"])
	require.NoError(t, err)
	gotJSON, err := json.Marshal(got.Tools[0])
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(gotJSON))
}

func TestGateway_DescribeTools_ScopedToolIsNotFound(t *testing.T) {
	g := newDiscoveryTestGateway()

	res, _, err := g.handleDescribeTools(t.Context(), callToolReq([]string{"mcp:grafana:read"}), describeToolsInput{
		Names: []string{"grafana.add_dashboard"},
	})
	require.NoError(t, err)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)

	var got struct {
		Tools    []*mcp.Tool `json:"tools"`
		NotFound []string    `json:"not_found"`
	}
	require.NoError(t, json.Unmarshal([]byte(text.Text), &got))
	require.Empty(t, got.Tools)
	require.Equal(t, []string{"grafana.add_dashboard"}, got.NotFound)
}

func TestGateway_DescribeTools_RequiresNames(t *testing.T) {
	g := newDiscoveryTestGateway()
	_, _, err := g.handleDescribeTools(t.Context(), callToolReq(nil), describeToolsInput{})
	require.Error(t, err)
}

func TestGateway_LazyMiddleware_FiltersToolsList(t *testing.T) {
	g := newDiscoveryTestGateway()

	next := func(_ context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		return &mcp.ListToolsResult{Tools: []*mcp.Tool{
			{Name: "grafana.get_dashboard"},
			{Name: "ssh_exec"},
			{Name: searchToolsName},
			{Name: describeToolsName},
		}}, nil
	}
	res, err := g.lazyMiddleware()(next)(t.Context(), "tools/list", &mcp.ListToolsRequest{Params: &mcp.ListToolsParams{}})
	require.NoError(t, err)
	lt, ok := res.(*mcp.ListToolsResult)
	require.True(t, ok)

	names := make([]string, 0, len(lt.Tools))
	for _, tool := range lt.Tools {
		names = append(names, tool.Name)
	}
	require.Equal(t, []string{"ssh_exec", searchToolsName, describeToolsName}, names)
}

func TestGateway_LazyMiddleware_DoesNotGateCalls(t *testing.T) {
	g := newDiscoveryTestGateway()

	called := false
	next := func(_ context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		called = true
		return &mcp.CallToolResult{}, nil
	}
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "grafana.get_dashboard"}}
	_, err := g.lazyMiddleware()(next)(t.Context(), "tools/call", req)
	require.NoError(t, err)
	require.True(t, called)
}

func TestGateway_ScopeMiddleware_AllowsGatewayTools(t *testing.T) {
	g := newDiscoveryTestGateway()
	mw := g.scopeMiddleware(nil)

	req := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: searchToolsName},
		Extra:  &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{Scopes: []string{"mcp:grafana:read"}}},
	}
	_, err := mw(callToolNext(t))(t.Context(), "tools/call", req)
	require.NoError(t, err)

	// Without discovery enabled the name is not reserved and stays scope-gated.
	g.discovery = false
	_, err = mw(callToolNext(t))(t.Context(), "tools/call", req)
	require.Error(t, err)
}

func TestGateway_Discovery_EndToEnd(t *testing.T) {
	upServerTr, upClientTr := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "0"}, nil)
	srv.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "echo back the message",
		InputSchema: map[string]any{"type": "object"},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	go func() { _ = srv.Run(t.Context(), upServerTr) }()

	cfg := &Config{
		Server: ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{
			{Name: "u1", Kind: "stdio", Command: []string{"ignored"}, Tools: ToolsConfig{Lazy: true}},
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

	downSess, err := mcp.NewClient(&mcp.Implementation{Name: "down", Version: "0"}, nil).Connect(t.Context(), gwClientTr, nil)
	require.NoError(t, err)
	defer downSess.Close()

	// tools/list hides the lazy upstream but keeps the discovery tools.
	list, err := downSess.ListTools(t.Context(), &mcp.ListToolsParams{})
	require.NoError(t, err)
	names := make([]string, 0, len(list.Tools))
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	require.ElementsMatch(t, []string{searchToolsName, describeToolsName}, names)

	// search_tools finds it, describe_tools returns its schema.
	searchRes, err := downSess.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      searchToolsName,
		Arguments: map[string]any{"query": "echo"},
	})
	require.NoError(t, err)
	require.False(t, searchRes.IsError)
	var search searchToolsOutput
	require.NoError(t, json.Unmarshal([]byte(searchRes.Content[0].(*mcp.TextContent).Text), &search))
	require.Equal(t, []toolMatch{{Name: "echo", Upstream: "u1", Description: "echo back the message", Hidden: true}}, search.Tools)

	describeRes, err := downSess.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      describeToolsName,
		Arguments: map[string]any{"names": []string{"echo"}},
	})
	require.NoError(t, err)
	require.False(t, describeRes.IsError)
	var describe struct {
		Tools []*mcp.Tool `json:"tools"`
	}
	require.NoError(t, json.Unmarshal([]byte(describeRes.Content[0].(*mcp.TextContent).Text), &describe))
	require.Len(t, describe.Tools, 1)
	require.Equal(t, "echo", describe.Tools[0].Name)
	require.NotNil(t, describe.Tools[0].InputSchema)

	// A hidden tool is still callable.
	callRes, err := downSess.CallTool(t.Context(), &mcp.CallToolParams{Name: "echo"})
	require.NoError(t, err)
	require.False(t, callRes.IsError)
	require.Equal(t, "ok", callRes.Content[0].(*mcp.TextContent).Text)
}

func TestGateway_Discovery_DisabledByDefault(t *testing.T) {
	cfg := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{{Name: "u1", Kind: "stdio", Command: []string{"ignored"}}},
	}
	g, err := New(cfg, Options{})
	require.NoError(t, err)
	require.False(t, g.discovery)
}
