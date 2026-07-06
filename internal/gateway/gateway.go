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
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/go-faster/gooners/internal/gateway/middleware"
	"github.com/go-faster/gooners/internal/gateway/router"
	"github.com/go-faster/gooners/internal/mcputil"
)

// Gateway aggregates upstreams and re-exports their tools on a local server.
type Gateway struct {
	cfg       *Config
	upstreams []*Upstream
	server    *mcp.Server
	resolver  SecretResolver
	routes    []routedServer
	router    *router.Router[*mcp.Server]

	registry   upstreamRegistry
	registryMu sync.RWMutex
	routeMu    sync.RWMutex

	promptRegistry           featureRegistry[*mcp.Prompt]
	resourceRegistry         featureRegistry[*mcp.Resource]
	resourceTemplateRegistry featureRegistry[*mcp.ResourceTemplate]

	logger   *zap.Logger
	slogger  *slog.Logger
	mp       metric.MeterProvider
	tp       trace.TracerProvider
	redactor *Redactor
}

type routedServer struct {
	upstream string
	host     string
	path     string
	server   *mcp.Server
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

func promptEqual(a, b *mcp.Prompt) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Name != b.Name || a.Title != b.Title || a.Description != b.Description {
		return false
	}
	aj, _ := json.Marshal(a.Arguments)
	bj, _ := json.Marshal(b.Arguments)
	if !bytes.Equal(aj, bj) {
		return false
	}
	aj, _ = json.Marshal(a.Meta)
	bj, _ = json.Marshal(b.Meta)
	return bytes.Equal(aj, bj)
}

func resourceEqual(a, b *mcp.Resource) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.URI != b.URI || a.Name != b.Name || a.Title != b.Title || a.Description != b.Description || a.MIMEType != b.MIMEType || a.Size != b.Size {
		return false
	}
	aj, _ := json.Marshal(a.Annotations)
	bj, _ := json.Marshal(b.Annotations)
	return bytes.Equal(aj, bj)
}

func resourceTemplateEqual(a, b *mcp.ResourceTemplate) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.URITemplate != b.URITemplate || a.Name != b.Name || a.Title != b.Title || a.Description != b.Description || a.MIMEType != b.MIMEType {
		return false
	}
	aj, _ := json.Marshal(a.Annotations)
	bj, _ := json.Marshal(b.Annotations)
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

	res, err := NewSecretResolver(cfg.Secrets, opts.Slogger.With("component", "secretresolver"))
	if err != nil {
		return nil, err
	}

	var redactor *Redactor
	if cfg.Redact.Enabled {
		var err error
		redactor, err = NewRedactor(cfg.Redact.Patterns, cfg.Redact.MinEntropy)
		if err != nil {
			return nil, errors.Wrap(err, "create redactor")
		}
	}

	g := &Gateway{
		cfg:      cfg,
		resolver: res,
		registry: upstreamRegistry{
			finalToUpstream:    make(map[string]string),
			upstreamRegistered: make(map[string]map[string]struct{}),
			registeredTools:    make(map[string]*mcp.Tool),
		},
		promptRegistry:           newFeatureRegistry[*mcp.Prompt](promptEqual),
		resourceRegistry:         newFeatureRegistry[*mcp.Resource](resourceEqual),
		resourceTemplateRegistry: newFeatureRegistry[*mcp.ResourceTemplate](resourceTemplateEqual),
		upstreams:                []*Upstream{},
		registryMu:               sync.RWMutex{},
		mp:                       opts.MeterProvider,
		tp:                       opts.TracerProvider,
		logger:                   opts.Logger,
		slogger:                  opts.Slogger,
		redactor:                 redactor,
	}
	for _, uc := range cfg.Upstreams {
		if g.registry.upstreamRegistered[uc.Name] == nil {
			g.registry.upstreamRegistered[uc.Name] = make(map[string]struct{})
		}
	}
	for _, uc := range cfg.Upstreams {
		uopts := UpstreamOptions{
			Logger:                opts.Slogger.With("upstream", uc.Name),
			Resolver:              res,
			OnToolListChanged:     g.onToolListChanged,
			OnPromptListChanged:   g.onPromptListChanged,
			OnResourceListChanged: g.onResourceListChanged,
			OnResourceUpdated:     g.onResourceUpdated,
			OnReconnect:           g.onUpstreamReconnect,
		}

		uopts.Redactor = redactor
		if uc.Redact != nil {
			if uc.Redact.Enabled {
				r, err := NewRedactor(uc.Redact.Patterns, uc.Redact.MinEntropy)
				if err != nil {
					return nil, errors.Wrapf(err, "upstream %q: create redactor", uc.Name)
				}
				uopts.Redactor = r
			} else {
				uopts.Redactor = nil
			}
		}

		if uc.Reconnect != nil {
			if err := applyReconnectConfig(uc.Reconnect, &uopts); err != nil {
				return nil, errors.Wrapf(err, "parse reconnect config for upstream %s", uc.Name)
			}
		}
		u, err := NewUpstream(uc, uopts)
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
		for _, c := range collisions {
			g.logger.Warn("tool collision: name already owned by another upstream",
				zap.String("upstream", upstreamName),
				zap.String("owner", c.Upstream),
				zap.String("tool", c.Tool),
				zap.String("result_name", c.ResultName),
			)
		}
	}
	g.logger.Info("tools re-synced",
		zap.String("upstream", upstreamName),
		zap.Int("added", len(added)),
		zap.Int("removed", len(removed)),
		zap.Int("collisions", len(collisions)),
	)
	if err := g.refreshRouteServer(ctx, u, routeRefresh{tools: tools, haveTools: true}); err != nil {
		g.logger.Warn("route server refresh failed", zap.String("upstream", upstreamName), zap.Error(err))
	}
	return nil
}

func (g *Gateway) onPromptListChanged(ctx context.Context, upstreamName string) error {
	u := g.upstreamByName(upstreamName)
	if u == nil {
		g.logger.Warn("prompts changed for unknown upstream", zap.String("upstream", upstreamName))
		return nil
	}
	prompts, err := u.ListPrompts(ctx)
	if err != nil {
		g.logger.Warn("re-list prompts failed", zap.String("upstream", upstreamName), zap.Error(err))
		return err
	}
	added, removed, collisions := g.registerUpstreamPrompts(u, prompts)
	if len(collisions) > 0 {
		for _, c := range collisions {
			g.logger.Warn("prompt collision: name already owned by another upstream",
				zap.String("upstream", upstreamName),
				zap.String("owner", c.Upstream),
				zap.String("tool", c.Tool),
				zap.String("result_name", c.ResultName),
			)
		}
	}
	g.logger.Info("prompts re-synced",
		zap.String("upstream", upstreamName),
		zap.Int("added", len(added)),
		zap.Int("removed", len(removed)),
		zap.Int("collisions", len(collisions)),
	)
	if err := g.refreshRouteServer(ctx, u, routeRefresh{prompts: prompts, havePrompts: true}); err != nil {
		g.logger.Warn("route server refresh failed", zap.String("upstream", upstreamName), zap.Error(err))
	}
	return nil
}

func (g *Gateway) onResourceListChanged(ctx context.Context, upstreamName string) error {
	u := g.upstreamByName(upstreamName)
	if u == nil {
		g.logger.Warn("resources changed for unknown upstream", zap.String("upstream", upstreamName))
		return nil
	}
	resources, err := u.ListResources(ctx)
	if err != nil {
		g.logger.Warn("re-list resources failed", zap.String("upstream", upstreamName), zap.Error(err))
		return err
	}
	templates, err := u.ListResourceTemplates(ctx)
	if err != nil {
		g.logger.Warn("re-list resource templates failed", zap.String("upstream", upstreamName), zap.Error(err))
		return err
	}
	addedR, removedR, collisionsR := g.registerUpstreamResources(u, resources)
	addedT, removedT, collisionsT := g.registerUpstreamResourceTemplates(u, templates)
	collisions := make([]Collision, 0, len(collisionsR)+len(collisionsT))
	collisions = append(collisions, collisionsR...)
	collisions = append(collisions, collisionsT...)
	if len(collisions) > 0 {
		for _, c := range collisions {
			g.logger.Warn("resource collision: name already owned by another upstream",
				zap.String("upstream", upstreamName),
				zap.String("owner", c.Upstream),
				zap.String("tool", c.Tool),
				zap.String("result_name", c.ResultName),
			)
		}
	}
	g.logger.Info("resources re-synced",
		zap.String("upstream", upstreamName),
		zap.Int("added", len(addedR)+len(addedT)),
		zap.Int("removed", len(removedR)+len(removedT)),
		zap.Int("collisions", len(collisions)),
	)
	if err := g.refreshRouteServer(ctx, u, routeRefresh{resources: resources, haveResources: true, templates: templates, haveTemplates: true}); err != nil {
		g.logger.Warn("route server refresh failed", zap.String("upstream", upstreamName), zap.Error(err))
	}
	return nil
}

func (g *Gateway) onResourceUpdated(ctx context.Context, upstreamName, uri string) error {
	g.logger.Info("resource updated", zap.String("upstream", upstreamName), zap.String("uri", uri))
	return g.server.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: uri})
}

func applyReconnectConfig(cfg *ReconnectConfig, opts *UpstreamOptions) error {
	if cfg.KeepAlive != "" {
		d, err := time.ParseDuration(cfg.KeepAlive)
		if err != nil {
			return errors.Wrap(err, "keepalive")
		}
		opts.KeepAlive = d
	}
	if cfg.InitialBackoff != "" {
		d, err := time.ParseDuration(cfg.InitialBackoff)
		if err != nil {
			return errors.Wrap(err, "initial_backoff")
		}
		opts.ReconnectInitial = d
	}
	if cfg.MaxBackoff != "" {
		d, err := time.ParseDuration(cfg.MaxBackoff)
		if err != nil {
			return errors.Wrap(err, "max_backoff")
		}
		opts.ReconnectMax = d
	}
	return nil
}

func (g *Gateway) onUpstreamReconnect(ctx context.Context, upstreamName string) error {
	u := g.upstreamByName(upstreamName)
	if u == nil {
		g.logger.Warn("reconnect for unknown upstream", zap.String("upstream", upstreamName))
		return nil
	}

	var route routeRefresh
	if tools, err := u.ListTools(ctx); err != nil {
		g.logger.Warn("reconnect list tools failed", zap.String("upstream", upstreamName), zap.Error(err))
	} else {
		route.tools = tools
		route.haveTools = true
		added, removed, collisions := g.registerUpstreamTools(u, tools)
		if len(collisions) > 0 {
			for _, c := range collisions {
				g.logger.Warn("tool collision: name already owned by another upstream",
					zap.String("upstream", upstreamName),
					zap.String("owner", c.Upstream),
					zap.String("tool", c.Tool),
					zap.String("result_name", c.ResultName),
				)
			}
		}
		g.logger.Info("tools re-registered after reconnect",
			zap.String("upstream", upstreamName),
			zap.Int("added", len(added)),
			zap.Int("removed", len(removed)),
			zap.Int("collisions", len(collisions)),
		)
	}

	if prompts, err := u.ListPrompts(ctx); err != nil {
		g.logger.Warn("reconnect list prompts failed", zap.String("upstream", upstreamName), zap.Error(err))
	} else {
		route.prompts = prompts
		route.havePrompts = true
		added, removed, collisions := g.registerUpstreamPrompts(u, prompts)
		if len(collisions) > 0 {
			for _, c := range collisions {
				g.logger.Warn("prompt collision: name already owned by another upstream",
					zap.String("upstream", upstreamName),
					zap.String("owner", c.Upstream),
					zap.String("tool", c.Tool),
					zap.String("result_name", c.ResultName),
				)
			}
		}
		g.logger.Info("prompts re-registered after reconnect",
			zap.String("upstream", upstreamName),
			zap.Int("added", len(added)),
			zap.Int("removed", len(removed)),
			zap.Int("collisions", len(collisions)),
		)
	}

	resources, err := u.ListResources(ctx)
	if err != nil {
		g.logger.Warn("reconnect list resources failed", zap.String("upstream", upstreamName), zap.Error(err))
	}
	templates, templateErr := u.ListResourceTemplates(ctx)
	if templateErr != nil {
		g.logger.Warn("reconnect list resource templates failed", zap.String("upstream", upstreamName), zap.Error(templateErr))
	}
	if err == nil {
		route.resources = resources
		route.haveResources = true
		added, removed, collisions := g.registerUpstreamResources(u, resources)
		if len(collisions) > 0 {
			for _, c := range collisions {
				g.logger.Warn("resource collision: name already owned by another upstream",
					zap.String("upstream", upstreamName),
					zap.String("owner", c.Upstream),
					zap.String("tool", c.Tool),
					zap.String("result_name", c.ResultName),
				)
			}
		}
		g.logger.Info("resources re-registered after reconnect",
			zap.String("upstream", upstreamName),
			zap.Int("added", len(added)),
			zap.Int("removed", len(removed)),
			zap.Int("collisions", len(collisions)),
		)
	}
	if templateErr == nil {
		route.templates = templates
		route.haveTemplates = true
		added, removed, collisions := g.registerUpstreamResourceTemplates(u, templates)
		if len(collisions) > 0 {
			for _, c := range collisions {
				g.logger.Warn("resource template collision: name already owned by another upstream",
					zap.String("upstream", upstreamName),
					zap.String("owner", c.Upstream),
					zap.String("tool", c.Tool),
					zap.String("result_name", c.ResultName),
				)
			}
		}
		g.logger.Info("resource templates re-registered after reconnect",
			zap.String("upstream", upstreamName),
			zap.Int("added", len(added)),
			zap.Int("removed", len(removed)),
			zap.Int("collisions", len(collisions)),
		)
	}
	if err := g.refreshRouteServer(ctx, u, route); err != nil {
		g.logger.Warn("route server refresh failed", zap.String("upstream", upstreamName), zap.Error(err))
	}
	return nil
}

// Build connects upstreams, lists tools, checks collisions, registers handlers.
func (g *Gateway) Build(ctx context.Context) error {
	type listed struct {
		u         *Upstream
		tools     []*mcp.Tool
		prompts   []*mcp.Prompt
		resources []*mcp.Resource
		templates []*mcp.ResourceTemplate
	}
	var listedItems []listed

	for _, u := range g.upstreams {
		if err := u.Connect(ctx); err != nil {
			g.logger.Warn("upstream unavailable during build; will retry in background", zap.String("upstream", u.cfg.Name), zap.Error(err))
			continue
		}
		tools, err := u.ListTools(ctx)
		if err != nil {
			g.logger.Warn("list tools failed during build; skipping upstream", zap.String("upstream", u.cfg.Name), zap.Error(err))
			u.closeSessionResources()
			continue
		}
		prompts, err := u.ListPrompts(ctx)
		if err != nil {
			g.logger.Warn("list prompts failed during build; skipping upstream", zap.String("upstream", u.cfg.Name), zap.Error(err))
			u.closeSessionResources()
			continue
		}
		resources, err := u.ListResources(ctx)
		if err != nil {
			g.logger.Warn("list resources failed during build; skipping upstream", zap.String("upstream", u.cfg.Name), zap.Error(err))
			u.closeSessionResources()
			continue
		}
		templates, err := u.ListResourceTemplates(ctx)
		if err != nil {
			g.logger.Warn("list resource templates failed during build; skipping upstream", zap.String("upstream", u.cfg.Name), zap.Error(err))
			u.closeSessionResources()
			continue
		}
		listedItems = append(listedItems, listed{u: u, tools: tools, prompts: prompts, resources: resources, templates: templates})
	}

	var (
		prefixes     = map[string]string{}
		toolSets     = map[string][]string{}
		promptSets   = map[string][]string{}
		resourceSets = map[string][]string{}
		templateSets = map[string][]string{}
	)
	for _, lt := range listedItems {
		u := lt.u
		for _, t := range lt.tools {
			toolSets[u.cfg.Name] = append(toolSets[u.cfg.Name], t.Name)
		}
		for _, p := range lt.prompts {
			promptSets[u.cfg.Name] = append(promptSets[u.cfg.Name], p.Name)
		}
		for _, r := range lt.resources {
			resourceSets[u.cfg.Name] = append(resourceSets[u.cfg.Name], r.URI)
		}
		for _, t := range lt.templates {
			templateSets[u.cfg.Name] = append(templateSets[u.cfg.Name], t.URITemplate)
		}
		prefixes[u.cfg.Name] = u.cfg.Tools.Prefix
	}

	if err := DetectCollisions(prefixes, toolSets); err != nil {
		_ = g.Close(ctx)
		return err
	}
	if err := DetectCollisions(prefixes, promptSets); err != nil {
		_ = g.Close(ctx)
		return err
	}
	if err := DetectCollisions(map[string]string{}, resourceSets); err != nil {
		_ = g.Close(ctx)
		return err
	}
	if err := DetectCollisions(map[string]string{}, templateSets); err != nil {
		_ = g.Close(ctx)
		return err
	}

	for _, lt := range listedItems {
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
		addedP, removedP, collisionsP := g.registerUpstreamPrompts(u, lt.prompts)
		if len(collisionsP) > 0 {
			g.logger.Warn("build collisions", zap.String("upstream", u.cfg.Name), zap.Int("collisions", len(collisionsP)))
		}
		g.logger.Info("prompts registered",
			zap.String("upstream", u.cfg.Name),
			zap.Int("added", len(addedP)),
			zap.Int("removed", len(removedP)),
			zap.Int("collisions", len(collisionsP)),
		)
		addedR, removedR, collisionsR := g.registerUpstreamResources(u, lt.resources)
		addedT, removedT, collisionsT := g.registerUpstreamResourceTemplates(u, lt.templates)
		collisionsAll := make([]Collision, 0, len(collisionsR)+len(collisionsT))
		collisionsAll = append(collisionsAll, collisionsR...)
		collisionsAll = append(collisionsAll, collisionsT...)
		if len(collisionsAll) > 0 {
			g.logger.Warn("build collisions", zap.String("upstream", u.cfg.Name), zap.Int("collisions", len(collisionsAll)))
		}
		g.logger.Info("resources registered",
			zap.String("upstream", u.cfg.Name),
			zap.Int("added", len(addedR)+len(addedT)),
			zap.Int("removed", len(removedR)+len(removedT)),
			zap.Int("collisions", len(collisionsAll)),
		)
		if u.hasRoute() {
			g.setRouteServer(u, g.newUpstreamRouteServer(u, lt.tools, lt.prompts, lt.resources, lt.templates))
		}
	}
	return nil
}

// Server returns the local MCP server for transport.Run.
func (g *Gateway) Server() *mcp.Server { return g.server }

// ServerForRequest returns a route-specific upstream server when the request
// host/path matches an upstream route, otherwise the aggregate gateway server.
func (g *Gateway) ServerForRequest(req *http.Request) *mcp.Server {
	if req == nil {
		return g.server
	}
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	g.routeMu.RLock()
	rt := g.router
	g.routeMu.RUnlock()

	if rt != nil {
		if best, ok := rt.Lookup(host, req.URL.Path); ok {
			return best
		}
	}
	return g.server
}

type routeRefresh struct {
	tools         []*mcp.Tool
	haveTools     bool
	prompts       []*mcp.Prompt
	havePrompts   bool
	resources     []*mcp.Resource
	haveResources bool
	templates     []*mcp.ResourceTemplate
	haveTemplates bool
}

func (g *Gateway) refreshRouteServer(ctx context.Context, u *Upstream, known routeRefresh) error {
	if !u.hasRoute() {
		return nil
	}
	tools := known.tools
	if !known.haveTools {
		var err error
		tools, err = u.ListTools(ctx)
		if err != nil {
			return errors.Wrap(err, "list tools")
		}
	}
	prompts := known.prompts
	if !known.havePrompts {
		var err error
		prompts, err = u.ListPrompts(ctx)
		if err != nil {
			return errors.Wrap(err, "list prompts")
		}
	}
	resources := known.resources
	if !known.haveResources {
		var err error
		resources, err = u.ListResources(ctx)
		if err != nil {
			return errors.Wrap(err, "list resources")
		}
	}
	templates := known.templates
	if !known.haveTemplates {
		var err error
		templates, err = u.ListResourceTemplates(ctx)
		if err != nil {
			return errors.Wrap(err, "list resource templates")
		}
	}
	g.setRouteServer(u, g.newUpstreamRouteServer(u, tools, prompts, resources, templates))
	return nil
}

func (g *Gateway) setRouteServer(u *Upstream, server *mcp.Server) {
	g.routeMu.Lock()
	defer g.routeMu.Unlock()

	updated := false
	for i := range g.routes {
		if g.routes[i].upstream == u.cfg.Name {
			g.routes[i].server = server
			updated = true
			break
		}
	}
	if !updated {
		g.routes = append(g.routes, routedServer{
			upstream: u.cfg.Name,
			host:     u.cfg.Route.Host,
			path:     u.cfg.Route.Path,
			server:   server,
		})
	}

	rt := router.New[*mcp.Server]()
	for _, r := range g.routes {
		rt.Add(r.host, r.path, r.server)
	}
	g.router = rt
}

func (g *Gateway) newUpstreamRouteServer(u *Upstream, tools []*mcp.Tool, prompts []*mcp.Prompt, resources []*mcp.Resource, templates []*mcp.ResourceTemplate) *mcp.Server {
	s := mcputil.NewServer(mcputil.ServerConfig{
		Name:         u.cfg.Name,
		Instructions: g.cfg.Server.Instructions,
		Logger:       g.slogger.With("component", "route-server", "upstream", u.cfg.Name),
	})
	for _, rt := range tools {
		if !u.allowed(rt.Name) {
			continue
		}
		orig := rt.Name
		tool := &mcp.Tool{
			Name:         orig,
			Description:  TrimDescription(rt.Description, u.cfg.Tools.DescMax),
			InputSchema:  rt.InputSchema,
			OutputSchema: rt.OutputSchema,
			Annotations:  rt.Annotations,
			Title:        rt.Title,
		}
		h := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return u.CallTool(ctx, &mcp.CallToolParams{Meta: req.Params.Meta, Name: orig, Arguments: req.Params.Arguments})
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
		if u.redactor != nil {
			h = middleware.Redact(u.redactor.Redact)(h)
		}
		s.AddTool(tool, h)
	}
	for _, rp := range prompts {
		orig := rp.Name
		prompt := &mcp.Prompt{
			Name:        orig,
			Description: TrimDescription(rp.Description, u.cfg.Tools.DescMax),
			Arguments:   rp.Arguments,
			Title:       rp.Title,
			Meta:        rp.Meta,
		}
		s.AddPrompt(prompt, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return u.GetPrompt(ctx, &mcp.GetPromptParams{Meta: req.Params.Meta, Name: orig, Arguments: req.Params.Arguments})
		})
	}
	for _, rr := range resources {
		resource := &mcp.Resource{
			URI:         rr.URI,
			Name:        rr.Name,
			Description: TrimDescription(rr.Description, u.cfg.Tools.DescMax),
			MIMEType:    rr.MIMEType,
			Size:        rr.Size,
			Title:       rr.Title,
			Annotations: rr.Annotations,
			Meta:        rr.Meta,
		}
		s.AddResource(resource, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return u.ReadResource(ctx, &mcp.ReadResourceParams{URI: req.Params.URI})
		})
	}
	for _, rt := range templates {
		tpl := &mcp.ResourceTemplate{
			URITemplate: rt.URITemplate,
			Name:        rt.Name,
			Description: TrimDescription(rt.Description, u.cfg.Tools.DescMax),
			MIMEType:    rt.MIMEType,
			Title:       rt.Title,
			Annotations: rt.Annotations,
			Meta:        rt.Meta,
		}
		s.AddResourceTemplate(tpl, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return u.ReadResource(ctx, &mcp.ReadResourceParams{URI: req.Params.URI})
		})
	}
	return s
}

// registerUpstreamTools applies the same diff/apply algorithm as featureRegistry[T]
// but for tools, which additionally support prefix/allow/deny/desc-trim and
// per-upstream redactor middleware wiring. Any fix to the core sync logic
// must be mirrored here.
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
			collisions = append(collisions, Collision{Upstream: owner, Tool: rawNameByFinal[name], ResultName: name})
			continue
		}
		if !owned || changed {
			orig := rawNameByFinal[name]
			h := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return u.CallTool(ctx, &mcp.CallToolParams{Meta: req.Params.Meta, Name: orig, Arguments: req.Params.Arguments})
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
			if u.redactor != nil {
				h = middleware.Redact(u.redactor.Redact)(h)
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

func (g *Gateway) registerUpstreamPrompts(u *Upstream, rawPrompts []*mcp.Prompt) (added, removed []string, collisions []Collision) {
	g.registryMu.Lock()
	defer g.registryMu.Unlock()

	g.promptRegistry.ensureUpstream(u.cfg.Name)

	newSet := map[string]struct{}{}
	promptByFinal := map[string]*mcp.Prompt{}
	rawNameByFinal := map[string]string{}

	for _, rp := range rawPrompts {
		finalName := NamespaceName(u.cfg.Tools.Prefix, rp.Name)
		newSet[finalName] = struct{}{}
		promptByFinal[finalName] = &mcp.Prompt{
			Name:        finalName,
			Description: TrimDescription(rp.Description, u.cfg.Tools.DescMax),
			Arguments:   rp.Arguments,
			Title:       rp.Title,
			Meta:        rp.Meta,
		}
		rawNameByFinal[finalName] = rp.Name
	}

	toRemove, toAddOrChange, colls := g.promptRegistry.diff(u.cfg.Name, promptByFinal, rawNameByFinal)
	collisions = append(collisions, colls...)
	for _, name := range toRemove {
		g.server.RemovePrompts(name)
		removed = append(removed, name)
	}
	for _, name := range toAddOrChange {
		orig := rawNameByFinal[name]
		final := promptByFinal[name]
		h := func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return u.GetPrompt(ctx, &mcp.GetPromptParams{Meta: req.Params.Meta, Name: orig, Arguments: req.Params.Arguments})
		}
		g.server.AddPrompt(final, h)
		if _, owned := g.promptRegistry.finalToUpstream[name]; !owned {
			added = append(added, name)
		}
	}
	toAddOrChangeMap := make(map[string]*mcp.Prompt, len(toAddOrChange))
	for _, name := range toAddOrChange {
		toAddOrChangeMap[name] = promptByFinal[name]
	}
	g.promptRegistry.apply(u.cfg.Name, toRemove, toAddOrChangeMap, newSet)

	return added, removed, collisions
}

func (g *Gateway) registerUpstreamResources(u *Upstream, rawResources []*mcp.Resource) (added, removed []string, collisions []Collision) {
	g.registryMu.Lock()
	defer g.registryMu.Unlock()

	g.resourceRegistry.ensureUpstream(u.cfg.Name)

	newSet := map[string]struct{}{}
	resByFinal := map[string]*mcp.Resource{}

	for _, rr := range rawResources {
		uri := rr.URI
		newSet[uri] = struct{}{}
		resByFinal[uri] = &mcp.Resource{
			URI:         uri,
			Name:        rr.Name,
			Description: TrimDescription(rr.Description, u.cfg.Tools.DescMax),
			MIMEType:    rr.MIMEType,
			Size:        rr.Size,
			Title:       rr.Title,
			Annotations: rr.Annotations,
			Meta:        rr.Meta,
		}
	}

	toRemove, toAddOrChange, colls := g.resourceRegistry.diff(u.cfg.Name, resByFinal, nil)
	collisions = append(collisions, colls...)
	for _, uri := range toRemove {
		g.server.RemoveResources(uri)
		removed = append(removed, uri)
	}
	for _, uri := range toAddOrChange {
		final := resByFinal[uri]
		h := func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return u.ReadResource(ctx, &mcp.ReadResourceParams{URI: req.Params.URI})
		}
		g.server.AddResource(final, h)
		if _, owned := g.resourceRegistry.finalToUpstream[uri]; !owned {
			added = append(added, uri)
		}
	}
	toAddOrChangeMap := make(map[string]*mcp.Resource, len(toAddOrChange))
	for _, uri := range toAddOrChange {
		toAddOrChangeMap[uri] = resByFinal[uri]
	}
	g.resourceRegistry.apply(u.cfg.Name, toRemove, toAddOrChangeMap, newSet)

	return added, removed, collisions
}

func (g *Gateway) registerUpstreamResourceTemplates(u *Upstream, rawTemplates []*mcp.ResourceTemplate) (added, removed []string, collisions []Collision) {
	g.registryMu.Lock()
	defer g.registryMu.Unlock()

	g.resourceTemplateRegistry.ensureUpstream(u.cfg.Name)

	newSet := map[string]struct{}{}
	tplByFinal := map[string]*mcp.ResourceTemplate{}

	for _, rt := range rawTemplates {
		ut := rt.URITemplate
		newSet[ut] = struct{}{}
		tplByFinal[ut] = &mcp.ResourceTemplate{
			URITemplate: ut,
			Name:        rt.Name,
			Description: TrimDescription(rt.Description, u.cfg.Tools.DescMax),
			MIMEType:    rt.MIMEType,
			Title:       rt.Title,
			Annotations: rt.Annotations,
			Meta:        rt.Meta,
		}
	}

	toRemove, toAddOrChange, colls := g.resourceTemplateRegistry.diff(u.cfg.Name, tplByFinal, nil)
	collisions = append(collisions, colls...)
	for _, ut := range toRemove {
		g.server.RemoveResourceTemplates(ut)
		removed = append(removed, ut)
	}
	for _, ut := range toAddOrChange {
		final := tplByFinal[ut]
		h := func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return u.ReadResource(ctx, &mcp.ReadResourceParams{URI: req.Params.URI})
		}
		g.server.AddResourceTemplate(final, h)
		if _, owned := g.resourceTemplateRegistry.finalToUpstream[ut]; !owned {
			added = append(added, ut)
		}
	}
	toAddOrChangeMap := make(map[string]*mcp.ResourceTemplate, len(toAddOrChange))
	for _, ut := range toAddOrChange {
		toAddOrChangeMap[ut] = tplByFinal[ut]
	}
	g.resourceTemplateRegistry.apply(u.cfg.Name, toRemove, toAddOrChangeMap, newSet)

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

// RegisteredPrompts returns a snapshot of finalName -> upstreamName for tests.
func (g *Gateway) RegisteredPrompts() map[string]string {
	g.registryMu.RLock()
	defer g.registryMu.RUnlock()
	out := make(map[string]string, len(g.promptRegistry.finalToUpstream))
	maps.Copy(out, g.promptRegistry.finalToUpstream)
	return out
}

// RegisteredResources returns a snapshot of finalName -> upstreamName for tests.
func (g *Gateway) RegisteredResources() map[string]string {
	g.registryMu.RLock()
	defer g.registryMu.RUnlock()
	out := make(map[string]string, len(g.resourceRegistry.finalToUpstream))
	maps.Copy(out, g.resourceRegistry.finalToUpstream)
	return out
}

// RegisteredResourceTemplates returns a snapshot of finalName -> upstreamName for tests.
func (g *Gateway) RegisteredResourceTemplates() map[string]string {
	g.registryMu.RLock()
	defer g.registryMu.RUnlock()
	out := make(map[string]string, len(g.resourceTemplateRegistry.finalToUpstream))
	maps.Copy(out, g.resourceTemplateRegistry.finalToUpstream)
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
	clear(g.promptRegistry.finalToUpstream)
	clear(g.promptRegistry.upstreamRegistered)
	clear(g.promptRegistry.registered)
	clear(g.resourceRegistry.finalToUpstream)
	clear(g.resourceRegistry.upstreamRegistered)
	clear(g.resourceRegistry.registered)
	clear(g.resourceTemplateRegistry.finalToUpstream)
	clear(g.resourceTemplateRegistry.upstreamRegistered)
	clear(g.resourceTemplateRegistry.registered)
	g.registryMu.Unlock()
	return nil
}
