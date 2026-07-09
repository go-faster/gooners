// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// scopeMiddleware restricts tools/call and filters tools/list results to what the
// caller's OAuth-granted scopes permit.
//
// forUpstream is nil for the aggregate gateway server, which multiplexes tools
// from every upstream under (optionally prefixed) names; the owning upstream is
// resolved per tool via the gateway's tool registry. It is non-nil for a
// route-specific single-upstream server, where every tool already belongs to
// forUpstream under its unprefixed name.
//
// Requests with no TokenInfo (the static shared-secret auth path, or auth
// disabled) are left unrestricted: scopes only apply to OAuth-issued tokens.
func (g *Gateway) scopeMiddleware(forUpstream *Upstream) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			extra := req.GetExtra()
			if extra == nil || extra.TokenInfo == nil {
				return next(ctx, method, req)
			}
			switch method {
			case "tools/call":
				call, ok := req.(*mcp.CallToolRequest)
				if !ok {
					return next(ctx, method, req)
				}
				u, rawName := g.resolveToolOwner(forUpstream, call.Params.Name)
				if u == nil || !scopeAllowsTool(extra.TokenInfo.Scopes, u.cfg.Name, u.cfg.Tools.Scopes, rawName) {
					return nil, fmt.Errorf("tool %q not permitted by granted OAuth scope", call.Params.Name)
				}
				return next(ctx, method, req)
			case "tools/list":
				res, err := next(ctx, method, req)
				if err != nil {
					return res, err
				}
				lt, ok := res.(*mcp.ListToolsResult)
				if !ok {
					return res, nil
				}
				lt.Tools = slices.DeleteFunc(slices.Clone(lt.Tools), func(t *mcp.Tool) bool {
					u, rawName := g.resolveToolOwner(forUpstream, t.Name)
					return u == nil || !scopeAllowsTool(extra.TokenInfo.Scopes, u.cfg.Name, u.cfg.Tools.Scopes, rawName)
				})
				return lt, nil
			default:
				return next(ctx, method, req)
			}
		}
	}
}

// resolveToolOwner returns the upstream owning finalName and finalName with that
// upstream's tool prefix stripped (the name ScopeConfig.Match patterns match
// against). forUpstream, when non-nil, is returned directly since route servers
// expose only their own upstream's tools under unprefixed names.
func (g *Gateway) resolveToolOwner(forUpstream *Upstream, finalName string) (owner *Upstream, rawName string) {
	if forUpstream != nil {
		return forUpstream, finalName
	}
	g.registryMu.RLock()
	upstreamName, ok := g.registry.finalToUpstream[finalName]
	g.registryMu.RUnlock()
	if !ok {
		return nil, ""
	}
	u := g.upstreamByName(upstreamName)
	if u == nil {
		return nil, ""
	}
	return u, strings.TrimPrefix(finalName, u.cfg.Tools.Prefix)
}

// scopeAllowsTool reports whether granted covers rawName in upstream, either via
// the upstream's base "mcp:<upstream>" scope (full access) or a named sub-scope
// ("mcp:<upstream>:<name>") whose Match patterns include rawName.
func scopeAllowsTool(granted []string, upstream string, scopes []ScopeConfig, rawName string) bool {
	if slices.Contains(granted, upstreamScope(upstream)) {
		return true
	}
	for _, sc := range scopes {
		if !slices.Contains(granted, upstreamSubScope(upstream, sc.Name)) {
			continue
		}
		for _, pat := range sc.Match {
			if ok, _ := path.Match(pat, rawName); ok {
				return true
			}
		}
	}
	return false
}
