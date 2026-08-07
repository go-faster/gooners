// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Names of the gateway's own (synthetic, non-proxied) tools. Everything else the
// aggregate server exposes is an upstream tool.
const (
	searchToolsName   = "search_tools"
	describeToolsName = "describe_tools"
)

// Defaults and caps for search_tools.
const (
	searchDefaultLimit = 20
	searchMaxLimit     = 200
	searchDescMax      = 200
)

// reservedTool reports whether name belongs to the gateway itself rather than
// to an upstream. Such tools have no owning upstream, so scope and lazy
// filtering must let them through, and an upstream may not take the name.
//
// Each name is reserved only while its feature is on: a gateway without
// discovery, or without a blob store, must not shadow an upstream tool that
// happens to share the name.
func (g *Gateway) reservedTool(name string) bool {
	switch name {
	case searchToolsName, describeToolsName:
		return g.discovery
	case blobShareName:
		return g.blobStore != nil
	default:
		return false
	}
}

// isOwnTool reports whether name is one of the tools the gateway itself
// registered on the aggregate server. Route servers (forUpstream != nil) never
// carry them, so a same-named upstream tool there must still go through scope
// checks.
func (g *Gateway) isOwnTool(forUpstream *Upstream, name string) bool {
	return forUpstream == nil && g.reservedTool(name)
}

type searchToolsInput struct {
	Query string `json:"query" jsonschema:"space-separated terms matched case-insensitively against tool names and descriptions; empty matches every tool"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of matches to return (default 20, max 200)"`
}

type toolMatch struct {
	Name        string `json:"name"`
	Upstream    string `json:"upstream"`
	Description string `json:"description,omitempty"`
	// Hidden marks a tool omitted from tools/list; its schema must be fetched
	// with describe_tools before it can be called.
	Hidden bool `json:"hidden,omitempty"`
}

type searchToolsOutput struct {
	Tools []toolMatch `json:"tools"`
	// Total counts every match, including those dropped by limit.
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
}

type describeToolsInput struct {
	Names []string `json:"names" jsonschema:"final tool names to describe, as returned by search_tools"`
}

// registerDiscoveryTools adds the gateway's own search/describe tools to the
// aggregate server. Route servers expose a single upstream and never hide its
// tools, so they do not get them.
func (g *Gateway) registerDiscoveryTools() {
	mcp.AddTool(g.server, &mcp.Tool{
		Name:        searchToolsName,
		Description: "Search the gateway's tool catalog by name and description. Returns names and descriptions only, no input schemas; use describe_tools to get the schema of a tool before calling it. Tools marked hidden are absent from tools/list but callable once described.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, g.handleSearchTools)
	mcp.AddTool(g.server, &mcp.Tool{
		Name:        describeToolsName,
		Description: "Return the full definitions, including input schemas, of the named tools. The definitions are identical to what tools/list reports, so they can be added to the tool set sent to the model as-is.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, g.handleDescribeTools)
}

func (g *Gateway) handleSearchTools(_ context.Context, req *mcp.CallToolRequest, in searchToolsInput) (*mcp.CallToolResult, searchToolsOutput, error) {
	limit := in.Limit
	switch {
	case limit <= 0:
		limit = searchDefaultLimit
	case limit > searchMaxLimit:
		limit = searchMaxLimit
	}

	terms := strings.Fields(strings.ToLower(in.Query))
	type scored struct {
		match toolMatch
		score int
	}
	var matches []scored
	for name, tool := range g.visibleTools(req) {
		score, ok := searchScore(terms, name, tool.Description)
		if !ok {
			continue
		}
		u, _ := g.resolveToolOwner(nil, name)
		matches = append(matches, scored{
			match: toolMatch{
				Name:        name,
				Upstream:    u.cfg.Name,
				Description: TrimDescription(tool.Description, searchDescMax),
				Hidden:      u.cfg.Tools.lazy(),
			},
			score: score,
		})
	}
	slices.SortFunc(matches, func(a, b scored) int {
		if c := cmp.Compare(a.score, b.score); c != 0 {
			return c
		}
		return cmp.Compare(a.match.Name, b.match.Name)
	})

	out := searchToolsOutput{Total: len(matches), Tools: []toolMatch{}}
	if len(matches) > limit {
		matches = matches[:limit]
		out.Truncated = true
	}
	for _, m := range matches {
		out.Tools = append(out.Tools, m.match)
	}
	return nil, out, nil
}

// handleDescribeTools returns the requested tools verbatim from the registry, so
// the JSON a client splices into its tool set is exactly what tools/list emits.
// Unknown names and names the caller's scopes hide are reported the same way:
// the catalog must not confirm the existence of a tool the caller cannot see.
func (g *Gateway) handleDescribeTools(_ context.Context, req *mcp.CallToolRequest, in describeToolsInput) (*mcp.CallToolResult, any, error) {
	if len(in.Names) == 0 {
		return nil, nil, errors.New("names is required")
	}
	visible := g.visibleTools(req)
	res := struct {
		Tools    []*mcp.Tool `json:"tools"`
		NotFound []string    `json:"not_found,omitempty"`
	}{Tools: []*mcp.Tool{}}
	for _, name := range in.Names {
		tool, ok := visible[name]
		if !ok {
			res.NotFound = append(res.NotFound, name)
			continue
		}
		res.Tools = append(res.Tools, tool)
	}
	data, err := json.Marshal(res)
	if err != nil {
		return nil, nil, errors.Wrap(err, "marshal tools")
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
}

// visibleTools snapshots the registered upstream tools the request's credential
// is allowed to see, applying the same predicate scopeMiddleware applies to
// tools/list. Gateway tools are excluded: they are not in the registry.
func (g *Gateway) visibleTools(req *mcp.CallToolRequest) map[string]*mcp.Tool {
	g.registryMu.RLock()
	out := make(map[string]*mcp.Tool, len(g.registry.registeredTools))
	maps.Copy(out, g.registry.registeredTools)
	g.registryMu.RUnlock()

	// Scopes only restrict OAuth-issued tokens; other auth paths see everything,
	// exactly as in scopeMiddleware.
	extra := req.GetExtra()
	scoped := extra != nil && extra.TokenInfo != nil
	for name := range out {
		u, rawName := g.resolveToolOwner(nil, name)
		if u == nil {
			delete(out, name)
			continue
		}
		if scoped && !scopeAllowsTool(extra.TokenInfo.Scopes, u.cfg.Name, u.cfg.Tools.Scopes, rawName) {
			delete(out, name)
		}
	}
	return out
}

// searchScore reports whether every term occurs in the tool's name or description,
// and how good the match is (lower is better): an exact name hit beats a
// name-only hit, which beats one that needed the description.
func searchScore(terms []string, name, description string) (int, bool) {
	if len(terms) == 0 {
		return 1, true
	}
	lowerName := strings.ToLower(name)
	lowerDesc := strings.ToLower(description)
	score := 1
	for _, term := range terms {
		switch {
		case strings.Contains(lowerName, term):
		case strings.Contains(lowerDesc, term):
			score = 2
		default:
			return 0, false
		}
	}
	if len(terms) == 1 && lowerName == terms[0] {
		return 0, true
	}
	return score, true
}

// lazyMiddleware drops tools belonging to lazy upstreams from tools/list. It
// deliberately does not gate tools/call: registration stays global, so a client
// that obtained a schema from describe_tools can call the tool without the
// gateway remembering anything about the session.
func (g *Gateway) lazyMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/list" {
				return next(ctx, method, req)
			}
			res, err := next(ctx, method, req)
			if err != nil {
				return res, err
			}
			lt, ok := res.(*mcp.ListToolsResult)
			if !ok {
				return res, nil
			}
			lt.Tools = slices.DeleteFunc(slices.Clone(lt.Tools), func(t *mcp.Tool) bool {
				u, _ := g.resolveToolOwner(nil, t.Name)
				return u != nil && u.cfg.Tools.lazy()
			})
			return lt, nil
		}
	}
}
