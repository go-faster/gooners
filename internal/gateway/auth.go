// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"context"
	"net/http"

	"github.com/go-faster/gooners/mcpauth"
)

// HTTPMiddleware returns middleware that enforces optional inbound gateway auth,
// or nil when auth is disabled.
//
// The scopes handed to mcpauth are derived from the configured upstreams: what
// a gateway credential can reach is a property of what it fronts, which is the
// one thing a general auth package cannot know.
func (g *Gateway) HTTPMiddleware() func(http.Handler) http.Handler {
	return mcpauth.Middleware(g.cfg.Auth, mcpauth.Options{
		Name: "mcpgateway",
		// Resolved per request against the live config rather than the snapshot
		// taken here, so a reloaded secret takes effect without a restart.
		Secret: func(ctx context.Context) (string, error) {
			return Interpolate(ctx, g.config().Auth.Value, g.secretResolver())
		},
		Scopes: derivedUpstreamScopes(g.cfg.Upstreams),
	})
}

// derivedUpstreamScopes computes the "mcp:<upstream>" base scope (full access to
// that upstream) and "mcp:<upstream>:<name>" sub-scope for every configured
// ScopeConfig, for every upstream.
func derivedUpstreamScopes(upstreams []UpstreamConfig) []string {
	var out []string
	for _, u := range upstreams {
		out = append(out, upstreamScope(u.Name))
		for _, sc := range u.Tools.Scopes {
			out = append(out, upstreamSubScope(u.Name, sc.Name))
		}
	}

	return out
}

func upstreamScope(upstream string) string {
	return "mcp:" + upstream
}

func upstreamSubScope(upstream, name string) string {
	return "mcp:" + upstream + ":" + name
}
