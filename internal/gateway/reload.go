package gateway

import (
	"context"
	"reflect"
	"slices"
	"sync"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/go-faster/gooners/internal/gateway/router"
)

// ReloadResult reports what a [Gateway.Reload] did, by upstream name.
type ReloadResult struct {
	Added     []string
	Removed   []string
	Restarted []string
	Unchanged []string
	// RestartRequired lists config sections that differ from the running
	// configuration but only take effect after a process restart.
	RestartRequired []string
}

// Reload applies cfg to a running gateway without dropping the downstream MCP
// server: clients keep their sessions and learn about the new tool set through
// the listChanged notifications the registry already emits.
//
// An invalid configuration is rejected before anything is mutated, so the
// previously loaded one keeps serving. Upstreams whose effective configuration
// did not change keep their live session; the rest are closed and reconnected.
// Sections that cannot change in place are reported in
// [ReloadResult.RestartRequired] rather than silently applied.
func (g *Gateway) Reload(ctx context.Context, cfg *Config) (ReloadResult, error) {
	g.reloadMu.Lock()
	defer g.reloadMu.Unlock()

	var res ReloadResult
	if cfg == nil {
		return res, errors.New("nil config")
	}
	cfg.setDefaults()
	if err := cfg.Validate(); err != nil {
		return res, errors.Wrap(err, "validate")
	}

	old := g.config()
	resolver, err := NewSecretResolver(cfg.Secrets, g.slogger.With("component", "secretresolver"))
	if err != nil {
		return res, errors.Wrap(err, "secret resolver")
	}
	var redactor *Redactor
	if cfg.Redact.Enabled {
		if redactor, err = NewRedactor(cfg.Redact.Patterns, cfg.Redact.MinEntropy); err != nil {
			return res, errors.Wrap(err, "create redactor")
		}
	}
	res.RestartRequired = restartRequired(old, cfg)

	plan := planUpstreams(old, cfg)
	res.Unchanged = plan.unchanged

	// Construct every new upstream before touching live state, so a
	// construction failure leaves the running gateway exactly as it was.
	fresh := make([]*Upstream, 0, len(plan.add)+len(plan.restart))
	for _, uc := range slices.Concat(plan.add, plan.restart) {
		u, err := g.newUpstream(uc, cfg, resolver, redactor)
		if err != nil {
			return res, errors.Wrapf(err, "upstream %q", uc.Name)
		}
		fresh = append(fresh, u)
	}

	// Mounts are a table the tool reads per call, so swapping them needs no new
	// listener and no reconnect. The store itself was built at startup.
	mounts := newBlobMounts(cfg.Blob.Mounts)

	g.stateMu.Lock()
	g.cfg = cfg
	g.resolver = resolver
	oldMounts := g.blobMounts
	g.blobMounts = mounts
	g.stateMu.Unlock()

	// The tool's description names the served directories, so a changed mount
	// list has to be re-advertised; re-registering emits listChanged and the
	// client re-reads it.
	if g.blobStore != nil && !slices.EqualFunc(oldMounts, mounts, func(a, b blobMount) bool {
		return a.name == b.name && a.prefix == b.prefix
	}) {
		g.registerBlobTool(mounts)
	}

	// Detach before attaching: a renamed prefix or a moved route must free its
	// names before the replacement claims them, otherwise the replacement is
	// reported as a collision against its own predecessor. Deregistration is
	// synchronous — it frees those names — while the closes that follow are
	// concurrent, so a reload removing several upstreams waits out one drain
	// rather than their sum.
	detached := make([]*Upstream, 0, len(plan.remove)+len(plan.restart))
	for _, uc := range slices.Concat(plan.remove, plan.restart) {
		u := g.upstreamByName(uc.Name)
		if u == nil {
			continue
		}
		g.detachUpstream(u)
		detached = append(detached, u)
	}
	closeUpstreams(ctx, detached)

	for _, u := range fresh {
		g.attachUpstream(ctx, u)
	}

	for _, uc := range plan.add {
		res.Added = append(res.Added, uc.Name)
	}
	for _, uc := range plan.remove {
		res.Removed = append(res.Removed, uc.Name)
	}
	for _, uc := range plan.restart {
		res.Restarted = append(res.Restarted, uc.Name)
	}
	return res, nil
}

// UpstreamStatus implements [Reloadable], counting upstreams by whether they
// currently hold a session. A disconnected upstream is still configured: its
// supervisor is retrying, so this is the signal that separates "not configured"
// from "configured but unreachable".
func (g *Gateway) UpstreamStatus() UpstreamStatus {
	var st UpstreamStatus
	for _, u := range g.upstreamList() {
		if u.currentSession() != nil {
			st.Connected++
		} else {
			st.Disconnected++
		}
	}
	return st
}

type upstreamPlan struct {
	add       []UpstreamConfig
	restart   []UpstreamConfig
	remove    []UpstreamConfig
	unchanged []string
}

// planUpstreams diffs the upstream sets of two configurations. An upstream is
// restarted when its own section changed, when a secret it interpolates changed,
// or when the global [RedactConfig] it inherits changed: all three are baked
// into the live session or its handler chain at connect time.
func planUpstreams(oldCfg, newCfg *Config) upstreamPlan {
	var plan upstreamPlan
	oldByName := make(map[string]UpstreamConfig, len(oldCfg.Upstreams))
	for _, uc := range oldCfg.Upstreams {
		oldByName[uc.Name] = uc
	}
	oldSecrets := secretsByName(oldCfg.Secrets)
	newSecrets := secretsByName(newCfg.Secrets)
	globalRedactChanged := !reflect.DeepEqual(oldCfg.Redact, newCfg.Redact)

	seen := make(map[string]struct{}, len(newCfg.Upstreams))
	for _, uc := range newCfg.Upstreams {
		seen[uc.Name] = struct{}{}
		prev, ok := oldByName[uc.Name]
		switch {
		case !ok:
			plan.add = append(plan.add, uc)
		case !reflect.DeepEqual(prev, uc),
			uc.Redact == nil && globalRedactChanged,
			secretRefsChanged(uc, oldSecrets, newSecrets):
			plan.restart = append(plan.restart, uc)
		default:
			plan.unchanged = append(plan.unchanged, uc.Name)
		}
	}
	for _, uc := range oldCfg.Upstreams {
		if _, ok := seen[uc.Name]; !ok {
			plan.remove = append(plan.remove, uc)
		}
	}
	return plan
}

func secretsByName(secrets []SecretConfig) map[string]SecretConfig {
	out := make(map[string]SecretConfig, len(secrets))
	for _, s := range secrets {
		out[s.Name] = s
	}
	return out
}

func secretRefsChanged(uc UpstreamConfig, oldSecrets, newSecrets map[string]SecretConfig) bool {
	for _, values := range []map[string]string{uc.Env, uc.Headers} {
		for _, v := range values {
			for name := range extractSecretRefs(v) {
				if oldSecrets[name] != newSecrets[name] {
					return true
				}
			}
		}
	}
	return false
}

// restartRequired names the sections that a live reload cannot apply. The
// gateway's own MCP server identity and its HTTP middleware chain are handed to
// the transport once at startup; the discovery tools and the lazy middleware are
// installed on the server at construction.
func restartRequired(oldCfg, newCfg *Config) []string {
	var out []string
	if oldCfg.Server != newCfg.Server {
		out = append(out, "server")
	}
	if !reflect.DeepEqual(oldCfg.Auth, newCfg.Auth) {
		out = append(out, "auth")
	}
	if oldCfg.anyLazy() != newCfg.anyLazy() {
		out = append(out, "tools.lazy")
	}
	// The blob store is built once, from a listener and a base_url or from a
	// bucket it authenticated against at startup. Its mount list is not here:
	// that reloads.
	if blobStoreChanged(oldCfg.Blob, newCfg.Blob) {
		out = append(out, "blob")
	}
	return out
}

// blobStoreChanged reports whether the blob section changed in a way that
// needs a new store, ignoring the mounts a live gateway can swap.
func blobStoreChanged(oldCfg, newCfg BlobConfig) bool {
	oldCfg.Mounts, newCfg.Mounts = nil, nil
	return !reflect.DeepEqual(oldCfg, newCfg)
}

// detachUpstream removes everything an upstream contributed, leaving it
// connected: the caller closes it, which is what lets several be drained at
// once. Syncing it against an empty feature set reuses the same diff/apply path
// a listChanged notification takes, so downstream clients see one ordinary
// removal rather than a special case.
func (g *Gateway) detachUpstream(u *Upstream) {
	name := u.cfg.Name
	g.registerUpstreamTools(u, nil)
	g.registerUpstreamPrompts(u, nil)
	g.registerUpstreamResources(u, nil)
	g.registerUpstreamResourceTemplates(u, nil)

	g.registryMu.Lock()
	delete(g.registry.upstreamRegistered, name)
	delete(g.promptRegistry.upstreamRegistered, name)
	delete(g.resourceRegistry.upstreamRegistered, name)
	delete(g.resourceTemplateRegistry.upstreamRegistered, name)
	g.registryMu.Unlock()

	g.removeRoute(name)

	g.stateMu.Lock()
	g.upstreams = slices.DeleteFunc(g.upstreams, func(x *Upstream) bool { return x == u })
	g.stateMu.Unlock()
}

// closeUpstreams closes every upstream concurrently, so a caller waits out one
// drain timeout rather than the sum of them.
func closeUpstreams(ctx context.Context, ups []*Upstream) {
	var wg sync.WaitGroup
	for _, u := range ups {
		wg.Go(func() { _ = u.Close(ctx) })
	}
	wg.Wait()
}

// attachUpstream registers an upstream and connects it. An upstream that is
// unreachable right now is still attached: its supervisor retries in the
// background, exactly as it does for one that was down at startup.
func (g *Gateway) attachUpstream(ctx context.Context, u *Upstream) {
	g.stateMu.Lock()
	g.upstreams = append(g.upstreams, u)
	g.stateMu.Unlock()

	if err := u.Connect(ctx); err != nil {
		g.logger.Warn("upstream unavailable after reload; will retry in background",
			zap.String("upstream", u.cfg.Name), zap.Error(err))
		return
	}
	if err := g.syncUpstream(ctx, u); err != nil {
		g.logger.Warn("upstream sync failed after reload",
			zap.String("upstream", u.cfg.Name), zap.Error(err))
	}
}

// syncUpstream lists an upstream and registers everything it exports.
func (g *Gateway) syncUpstream(ctx context.Context, u *Upstream) error {
	tools, err := u.ListTools(ctx)
	if err != nil {
		return errors.Wrap(err, "list tools")
	}
	prompts, err := u.ListPrompts(ctx)
	if err != nil {
		return errors.Wrap(err, "list prompts")
	}
	resources, err := u.ListResources(ctx)
	if err != nil {
		return errors.Wrap(err, "list resources")
	}
	templates, err := u.ListResourceTemplates(ctx)
	if err != nil {
		return errors.Wrap(err, "list resource templates")
	}

	addedT, _, collT := g.registerUpstreamTools(u, tools)
	addedP, _, collP := g.registerUpstreamPrompts(u, prompts)
	addedR, _, collR := g.registerUpstreamResources(u, resources)
	addedTpl, _, collTpl := g.registerUpstreamResourceTemplates(u, templates)
	collisions := len(collT) + len(collP) + len(collR) + len(collTpl)
	if collisions > 0 {
		g.logger.Warn("collisions while registering upstream",
			zap.String("upstream", u.cfg.Name), zap.Int("collisions", collisions))
	}
	g.logger.Info("upstream registered",
		zap.String("upstream", u.cfg.Name),
		zap.Int("tools", len(addedT)),
		zap.Int("prompts", len(addedP)),
		zap.Int("resources", len(addedR)+len(addedTpl)),
		zap.Int("collisions", collisions),
	)

	if u.hasRoute() {
		g.setRouteServer(u, g.newUpstreamRouteServer(u, tools, prompts, resources, templates))
	}
	return nil
}

// removeRoute drops an upstream's routed server and rebuilds the router.
func (g *Gateway) removeRoute(upstream string) {
	g.routeMu.Lock()
	defer g.routeMu.Unlock()

	before := len(g.routes)
	g.routes = slices.DeleteFunc(g.routes, func(r routedServer) bool { return r.upstream == upstream })
	if len(g.routes) == before {
		return
	}
	rt := router.New[*mcp.Server]()
	for _, r := range g.routes {
		rt.Add(r.host, r.path, r.server)
	}
	g.router = rt
}
