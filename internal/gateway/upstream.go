// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"context"
	"log/slog"
	"path"
	"time"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	gwtransport "github.com/go-faster/gooners/internal/gateway/transport"
)

// Upstream represents one configured upstream MCP server (not yet connected).
type Upstream struct {
	cfg            UpstreamConfig
	client         *mcp.Client
	session        *mcp.ClientSession
	resolver       SecretResolver
	logger         *slog.Logger
	transport      mcp.Transport
	cleanup        func() error
	connectTimeout time.Duration
}

// UpstreamOptions configures optional dependencies for an Upstream.
type UpstreamOptions struct {
	Logger            *slog.Logger
	Resolver          SecretResolver
	OnToolListChanged func(ctx context.Context, upstreamName string) error
	ConnectTimeout    time.Duration
}

func (o *UpstreamOptions) setDefaults() {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.ConnectTimeout == 0 {
		o.ConnectTimeout = 10 * time.Second
	}
}

// NewUpstream creates an Upstream from config. It does not connect.
func NewUpstream(cfg UpstreamConfig, opts UpstreamOptions) (*Upstream, error) {
	opts.setDefaults()
	u := &Upstream{
		cfg:            cfg,
		resolver:       opts.Resolver,
		logger:         opts.Logger,
		connectTimeout: opts.ConnectTimeout,
	}
	impl := &mcp.Implementation{Name: "mcpgateway-client", Version: "0"}
	u.client = mcp.NewClient(impl, &mcp.ClientOptions{
		Logger: opts.Logger,
		ToolListChangedHandler: func(_ context.Context, _ *mcp.ToolListChangedRequest) {
			if opts.OnToolListChanged != nil {
				_ = opts.OnToolListChanged(context.Background(), cfg.Name)
			}
		},
	})
	return u, nil
}

// Connect establishes the session using the injected BuildTransport.
// If the session is already set (e.g. by test helper), this is a no-op.
func (u *Upstream) Connect(ctx context.Context) error {
	if u.session != nil {
		return nil
	}
	tr, cl, err := BuildTransport(ctx, u.cfg, u.resolver)
	if err != nil {
		return errors.Wrap(err, "build transport")
	}
	u.transport = tr
	u.cleanup = cl
	timeout := u.connectTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sess, err := u.client.Connect(cctx, tr, nil)
	if err != nil {
		u.transport = nil
		u.cleanup = nil
		u.session = nil
		return errors.Wrap(err, "connect")
	}
	u.session = sess
	return nil
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
	return &Upstream{cfg: cfg, transport: tr, cleanup: cl}
}

// newUpstreamWithInMemoryClient constructs Upstream with mcp.Client wired to call handlerOnListChanged on tool list changed.
// Does not call Connect; caller must.
func newUpstreamWithInMemoryClient(cfg UpstreamConfig, clientTr mcp.Transport, handlerOnListChanged func(context.Context, string) error) *Upstream {
	u := &Upstream{cfg: cfg}
	impl := &mcp.Implementation{Name: "mcpgateway-client", Version: "0"}
	u.client = mcp.NewClient(impl, &mcp.ClientOptions{
		Logger: slog.Default(),
		ToolListChangedHandler: func(_ context.Context, _ *mcp.ToolListChangedRequest) {
			if handlerOnListChanged != nil {
				_ = handlerOnListChanged(context.Background(), cfg.Name)
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
	if u.session != nil {
		_ = u.session.Close()
	}
	if u.cleanup != nil {
		_ = u.cleanup()
	}
	return nil
}

// ListTools calls the upstream.
func (u *Upstream) ListTools(ctx context.Context) ([]*mcp.Tool, error) {
	if u.session == nil {
		return nil, errors.New("not connected")
	}
	res, err := u.session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// CallTool forwards the call to the upstream session.
func (u *Upstream) CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if u.session == nil {
		return nil, errors.New("not connected")
	}
	return u.session.CallTool(ctx, params)
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
