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

	promptRegistry           featureRegistry[*mcp.Prompt]
	resourceRegistry         featureRegistry[*mcp.Resource]
	resourceTemplateRegistry featureRegistry[*mcp.ResourceTemplate]

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

type featureRegistry[T any] struct {
	finalToUpstream    map[string]string
	upstreamRegistered map[string]map[string]struct{}
	registered         map[string]T
	equal              func(a, b T) bool
}

func newFeatureRegistry[T any](equal func(a, b T) bool) featureRegistry[T] {
	return featureRegistry[T]{
		finalToUpstream:    make(map[string]string),
		upstreamRegistered: make(map[string]map[string]struct{}),
		registered:         make(map[string]T),
		equal:              equal,
	}
}

func (r *featureRegistry[T]) ensureUpstream(up string) {
	if r.upstreamRegistered[up] == nil {
		r.upstreamRegistered[up] = make(map[string]struct{})
	}
}

func (r *featureRegistry[T]) diff(upstream string, newPayloads map[string]T, _ map[string]string) (toRemove, toAddOrChange []string, collisions []Collision) {
	r.ensureUpstream(upstream)
	prev := r.upstreamRegistered[upstream]
	for name := range prev {
		if _, still := newPayloads[name]; !still {
			toRemove = append(toRemove, name)
		}
	}
	for name, newP := range newPayloads {
		owner, owned := r.finalToUpstream[name]
		if owned && owner != upstream {
			collisions = append(collisions, Collision{Upstream: owner, Tool: "", ResultName: name})
			continue
		}
		if owned && owner == upstream {
			if !r.equal(r.registered[name], newP) {
				toAddOrChange = append(toAddOrChange, name)
			}
		} else {
			toAddOrChange = append(toAddOrChange, name)
		}
	}
	return
}

func (r *featureRegistry[T]) apply(upstream string, toRemove []string, toAddOrChangePayloads map[string]T, finalNewSet map[string]struct{}) {
	r.ensureUpstream(upstream)
	for _, name := range toRemove {
		delete(r.finalToUpstream, name)
		delete(r.registered, name)
		delete(r.upstreamRegistered[upstream], name)
	}
	for name, p := range toAddOrChangePayloads {
		r.finalToUpstream[name] = upstream
		r.registered[name] = p
	}
	r.upstreamRegistered[upstream] = map[string]struct{}{}
	for n := range finalNewSet {
		if owner, ok := r.finalToUpstream[n]; ok && owner == upstream {
			r.upstreamRegistered[upstream][n] = struct{}{}
		}
	}
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
		promptRegistry:           newFeatureRegistry[*mcp.Prompt](promptEqual),
		resourceRegistry:         newFeatureRegistry[*mcp.Resource](resourceEqual),
		resourceTemplateRegistry: newFeatureRegistry[*mcp.ResourceTemplate](resourceTemplateEqual),
		server:                   &mcp.Server{},
		upstreams:                []*Upstream{},
		registryMu:               sync.RWMutex{},
		mp:                       opts.MeterProvider,
		tp:                       opts.TracerProvider,
		logger:                   opts.Logger,
		slogger:                  opts.Slogger,
	}
	for _, uc := range cfg.Upstreams {
		if g.registry.upstreamRegistered[uc.Name] == nil {
			g.registry.upstreamRegistered[uc.Name] = make(map[string]struct{})
		}
	}
	for _, uc := range cfg.Upstreams {
		u, err := NewUpstream(uc, UpstreamOptions{
			Logger:                opts.Slogger.With("upstream", uc.Name),
			Resolver:              res,
			OnToolListChanged:     g.onToolListChanged,
			OnPromptListChanged:   g.onPromptListChanged,
			OnResourceListChanged: g.onResourceListChanged,
			OnResourceUpdated:     g.onResourceUpdated,
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
		g.logger.Warn("re-sync collisions", zap.String("upstream", upstreamName), zap.Int("collisions", len(collisions)))
	}
	g.logger.Info("prompts re-synced",
		zap.String("upstream", upstreamName),
		zap.Int("added", len(added)),
		zap.Int("removed", len(removed)),
		zap.Int("collisions", len(collisions)),
	)
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
		g.logger.Warn("re-sync collisions", zap.String("upstream", upstreamName), zap.Int("collisions", len(collisions)))
	}
	g.logger.Info("resources re-synced",
		zap.String("upstream", upstreamName),
		zap.Int("added", len(addedR)+len(addedT)),
		zap.Int("removed", len(removedR)+len(removedT)),
		zap.Int("collisions", len(collisions)),
	)
	return nil
}

func (g *Gateway) onResourceUpdated(ctx context.Context, upstreamName, uri string) error {
	g.logger.Info("resource updated", zap.String("upstream", upstreamName), zap.String("uri", uri))
	return g.server.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: uri})
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
			_ = g.Close(ctx)
			return errors.Wrapf(err, "connect upstream %s", u.cfg.Name)
		}
		tools, err := u.ListTools(ctx)
		if err != nil {
			_ = g.Close(ctx)
			return errors.Wrapf(err, "list tools %s", u.cfg.Name)
		}
		prompts, err := u.ListPrompts(ctx)
		if err != nil {
			_ = g.Close(ctx)
			return errors.Wrapf(err, "list prompts %s", u.cfg.Name)
		}
		resources, err := u.ListResources(ctx)
		if err != nil {
			_ = g.Close(ctx)
			return errors.Wrapf(err, "list resources %s", u.cfg.Name)
		}
		templates, err := u.ListResourceTemplates(ctx)
		if err != nil {
			_ = g.Close(ctx)
			return errors.Wrapf(err, "list resource templates %s", u.cfg.Name)
		}
		listedItems = append(listedItems, listed{u: u, tools: tools, prompts: prompts, resources: resources, templates: templates})
	}

	prefixes := map[string]string{}
	toolSets := map[string][]string{}
	promptSets := map[string][]string{}
	resourceSets := map[string][]string{}
	templateSets := map[string][]string{}
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
	if err := DetectCollisions(map[string]string{}, promptSets); err != nil {
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
			return u.GetPrompt(ctx, &mcp.GetPromptParams{Name: orig, Arguments: req.Params.Arguments})
		}
		g.server.AddPrompt(final, h)
		if _, owned := g.promptRegistry.finalToUpstream[name]; !owned {
			added = append(added, name)
		}
	}
	g.promptRegistry.apply(u.cfg.Name, toRemove, promptByFinal, newSet)

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
	g.resourceRegistry.apply(u.cfg.Name, toRemove, resByFinal, newSet)

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
	g.resourceTemplateRegistry.apply(u.cfg.Name, toRemove, tplByFinal, newSet)

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
