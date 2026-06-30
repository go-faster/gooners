// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
//
// The gateway subscribes to tools/listChanged notifications from upstreams and
// re-syncs by re-listing, diffing final names, and using AddTool/RemoveTools.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"sync"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/go-faster/gooners/internal/gateway/middleware"
	"github.com/go-faster/gooners/internal/mcputil"
)

// Gateway aggregates upstreams and re-exports their tools on a local server.
type Gateway struct {
	cfg       *Config
	upstreams []*Upstream
	server    *mcp.Server
	resolver  SecretResolver

	registry   upstreamRegistry
	registryMu sync.RWMutex

	logger  *zap.Logger
	slogger *slog.Logger
	mp      metric.MeterProvider
	tp      trace.TracerProvider
}

type upstreamRegistry struct {
	finalToUpstream    map[string]string
	upstreamRegistered map[string]map[string]struct{}
	registeredTools    map[string]*mcp.Tool
}

func (g *Gateway) upstreamByName(name string) *Upstream {
	for _, u := range g.upstreams {
		if u.cfg.Name == name {
			return u
		}
	}
	return nil
}

func toolEqual(a, b *mcp.Tool) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Name != b.Name || a.Title != b.Title || a.Description != b.Description {
		return false
	}
	aj, _ := json.Marshal(a.InputSchema)
	bj, _ := json.Marshal(b.InputSchema)
	if !bytes.Equal(aj, bj) {
		return false
	}
	aj, _ = json.Marshal(a.OutputSchema)
	bj, _ = json.Marshal(b.OutputSchema)
	if !bytes.Equal(aj, bj) {
		return false
	}
	aj, _ = json.Marshal(a.Annotations)
	bj, _ = json.Marshal(b.Annotations)
	return bytes.Equal(aj, bj)
}

// Options for New.
type Options struct {
	MeterProvider  metric.MeterProvider
	TracerProvider trace.TracerProvider
	Logger         *zap.Logger
	Slogger        *slog.Logger
}

func (o *Options) setDefaults() {
	if o.MeterProvider == nil {
		o.MeterProvider = otel.GetMeterProvider()
	}
	if o.TracerProvider == nil {
		o.TracerProvider = otel.GetTracerProvider()
	}
	if o.Logger == nil {
		o.Logger = zap.L()
	}
	if o.Slogger == nil {
		o.Slogger = slog.Default()
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
		registry: upstreamRegistry{
			finalToUpstream:    make(map[string]string),
			upstreamRegistered: make(map[string]map[string]struct{}),
			registeredTools:    make(map[string]*mcp.Tool),
		},
		server:     &mcp.Server{},
		upstreams:  []*Upstream{},
		registryMu: sync.RWMutex{},
		mp:         opts.MeterProvider,
		tp:         opts.TracerProvider,
		logger:     opts.Logger,
		slogger:    opts.Slogger,
	}
	for _, uc := range cfg.Upstreams {
		if g.registry.upstreamRegistered[uc.Name] == nil {
			g.registry.upstreamRegistered[uc.Name] = make(map[string]struct{})
		}
	}
	for _, uc := range cfg.Upstreams {
		u, err := NewUpstream(uc, UpstreamOptions{
			Logger:            opts.Slogger.With("upstream", uc.Name),
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
		Logger:       opts.Slogger.With("component", "server"),
	})
	return g, nil
}

func (g *Gateway) onToolListChanged(ctx context.Context, upstreamName string) error {
	u := g.upstreamByName(upstreamName)
	if u == nil {
		g.logger.Warn("tools changed for unknown upstream", zap.String("upstream", upstreamName))
		return nil
	}
	tools, err := u.ListTools(ctx)
	if err != nil {
		g.logger.Warn("re-list tools failed", zap.String("upstream", upstreamName), zap.Error(err))
		return err
	}
	added, removed, collisions := g.registerUpstreamTools(u, tools)
	if len(collisions) > 0 {
		g.logger.Warn("re-sync collisions", zap.String("upstream", upstreamName), zap.Int("collisions", len(collisions)))
	}
	g.logger.Info("tools re-synced",
		zap.String("upstream", upstreamName),
		zap.Int("added", len(added)),
		zap.Int("removed", len(removed)),
		zap.Int("collisions", len(collisions)),
	)
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

	for _, lt := range listedTools {
		u := lt.u
		added, removed, collisions := g.registerUpstreamTools(u, lt.tools)
		if len(collisions) > 0 {
			g.logger.Warn("build collisions", zap.String("upstream", u.cfg.Name), zap.Int("collisions", len(collisions)))
		}
		g.logger.Info("tools registered",
			zap.String("upstream", u.cfg.Name),
			zap.Int("added", len(added)),
			zap.Int("removed", len(removed)),
			zap.Int("collisions", len(collisions)),
		)
	}
	return nil
}

// Server returns the local MCP server for transport.Run.
func (g *Gateway) Server() *mcp.Server { return g.server }

func (g *Gateway) registerUpstreamTools(u *Upstream, rawTools []*mcp.Tool) (added, removed []string, collisions []Collision) {
	g.registryMu.Lock()
	defer g.registryMu.Unlock()

	if g.registry.upstreamRegistered[u.cfg.Name] == nil {
		g.registry.upstreamRegistered[u.cfg.Name] = make(map[string]struct{})
	}

	prev := g.registry.upstreamRegistered[u.cfg.Name]
	newSet := map[string]struct{}{}
	toolByFinal := map[string]*mcp.Tool{}
	rawNameByFinal := map[string]string{}

	for _, rt := range rawTools {
		if !u.allowed(rt.Name) {
			continue
		}
		finalName := NamespaceName(u.cfg.Tools.Prefix, rt.Name)
		newSet[finalName] = struct{}{}
		toolByFinal[finalName] = &mcp.Tool{
			Name:         finalName,
			Description:  TrimDescription(rt.Description, u.cfg.Tools.DescMax),
			InputSchema:  rt.InputSchema,
			OutputSchema: rt.OutputSchema,
			Annotations:  rt.Annotations,
			Title:        rt.Title,
		}
		rawNameByFinal[finalName] = rt.Name
	}

	for name := range prev {
		if _, still := newSet[name]; still {
			continue
		}
		g.server.RemoveTools(name)
		delete(g.registry.finalToUpstream, name)
		delete(g.registry.registeredTools, name)
		delete(g.registry.upstreamRegistered[u.cfg.Name], name)
		removed = append(removed, name)
	}

	for name, final := range toolByFinal {
		owner, owned := g.registry.finalToUpstream[name]
		prevTool := g.registry.registeredTools[name]
		changed := !toolEqual(prevTool, final)
		if owned && owner != u.cfg.Name {
			collisions = append(collisions, Collision{Upstream: owner, Tool: "", ResultName: name})
			continue
		}
		if !owned || changed {
			orig := rawNameByFinal[name]
			h := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return u.CallTool(ctx, &mcp.CallToolParams{Name: orig, Arguments: req.Params.Arguments})
			}
			mw, err := middleware.NewTelemetry(h, middleware.TelemetryOptions{
				Upstream:       u.cfg.Name,
				MeterProvider:  g.mp,
				TracerProvider: g.tp,
				Logger:         g.logger,
			})
			if err != nil {
				g.slogger.Error("failed to create telemetry middleware", "error", err)
			} else {
				h = mw
			}
			g.server.AddTool(final, h)
			g.registry.finalToUpstream[name] = u.cfg.Name
			g.registry.registeredTools[name] = final
			if !owned {
				added = append(added, name)
			}
		}
		g.registry.upstreamRegistered[u.cfg.Name][name] = struct{}{}
	}

	// Ensure the per-upstream set is exactly the names we ended up owning.
	g.registry.upstreamRegistered[u.cfg.Name] = map[string]struct{}{}
	for n := range newSet {
		if _, ok := g.registry.finalToUpstream[n]; ok && g.registry.finalToUpstream[n] == u.cfg.Name {
			g.registry.upstreamRegistered[u.cfg.Name][n] = struct{}{}
		}
	}

	return added, removed, collisions
}

// RegisteredTools returns a snapshot of finalName -> upstreamName for tests.
func (g *Gateway) RegisteredTools() map[string]string {
	g.registryMu.RLock()
	defer g.registryMu.RUnlock()
	out := make(map[string]string, len(g.registry.finalToUpstream))
	maps.Copy(out, g.registry.finalToUpstream)
	return out
}

// Close closes all upstreams.
func (g *Gateway) Close(ctx context.Context) error {
	for _, u := range g.upstreams {
		_ = u.Close(ctx)
	}
	g.registryMu.Lock()
	clear(g.registry.finalToUpstream)
	clear(g.registry.upstreamRegistered)
	clear(g.registry.registeredTools)
	g.registryMu.Unlock()
	return nil
}
