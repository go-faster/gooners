// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"context"
	"iter"
	"log/slog"
	"path"
	"sync"
	"time"
	"unicode/utf8"

	backoff "github.com/cenkalti/backoff/v5"
	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	gwtransport "github.com/go-faster/gooners/internal/gateway/transport"
)

// TransportBuilder constructs an mcp.Transport for an upstream and returns an
// optional cleanup function to call after the session closes.
type TransportBuilder func(ctx context.Context, cfg UpstreamConfig, r SecretResolver) (mcp.Transport, func() error, error)

// defaultTransportBuilder is the production transport builder used when none is
// provided via UpstreamOptions.
func defaultTransportBuilder(ctx context.Context, cfg UpstreamConfig, r SecretResolver) (mcp.Transport, func() error, error) {
	interp := func(s string) (string, error) {
		return Interpolate(ctx, s, r)
	}
	return gwtransport.Build(ctx, cfg.Kind, cfg.Command, cfg.URL, cfg.Env, cfg.Headers, interp)
}

// Upstream represents one configured upstream MCP server (not yet connected).
type Upstream struct {
	cfg              UpstreamConfig
	connectTimeout   time.Duration
	keepAlive        time.Duration
	reconnectInitial time.Duration
	reconnectMax     time.Duration

	client         *mcp.Client
	session        *mcp.ClientSession
	transport      mcp.Transport
	cleanup        func() error
	onReconnect    func(context.Context, string) error
	buildTransport TransportBuilder
	resolver       SecretResolver
	redactor       *Redactor

	logger *slog.Logger

	mu               sync.RWMutex
	supervisorCancel context.CancelFunc
	supervisorDone   chan struct{}
	closed           bool
}

// UpstreamOptions configures optional dependencies for an Upstream.
type UpstreamOptions struct {
	Logger   *slog.Logger
	Resolver SecretResolver
	// TransportBuilder builds the mcp.Transport for this upstream.
	// If nil, the default production builder (stdio/http/sse via config) is used.
	TransportBuilder      TransportBuilder
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
	// Redactor is the per-upstream redactor. If nil, no redaction is applied.
	Redactor *Redactor
}

func (o *UpstreamOptions) setDefaults() {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.TransportBuilder == nil {
		o.TransportBuilder = defaultTransportBuilder
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
		buildTransport:   opts.TransportBuilder,
		redactor:         opts.Redactor,
	}
	impl := &mcp.Implementation{Name: "mcpgateway-client", Version: "0"}
	u.client = mcp.NewClient(impl, &mcp.ClientOptions{
		Logger:    opts.Logger,
		KeepAlive: keepAlive,
		ToolListChangedHandler: func(ctx context.Context, _ *mcp.ToolListChangedRequest) {
			if opts.OnToolListChanged != nil {
				if err := opts.OnToolListChanged(ctx, cfg.Name); err != nil {
					u.logger.Warn("tool list changed handler failed", "upstream", cfg.Name, "error", err)
				}
			}
		},
		PromptListChangedHandler: func(ctx context.Context, _ *mcp.PromptListChangedRequest) {
			if opts.OnPromptListChanged != nil {
				if err := opts.OnPromptListChanged(ctx, cfg.Name); err != nil {
					u.logger.Warn("prompt list changed handler failed", "upstream", cfg.Name, "error", err)
				}
			}
		},
		ResourceListChangedHandler: func(ctx context.Context, _ *mcp.ResourceListChangedRequest) {
			if opts.OnResourceListChanged != nil {
				if err := opts.OnResourceListChanged(ctx, cfg.Name); err != nil {
					u.logger.Warn("resource list changed handler failed", "upstream", cfg.Name, "error", err)
				}
			}
		},
		ResourceUpdatedHandler: func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			if opts.OnResourceUpdated != nil && req != nil && req.Params != nil {
				if err := opts.OnResourceUpdated(ctx, cfg.Name, req.Params.URI); err != nil {
					u.logger.Warn("resource updated handler failed", "upstream", cfg.Name, "error", err)
				}
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
		u.startSupervisor(ctx, u.onReconnect)
		return err
	}
	u.startSupervisor(ctx, u.onReconnect)
	return nil
}

func (u *Upstream) connectOnce(ctx context.Context) (rerr error) {
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

	tr, closeTransport, err := u.buildTransport(ctx, u.cfg, u.resolver)
	if err != nil {
		return errors.Wrap(err, "build transport")
	}
	defer func() {
		if rerr != nil && closeTransport != nil {
			_ = closeTransport()
		}
	}()

	timeout := u.connectTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sess, err := u.client.Connect(cctx, tr, nil)
	if err != nil {
		return errors.Wrap(err, "connect")
	}

	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		_ = sess.Close()
		return errors.New("upstream closed")
	}
	u.transport = tr
	u.cleanup = closeTransport
	u.session = sess
	u.mu.Unlock()

	return nil
}

func (u *Upstream) startSupervisor(ctx context.Context, onReconnect func(context.Context, string) error) {
	u.mu.Lock()
	if u.closed || u.supervisorDone != nil || u.reconnectInitial < 0 {
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
	bo := u.newReconnectBackOff()
	for {
		sess := u.currentSession()
		if sess == nil {
			if !u.reconnectLoop(ctx, bo, onReconnect) {
				return
			}
			continue
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

		if !u.reconnectLoop(ctx, bo, onReconnect) {
			return
		}
	}
}

func (u *Upstream) newReconnectBackOff() *backoff.ExponentialBackOff {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = u.reconnectInitial
	bo.MaxInterval = u.reconnectMax
	bo.Multiplier = 2
	bo.Reset()
	return bo
}

func (u *Upstream) reconnectLoop(ctx context.Context, bo *backoff.ExponentialBackOff, onReconnect func(context.Context, string) error) bool {
	attempt := 0
	for {
		delay := bo.NextBackOff()
		if delay == backoff.Stop {
			return false
		}
		if !waitBackOff(ctx, delay) {
			return false
		}
		attempt++
		if err := u.connectOnce(ctx); err != nil {
			u.logger.Warn("upstream reconnect failed", "attempt", attempt, "backoff", delay, "error", err)
			continue
		}
		u.logger.Info("upstream connected", "attempt", attempt)
		bo.Reset()
		if onReconnect != nil {
			if err := onReconnect(ctx, u.cfg.Name); err != nil {
				u.logger.Warn("upstream reconnect re-sync failed", "error", err)
			}
		}
		return true
	}
}

func waitBackOff(ctx context.Context, d time.Duration) bool {
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

// newUpstreamWithTransport is a test helper that injects a ready transport.
func newUpstreamWithTransport(cfg UpstreamConfig, tr mcp.Transport, cl func() error) *Upstream {
	return &Upstream{cfg: cfg, transport: tr, cleanup: cl, logger: slog.Default(), reconnectInitial: time.Second, reconnectMax: 30 * time.Second, buildTransport: defaultTransportBuilder}
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
	u := &Upstream{cfg: cfg, logger: slog.Default(), reconnectInitial: time.Second, reconnectMax: 30 * time.Second, onReconnect: cb.OnReconnect, buildTransport: defaultTransportBuilder}
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

func (u *Upstream) hasRoute() bool { return u.cfg.Route.Host != "" || u.cfg.Route.Path != "" }

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
	return collectSeq(sess.Tools(ctx, &mcp.ListToolsParams{}))
}

// CallTool forwards the call to the upstream session.
func (u *Upstream) CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	sess := u.currentSession()
	if sess == nil {
		return nil, errors.New("not connected")
	}
	return sess.CallTool(ctx, params)
}

// ListPrompts calls the upstream. Upstreams that don't declare the prompts
// capability are treated as having none, rather than failing the gateway.
func (u *Upstream) ListPrompts(ctx context.Context) ([]*mcp.Prompt, error) {
	sess := u.currentSession()
	if sess == nil {
		return nil, errors.New("not connected")
	}
	if res := sess.InitializeResult(); res == nil || res.Capabilities == nil || res.Capabilities.Prompts == nil {
		return nil, nil
	}
	return collectSeq(sess.Prompts(ctx, &mcp.ListPromptsParams{}))
}

// GetPrompt forwards the call to the upstream session.
func (u *Upstream) GetPrompt(ctx context.Context, params *mcp.GetPromptParams) (*mcp.GetPromptResult, error) {
	sess := u.currentSession()
	if sess == nil {
		return nil, errors.New("not connected")
	}
	return sess.GetPrompt(ctx, params)
}

// ListResources calls the upstream. Upstreams that don't declare the
// resources capability are treated as having none, rather than failing the
// gateway.
func (u *Upstream) ListResources(ctx context.Context) ([]*mcp.Resource, error) {
	sess := u.currentSession()
	if sess == nil {
		return nil, errors.New("not connected")
	}
	if res := sess.InitializeResult(); res == nil || res.Capabilities == nil || res.Capabilities.Resources == nil {
		return nil, nil
	}
	return collectSeq(sess.Resources(ctx, &mcp.ListResourcesParams{}))
}

// ListResourceTemplates calls the upstream. Upstreams that don't declare the
// resources capability are treated as having none, rather than failing the
// gateway.
func (u *Upstream) ListResourceTemplates(ctx context.Context) ([]*mcp.ResourceTemplate, error) {
	sess := u.currentSession()
	if sess == nil {
		return nil, errors.New("not connected")
	}
	if res := sess.InitializeResult(); res == nil || res.Capabilities == nil || res.Capabilities.Resources == nil {
		return nil, nil
	}
	return collectSeq(sess.ResourceTemplates(ctx, &mcp.ListResourceTemplatesParams{}))
}

func collectSeq[T any](seq iter.Seq2[T, error]) ([]T, error) {
	out := make([]T, 0)
	for v, err := range seq {
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
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
// It truncates s to at most maxLen bytes, cutting only at rune boundaries,
// and appends "…" when truncation occurs.
func TrimDescription(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	// Walk rune-by-rune and stop before exceeding maxLen bytes.
	i := 0
	for i < maxLen {
		_, size := utf8.DecodeRuneInString(s[i:])
		if i+size > maxLen {
			break
		}
		i += size
	}
	return s[:i] + "…"
}
