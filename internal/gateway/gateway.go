// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
//
// The gateway subscribes to tools/listChanged notifications from upstreams via
// client AddReceivingMiddleware (or ToolListChangedHandler); for the scaffold
// it only logs a warning and does not implement re-list/diffing/re-registration.
package gateway

import (
	"context"
	"log/slog"
	"slices"
	"sync"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/mcputil"
)

// ToolMiddleware is the type for call-time tool middlewares (defined here to avoid subpackage import in this file).
type ToolMiddleware = func(mcp.ToolHandler) mcp.ToolHandler

func chain(mws ...ToolMiddleware) ToolMiddleware {
	return func(next mcp.ToolHandler) mcp.ToolHandler {
		for _, mw := range slices.Backward(mws) {
			next = mw(next)
		}
		return next
	}
}

// Gateway aggregates upstreams and re-exports their tools on a local server.
type Gateway struct {
	cfg         *Config
	server      *mcp.Server
	upstreams   []*Upstream
	resolver    SecretResolver
	redactor    *Redactor
	logger      *slog.Logger
	middlewares []ToolMiddleware
	mu          sync.RWMutex
}

// Options for New.
type Options struct {
	Logger      *slog.Logger
	Middlewares []ToolMiddleware
	Redactor    *Redactor
}

func (o *Options) setDefaults() {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// New builds the resolver, redactor, upstream objects (not connected) and local server.
func New(cfg *Config, opts Options) (*Gateway, error) {
	opts.setDefaults()
	res, err := NewSecretResolver(cfg.Secrets)
	if err != nil {
		return nil, err
	}
	red, err := NewRedactor(nil, 0) // TODO: from config if needed
	if err != nil {
		return nil, err
	}
	if opts.Redactor != nil {
		red = opts.Redactor
	}
	g := &Gateway{
		cfg:         cfg,
		resolver:    res,
		redactor:    red,
		logger:      opts.Logger,
		middlewares: opts.Middlewares,
	}
	for _, uc := range cfg.Upstreams {
		u, err := NewUpstream(uc, UpstreamOptions{Logger: opts.Logger, Resolver: res, Redactor: red})
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

// Build connects upstreams, lists tools, checks collisions, registers handlers.
func (g *Gateway) Build(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	prefixes := map[string]string{}
	toolSets := map[string][]string{}

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
		for _, t := range tools {
			toolSets[u.cfg.Name] = append(toolSets[u.cfg.Name], t.Name)
		}
		prefixes[u.cfg.Name] = u.cfg.Tools.Prefix

		// subscribe to listChanged (scaffold: just log)
		// We use client-side handler registration via middleware or opts; simplest is to wrap.
		// For scaffold we log on the client logger when we receive it.
		if u.client != nil {
			u.client.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
					if method == "notifications/tools/list_changed" {
						g.logger.Warn("tools changed, re-sync not implemented")
					}
					return next(ctx, method, req)
				}
			})
		}
	}

	if err := DetectCollisions(prefixes, toolSets); err != nil {
		_ = g.Close(ctx)
		return err
	}

	for _, u := range g.upstreams {
		rawTools, _ := u.ListTools(ctx)
		for _, rt := range rawTools {
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
			h = chain(g.middlewares...)(h)
			g.server.AddTool(final, h)
		}
	}
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
