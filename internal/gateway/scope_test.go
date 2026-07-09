// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

var grafanaScopes = []ScopeConfig{
	{Name: "read", Match: []string{"get_*", "list_*"}},
	{Name: "write", Match: []string{"add_*", "delete_*"}},
}

func TestScopeAllowsTool(t *testing.T) {
	tests := []struct {
		name    string
		granted []string
		raw     string
		want    bool
	}{
		{"base scope grants everything", []string{"mcp:grafana"}, "add_panel", true},
		{"read sub-scope grants matching tool", []string{"mcp:grafana:read"}, "get_dashboard", true},
		{"read sub-scope denies non-matching tool", []string{"mcp:grafana:read"}, "add_panel", false},
		{"write sub-scope grants matching tool", []string{"mcp:grafana:write"}, "delete_panel", true},
		{"unrelated scope denies", []string{"mcp:github"}, "get_dashboard", false},
		{"no scopes denies", nil, "get_dashboard", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scopeAllowsTool(tt.granted, "grafana", grafanaScopes, tt.raw)
			require.Equal(t, tt.want, got)
		})
	}
}

func newScopeTestGateway() *Gateway {
	u := &Upstream{cfg: UpstreamConfig{
		Name:  "grafana",
		Tools: ToolsConfig{Prefix: "grafana.", Scopes: grafanaScopes},
	}}
	return &Gateway{
		upstreams: []*Upstream{u},
		registry: upstreamRegistry{
			finalToUpstream: map[string]string{
				"grafana.get_dashboard": "grafana",
				"grafana.add_dashboard": "grafana",
			},
		},
	}
}

func callToolNext(t *testing.T) mcp.MethodHandler {
	t.Helper()
	return func(_ context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{}, nil
	}
}

func TestGateway_ScopeMiddleware_ToolsCall_AggregateServer(t *testing.T) {
	g := newScopeTestGateway()
	mw := g.scopeMiddleware(nil)

	allowed := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "grafana.get_dashboard"},
		Extra:  &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{Scopes: []string{"mcp:grafana:read"}}},
	}
	_, err := mw(callToolNext(t))(t.Context(), "tools/call", allowed)
	require.NoError(t, err)

	denied := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "grafana.add_dashboard"},
		Extra:  &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{Scopes: []string{"mcp:grafana:read"}}},
	}
	_, err = mw(callToolNext(t))(t.Context(), "tools/call", denied)
	require.Error(t, err)
}

func TestGateway_ScopeMiddleware_ToolsCall_RouteServer(t *testing.T) {
	g := newScopeTestGateway()
	mw := g.scopeMiddleware(g.upstreams[0])

	allowed := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "get_dashboard"},
		Extra:  &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{Scopes: []string{"mcp:grafana:read"}}},
	}
	_, err := mw(callToolNext(t))(t.Context(), "tools/call", allowed)
	require.NoError(t, err)

	denied := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "add_dashboard"},
		Extra:  &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{Scopes: []string{"mcp:grafana:read"}}},
	}
	_, err = mw(callToolNext(t))(t.Context(), "tools/call", denied)
	require.Error(t, err)
}

func TestGateway_ScopeMiddleware_ToolsCall_NoTokenInfo_FullAccess(t *testing.T) {
	g := newScopeTestGateway()
	mw := g.scopeMiddleware(nil)

	req := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "grafana.add_dashboard"},
		Extra:  &mcp.RequestExtra{},
	}
	_, err := mw(callToolNext(t))(t.Context(), "tools/call", req)
	require.NoError(t, err)
}

func TestGateway_ScopeMiddleware_ToolsList_FiltersTools(t *testing.T) {
	g := newScopeTestGateway()
	mw := g.scopeMiddleware(nil)

	next := func(_ context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		return &mcp.ListToolsResult{Tools: []*mcp.Tool{
			{Name: "grafana.get_dashboard"},
			{Name: "grafana.add_dashboard"},
		}}, nil
	}

	req := &mcp.ListToolsRequest{
		Params: &mcp.ListToolsParams{},
		Extra:  &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{Scopes: []string{"mcp:grafana:read"}}},
	}
	res, err := mw(next)(t.Context(), "tools/list", req)
	require.NoError(t, err)
	lt, ok := res.(*mcp.ListToolsResult)
	require.True(t, ok)
	require.Len(t, lt.Tools, 1)
	require.Equal(t, "grafana.get_dashboard", lt.Tools[0].Name)
}
