// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"context"
	"log/slog"
	"path"
	"sync"
	"time"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	gwtransport "github.com/go-faster/gooners/internal/gateway/transport"
)

// Upstream represents one configured upstream MCP server (not yet connected).
type Upstream struct {
	cfg              UpstreamConfig
	client           *mcp.Client
	session          *mcp.ClientSession
	resolver         SecretResolver
	logger           *slog.Logger
	transport        mcp.Transport
	cleanup          func() error
	connectTimeout   time.Duration
	keepAlive        time.Duration
	reconnectInitial time.Duration
	reconnectMax     time.Duration
	onReconnect      func(context.Context, string) error

	mu               sync.RWMutex
	supervisorCancel context.CancelFunc
	supervisorDone   chan struct{}
	closed           bool
}

// UpstreamOptions configures optional dependencies for an Upstream.
type UpstreamOptions struct {
	Logger                *slog.Logger
	Resolver              SecretResolver
	OnToolListChanged     func(ctx context.Context, upstreamName string) error
	OnPromptListChanged   func(ctx context.Context, upstreamName string) error
	OnResourceListChanged func(ctx context.Context, upstreamName string) error
	OnResourceUpdated     func(ctx context.Context, upstreamName, uri string) error
	OnReconnect           func(ctx context.Context, upstreamName string) error
	ConnectTimeout        time.Duration
	// KeepAlive configures SDK ping interval. Negative disables keepalive.
	KeepAlive        time.Duration
	ReconnectInitial time.Duration
	ReconnectMax     time.Duration
}

func (o *UpstreamOptions) setDefaults() {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.ConnectTimeout == 0 {
		o.ConnectTimeout = 10 * time.Second
	}
	if o.KeepAlive == 0 {
		o.KeepAlive = 30 * time.Second
	}
	if o.ReconnectInitial == 0 {
		o.ReconnectInitial = time.Second
	}
	if o.ReconnectMax == 0 {
		o.ReconnectMax = 30 * time.Second
	}
}

// NewUpstream creates an Upstream from config. It does not connect.
func NewUpstream(cfg UpstreamConfig, opts UpstreamOptions) (*Upstream, error) {
	opts.setDefaults()
	keepAlive := max(opts.KeepAlive, 0)
	u := &Upstream{
		cfg:              cfg,
		resolver:         opts.Resolver,
		logger:           opts.Logger,
		connectTimeout:   opts.ConnectTimeout,
		keepAlive:        keepAlive,
		reconnectInitial: opts.ReconnectInitial,
		reconnectMax:     opts.ReconnectMax,
		onReconnect:      opts.OnReconnect,
	}
	impl := &mcp.Implementation{Name: "mcpgateway-client", Version: "0"}
	u.client = mcp.NewClient(impl, &mcp.ClientOptions{
		Logger:    opts.Logger,
		KeepAlive: keepAlive,
		ToolListChangedHandler: func(_ context.Context, _ *mcp.ToolListChangedRequest) {
			if opts.OnToolListChanged != nil {
				_ = opts.OnToolListChanged(context.Background(), cfg.Name)
			}
		},
		PromptListChangedHandler: func(_ context.Context, _ *mcp.PromptListChangedRequest) {
			if opts.OnPromptListChanged != nil {
				_ = opts.OnPromptListChanged(context.Background(), cfg.Name)
			}
		},
		ResourceListChangedHandler: func(_ context.Context, _ *mcp.ResourceListChangedRequest) {
			if opts.OnResourceListChanged != nil {
				_ = opts.OnResourceListChanged(context.Background(), cfg.Name)
			}
		},
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			if opts.OnResourceUpdated != nil && req != nil && req.Params != nil {
				_ = opts.OnResourceUpdated(context.Background(), cfg.Name, req.Params.URI)
			}
		},
	})
	return u, nil
}

// Connect establishes the session using the injected BuildTransport.
// If the session is already set (e.g. by test helper), this is a no-op.
func (u *Upstream) Connect(ctx context.Context) error {
	u.mu.RLock()
	connected := u.session != nil
	u.mu.RUnlock()
	if connected {
		u.startSupervisor(ctx, u.onReconnect)
		return nil
	}
	if err := u.connectOnce(ctx); err != nil {
		return err
	}
	u.startSupervisor(ctx, u.onReconnect)
	return nil
}

func (u *Upstream) connectOnce(ctx context.Context) error {
	u.mu.RLock()
	closed := u.closed
	connected := u.session != nil
	u.mu.RUnlock()
	if closed {
		return errors.New("upstream closed")
	}
	if connected {
		return nil
	}
	tr, cl, err := BuildTransport(ctx, u.cfg, u.resolver)
	if err != nil {
		return errors.Wrap(err, "build transport")
	}
	timeout := u.connectTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sess, err := u.client.Connect(cctx, tr, nil)
	if err != nil {
		if cl != nil {
			_ = cl()
		}
		return errors.Wrap(err, "connect")
	}
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		_ = sess.Close()
		if cl != nil {
			_ = cl()
		}
		return errors.New("upstream closed")
	}
	u.transport = tr
	u.cleanup = cl
	u.session = sess
	u.mu.Unlock()
	return nil
}

func (u *Upstream) startSupervisor(ctx context.Context, onReconnect func(context.Context, string) error) {
	u.mu.Lock()
	if u.closed || u.supervisorDone != nil || u.reconnectInitial <= 0 {
		u.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	u.supervisorCancel = cancel
	u.supervisorDone = done
	u.mu.Unlock()

	go u.supervise(ctx, done, onReconnect)
}

func (u *Upstream) supervise(ctx context.Context, done chan struct{}, onReconnect func(context.Context, string) error) {
	defer close(done)
	backoff := u.reconnectInitial
	attempt := 0
	for {
		sess := u.currentSession()
		if sess == nil {
			return
		}
		waitErr := make(chan error, 1)
		go func() {
			waitErr <- sess.Wait()
		}()
		var err error
		select {
		case <-ctx.Done():
			_ = sess.Close()
			<-waitErr
			return
		case err = <-waitErr:
		}
		u.logger.Info("upstream session dropped", "error", err)
		u.closeSessionResources()

		for {
			if !waitBackoff(ctx, backoff) {
				return
			}
			attempt++
			if err := u.connectOnce(ctx); err != nil {
				u.logger.Warn("upstream reconnect failed", "attempt", attempt, "backoff", backoff, "error", err)
				backoff = nextBackoff(backoff, u.reconnectInitial, u.reconnectMax)
				continue
			}
			u.logger.Info("upstream reconnected", "attempt", attempt)
			backoff = u.reconnectInitial
			attempt = 0
			if onReconnect != nil {
				if err := onReconnect(ctx, u.cfg.Name); err != nil {
					u.logger.Warn("upstream reconnect re-sync failed", "error", err)
				}
			}
			break
		}
	}
}

func waitBackoff(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(current, initial, maxBackoff time.Duration) time.Duration {
	if current <= 0 {
		current = initial
	}
	next := current * 2
	if next < current || next > maxBackoff {
		return maxBackoff
	}
	return next
}

func (u *Upstream) currentSession() *mcp.ClientSession {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.session
}

func (u *Upstream) closeSessionResources() {
	u.mu.Lock()
	sess := u.session
	cleanup := u.cleanup
	u.session = nil
	u.transport = nil
	u.cleanup = nil
	u.mu.Unlock()
	if sess != nil {
		_ = sess.Close()
	}
	if cleanup != nil {
		_ = cleanup()
	}
}

// init wires the default BuildTransport using the transport package.
func init() {
	BuildTransport = func(ctx context.Context, cfg UpstreamConfig, r SecretResolver) (mcp.Transport, func() error, error) {
		interp := func(s string) (string, error) {
			return Interpolate(s, r)
		}
		return gwtransport.Build(ctx, cfg.Kind, cfg.Command, cfg.URL, cfg.Env, cfg.Headers, interp)
	}
}

// BuildTransport is overridable for tests (set by test hooks or main).
var BuildTransport = func(_ context.Context, _ UpstreamConfig, _ SecretResolver) (mcp.Transport, func() error, error) {
	return nil, nil, errors.New("transport builder not wired (wire in main or test)")
}

// newUpstreamWithTransport is a test helper that injects a ready transport.
func newUpstreamWithTransport(cfg UpstreamConfig, tr mcp.Transport, cl func() error) *Upstream {
	return &Upstream{cfg: cfg, transport: tr, cleanup: cl, logger: slog.Default(), reconnectInitial: time.Second, reconnectMax: 30 * time.Second}
}

// newUpstreamWithInMemoryClient constructs Upstream with mcp.Client wired to call handlerOnListChanged on tool list changed.
// Does not call Connect; caller must.
func newUpstreamWithInMemoryClient(cfg UpstreamConfig, clientTr mcp.Transport, handlerOnListChanged func(context.Context, string) error) *Upstream {
	return newUpstreamWithInMemoryClientWithCallbacks(cfg, clientTr, upstreamCallbacks{OnToolListChanged: handlerOnListChanged})
}

type upstreamCallbacks struct {
	OnToolListChanged     func(context.Context, string) error
	OnPromptListChanged   func(context.Context, string) error
	OnResourceListChanged func(context.Context, string) error
	OnResourceUpdated     func(context.Context, string, string) error
	OnReconnect           func(context.Context, string) error
}

// newUpstreamWithInMemoryClientWithCallbacks constructs Upstream with mcp.Client wired to call the provided callbacks.
func newUpstreamWithInMemoryClientWithCallbacks(cfg UpstreamConfig, clientTr mcp.Transport, cb upstreamCallbacks) *Upstream {
	u := &Upstream{cfg: cfg, logger: slog.Default(), reconnectInitial: time.Second, reconnectMax: 30 * time.Second, onReconnect: cb.OnReconnect}
	impl := &mcp.Implementation{Name: "mcpgateway-client", Version: "0"}
	u.client = mcp.NewClient(impl, &mcp.ClientOptions{
		Logger: slog.Default(),
		ToolListChangedHandler: func(_ context.Context, _ *mcp.ToolListChangedRequest) {
			if cb.OnToolListChanged != nil {
				_ = cb.OnToolListChanged(context.Background(), cfg.Name)
			}
		},
		PromptListChangedHandler: func(_ context.Context, _ *mcp.PromptListChangedRequest) {
			if cb.OnPromptListChanged != nil {
				_ = cb.OnPromptListChanged(context.Background(), cfg.Name)
			}
		},
		ResourceListChangedHandler: func(_ context.Context, _ *mcp.ResourceListChangedRequest) {
			if cb.OnResourceListChanged != nil {
				_ = cb.OnResourceListChanged(context.Background(), cfg.Name)
			}
		},
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			if cb.OnResourceUpdated != nil && req != nil && req.Params != nil {
				_ = cb.OnResourceUpdated(context.Background(), cfg.Name, req.Params.URI)
			}
		},
	})
	u.transport = clientTr
	return u
}

// Name returns the upstream name from config.
func (u *Upstream) Name() string { return u.cfg.Name }

// Close shuts the session and runs cleanup.
func (u *Upstream) Close(_ context.Context) error {
	u.mu.Lock()
	if u.closed {
		done := u.supervisorDone
		u.mu.Unlock()
		if done != nil {
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			select {
			case <-done:
			case <-timer.C:
				u.logger.Warn("timed out waiting for upstream supervisor")
			}
		}
		return nil
	}
	u.closed = true
	cancel := u.supervisorCancel
	done := u.supervisorDone
	u.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	u.closeSessionResources()
	if done != nil {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			u.logger.Warn("timed out waiting for upstream supervisor")
		}
	}
	return nil
}

// ListTools calls the upstream.
func (u *Upstream) ListTools(ctx context.Context) ([]*mcp.Tool, error) {
	sess := u.currentSession()
	if sess == nil {
		return nil, errors.New("not connected")
	}
	res, err := sess.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// CallTool forwards the call to the upstream session.
func (u *Upstream) CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	sess := u.currentSession()
	if sess == nil {
		return nil, errors.New("not connected")
	}
	return sess.CallTool(ctx, params)
}

// ListPrompts calls the upstream.
func (u *Upstream) ListPrompts(ctx context.Context) ([]*mcp.Prompt, error) {
	sess := u.currentSession()
	if sess == nil {
		return nil, errors.New("not connected")
	}
	res, err := sess.ListPrompts(ctx, &mcp.ListPromptsParams{})
	if err != nil {
		return nil, err
	}
	return res.Prompts, nil
}

// GetPrompt forwards the call to the upstream session.
func (u *Upstream) GetPrompt(ctx context.Context, params *mcp.GetPromptParams) (*mcp.GetPromptResult, error) {
	sess := u.currentSession()
	if sess == nil {
		return nil, errors.New("not connected")
	}
	return sess.GetPrompt(ctx, params)
}

// ListResources calls the upstream.
func (u *Upstream) ListResources(ctx context.Context) ([]*mcp.Resource, error) {
	sess := u.currentSession()
	if sess == nil {
		return nil, errors.New("not connected")
	}
	res, err := sess.ListResources(ctx, &mcp.ListResourcesParams{})
	if err != nil {
		return nil, err
	}
	return res.Resources, nil
}

// ListResourceTemplates calls the upstream.
func (u *Upstream) ListResourceTemplates(ctx context.Context) ([]*mcp.ResourceTemplate, error) {
	sess := u.currentSession()
	if sess == nil {
		return nil, errors.New("not connected")
	}
	res, err := sess.ListResourceTemplates(ctx, &mcp.ListResourceTemplatesParams{})
	if err != nil {
		return nil, err
	}
	return res.ResourceTemplates, nil
}

// ReadResource forwards the call to the upstream session.
func (u *Upstream) ReadResource(ctx context.Context, params *mcp.ReadResourceParams) (*mcp.ReadResourceResult, error) {
	sess := u.currentSession()
	if sess == nil {
		return nil, errors.New("not connected")
	}
	return sess.ReadResource(ctx, params)
}

// BuildTools applies prefix, allow/deny globs (via path.Match), and desc trim.
func (u *Upstream) BuildTools(tools []*mcp.Tool) []*mcp.Tool {
	out := make([]*mcp.Tool, 0, len(tools))
	for _, t := range tools {
		name := t.Name
		if !u.allowed(name) {
			continue
		}
		nt := &mcp.Tool{
			Name:         NamespaceName(u.cfg.Tools.Prefix, name),
			Description:  TrimDescription(t.Description, u.cfg.Tools.DescMax),
			InputSchema:  t.InputSchema,
			OutputSchema: t.OutputSchema,
			Annotations:  t.Annotations,
			Title:        t.Title,
		}
		out = append(out, nt)
	}
	return out
}

func (u *Upstream) allowed(name string) bool {
	allow := u.cfg.Tools.Allow
	deny := u.cfg.Tools.Deny
	if len(allow) > 0 {
		matched := false
		for _, p := range allow {
			if ok, _ := path.Match(p, name); ok {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, p := range deny {
		if ok, _ := path.Match(p, name); ok {
			return false
		}
	}
	return true
}

// TrimDescription is the pure helper used by BuildTools.
func TrimDescription(s string, maxLen int) string {
	if maxLen > 0 && len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}
