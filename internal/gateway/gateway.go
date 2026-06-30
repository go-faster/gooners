// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
//
// The gateway subscribes to tools/listChanged notifications from upstreams via
// client AddReceivingMiddleware (or ToolListChangedHandler); for the scaffold
// it only logs a warning and does not implement re-list/diffing/re-registration.
package gateway

import (
	"context"
	"log/slog"
	"sync"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/mcputil"
)

// Gateway aggregates upstreams and re-exports their tools on a local server.
type Gateway struct {
	cfg       *Config
	server    *mcp.Server
	upstreams []*Upstream
	resolver  SecretResolver
	logger    *slog.Logger
	mu        sync.RWMutex
}

// Options for New.
type Options struct {
	Logger *slog.Logger
}

func (o *Options) setDefaults() {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// New builds the resolver, upstream objects (not connected) and local server.
func New(cfg *Config, opts Options) (*Gateway, error) {
	opts.setDefaults()
	res, err := NewSecretResolver(cfg.Secrets)
	if err != nil {
		return nil, err
	}
	g := &Gateway{
		cfg:      cfg,
		resolver: res,
		logger:   opts.Logger,
	}
	for _, uc := range cfg.Upstreams {
		u, err := NewUpstream(uc, UpstreamOptions{
			Logger:            opts.Logger,
			Resolver:          res,
			OnToolListChanged: g.onToolListChanged,
		})
		if err != nil {
			return nil, err
		}
		g.upstreams = append(g.upstreams, u)
	}
	g.server = mcputil.NewServer(mcputil.ServerConfig{
		Name:         cfg.Server.Name,
		Instructions: cfg.Server.Instructions,
		Logger:       opts.Logger,
	})
	return g, nil
}

func (g *Gateway) onToolListChanged(_ context.Context, upstreamName string) error {
	g.logger.Warn("tools changed, re-sync not implemented", "upstream", upstreamName)
	return nil
}

// Build connects upstreams, lists tools, checks collisions, registers handlers.
func (g *Gateway) Build(ctx context.Context) error {
	type listed struct {
		u     *Upstream
		tools []*mcp.Tool
	}
	var listedTools []listed

	for _, u := range g.upstreams {
		if err := u.Connect(ctx); err != nil {
			_ = g.Close(ctx)
			return errors.Wrapf(err, "connect upstream %s", u.cfg.Name)
		}
		tools, err := u.ListTools(ctx)
		if err != nil {
			_ = g.Close(ctx)
			return errors.Wrapf(err, "list tools %s", u.cfg.Name)
		}
		listedTools = append(listedTools, listed{u: u, tools: tools})
	}

	prefixes := map[string]string{}
	toolSets := map[string][]string{}
	for _, lt := range listedTools {
		u := lt.u
		for _, t := range lt.tools {
			toolSets[u.cfg.Name] = append(toolSets[u.cfg.Name], t.Name)
		}
		prefixes[u.cfg.Name] = u.cfg.Tools.Prefix
	}

	if err := DetectCollisions(prefixes, toolSets); err != nil {
		_ = g.Close(ctx)
		return err
	}

	g.mu.Lock()
	for _, lt := range listedTools {
		u := lt.u
		for _, rt := range lt.tools {
			if !u.allowed(rt.Name) {
				continue
			}
			final := &mcp.Tool{
				Name:         NamespaceName(u.cfg.Tools.Prefix, rt.Name),
				Description:  TrimDescription(rt.Description, u.cfg.Tools.DescMax),
				InputSchema:  rt.InputSchema,
				OutputSchema: rt.OutputSchema,
				Annotations:  rt.Annotations,
				Title:        rt.Title,
			}
			orig := rt.Name
			h := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return u.CallTool(ctx, &mcp.CallToolParams{Name: orig, Arguments: req.Params.Arguments})
			}
			g.server.AddTool(final, h)
		}
	}
	g.mu.Unlock()
	return nil
}

// Close closes all upstreams.
func (g *Gateway) Close(ctx context.Context) error {
	for _, u := range g.upstreams {
		_ = u.Close(ctx)
	}
	return nil
}

// Server returns the local MCP server for transport.Run.
func (g *Gateway) Server() *mcp.Server { return g.server }
