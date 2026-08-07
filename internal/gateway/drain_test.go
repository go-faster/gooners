package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// blockingUpstream serves one tool that parks until release is closed, so a
// test can hold a call in flight across a close. Every transport it builds
// closes cleaned when its cleanup runs — in production that cleanup is the
// stdio SIGKILL, which is what actually frees a wedged upstream.
func blockingUpstream(t *testing.T, release <-chan struct{}) (build TransportBuilder, cleaned <-chan struct{}) {
	t.Helper()
	cleanups := make(chan struct{}, 8)
	buildFn := func(context.Context, UpstreamConfig, SecretResolver) (mcp.Transport, func() error, error) {
		serverTr, clientTr := mcp.NewInMemoryTransports()
		srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "0"}, nil)
		srv.AddTool(
			&mcp.Tool{Name: "slow", Description: "slow", InputSchema: map[string]any{"type": "object"}},
			func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil
			},
		)
		go func() { _ = srv.Run(context.Background(), serverTr) }()
		return clientTr, func() error {
			select {
			case cleanups <- struct{}{}:
			default:
			}
			return nil
		}, nil
	}
	done := make(chan struct{})
	go func() {
		<-cleanups
		close(done)
	}()
	return buildFn, done
}

func newBlockingUpstream(t *testing.T, release <-chan struct{}, drainTimeout time.Duration) (up *Upstream, cleaned <-chan struct{}) {
	t.Helper()
	buildFn, cleanups := blockingUpstream(t, release)
	u, err := NewUpstream(upstreamCfg("u1"), UpstreamOptions{
		TransportBuilder: buildFn,
		DrainTimeout:     drainTimeout,
		ReconnectInitial: time.Millisecond,
		ReconnectMax:     time.Millisecond,
	})
	require.NoError(t, err)
	require.NoError(t, u.Connect(t.Context()))
	return u, cleanups
}

// startCall launches a tool call and returns a channel carrying its error.
func startCall(t *testing.T, u *Upstream) <-chan error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		_, err := u.CallTool(context.Background(), &mcp.CallToolParams{Name: "slow"})
		errCh <- err
	}()
	// Wait until the call is actually counted, so the close under test races
	// with a genuinely in-flight call rather than with goroutine startup.
	require.Eventually(t, func() bool {
		u.mu.RLock()
		defer u.mu.RUnlock()
		return u.inflight > 0
	}, 5*time.Second, time.Millisecond)
	return errCh
}

func requireClosed(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatal(msg)
	}
}

// closeAsync closes u in the background and reports when it returned.
func closeAsync(t *testing.T, u *Upstream) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		require.NoError(t, u.Close(context.Background()))
	}()
	return done
}

// Close must wait for an in-flight call and let it return its result.
func TestUpstream_CloseDrainsInflight(t *testing.T) {
	release := make(chan struct{})
	u, _ := newBlockingUpstream(t, release, 5*time.Second)
	errCh := startCall(t, u)

	closed := closeAsync(t, u)
	select {
	case <-closed:
		t.Fatal("close did not wait for the in-flight call")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	require.NoError(t, <-errCh, "in-flight call was cut off by close")
	requireClosed(t, closed, "close did not finish after draining")
}

// A call that never finishes must not block shutdown. This is the case that
// matters: mcp.ClientSession.Close waits for outstanding calls, so without a
// bound on it the drain timeout would bound nothing and one wedged upstream
// would hang the process forever.
func TestUpstream_CloseDrainTimesOut(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	u, cleaned := newBlockingUpstream(t, release, 50*time.Millisecond)
	_ = startCall(t, u)

	start := time.Now()
	requireClosed(t, closeAsync(t, u), "close hung on a stuck upstream")
	require.Less(t, time.Since(start), 30*time.Second)

	// The transport is torn down even though the session close never finished;
	// in production this is the SIGKILL that frees the wedged process.
	requireClosed(t, cleaned, "transport was never torn down")
}

// A negative timeout opts out of waiting entirely.
func TestUpstream_DrainDisabled(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	u, cleaned := newBlockingUpstream(t, release, -1)
	_ = startCall(t, u)

	start := time.Now()
	requireClosed(t, closeAsync(t, u), "close hung with draining disabled")
	require.Less(t, time.Since(start), 30*time.Second)
	requireClosed(t, cleaned, "transport was never torn down")
}

// Draining must not accept new work: once Close starts, calls are refused
// rather than queued behind the drain.
func TestUpstream_ClosedRefusesNewCalls(t *testing.T) {
	release := make(chan struct{})
	close(release)
	u, _ := newBlockingUpstream(t, release, time.Second)
	require.NoError(t, u.Close(context.Background()))

	_, err := u.CallTool(t.Context(), &mcp.CallToolParams{Name: "slow"})
	require.ErrorIs(t, err, errUpstreamClosed)
	_, err = u.ListTools(t.Context())
	require.ErrorIs(t, err, errUpstreamClosed)
}

// Removing an upstream by reload drains it like any other close.
func TestGateway_Reload_DrainsRemovedUpstream(t *testing.T) {
	release := make(chan struct{})
	buildFn, _ := blockingUpstream(t, release)
	cfg := &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{upstreamCfg("u1"), upstreamCfg("u2")},
	}
	g, err := New(cfg, Options{TransportBuilder: buildFn})
	require.NoError(t, err)
	require.NoError(t, g.Build(t.Context()))
	t.Cleanup(func() { _ = g.Close(context.Background()) })

	u := g.upstreamByName("u2")
	require.NotNil(t, u)
	errCh := startCall(t, u)

	reloaded := make(chan struct{})
	go func() {
		defer close(reloaded)
		_, err := g.Reload(context.Background(), &Config{
			Server:    ServerConfig{Name: "gw"},
			Upstreams: []UpstreamConfig{upstreamCfg("u1")},
		})
		require.NoError(t, err)
	}()

	select {
	case <-reloaded:
		t.Fatal("reload did not drain the removed upstream")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	require.NoError(t, <-errCh, "reload cut off an in-flight call")
	requireClosed(t, reloaded, "reload did not finish")
}

func TestServerConfig_DrainTimeout(t *testing.T) {
	cfg := &Config{
		Server:    ServerConfig{Name: "gw", DrainTimeout: "nonsense"},
		Upstreams: []UpstreamConfig{upstreamCfg("u1")},
	}
	require.ErrorContains(t, cfg.Validate(), "drain_timeout")

	cfg.Server.DrainTimeout = "250ms"
	require.NoError(t, cfg.Validate())
	g, err := New(cfg, Options{TransportBuilder: fakeUpstreams(map[string][]string{"u1": {"a"}})})
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close(context.Background()) })
	require.Equal(t, 250*time.Millisecond, g.upstreamByName("u1").drainTimeout)
}

// call_timeout bounds one call. It is deliberately separate from drain_timeout:
// an upstream may have legitimately long-running tools and still need a bounded
// shutdown.
func TestUpstream_CallTimeout(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	buildFn, _ := blockingUpstream(t, release)
	u, err := NewUpstream(upstreamCfg("u1"), UpstreamOptions{
		TransportBuilder: buildFn,
		CallTimeout:      50 * time.Millisecond,
		ReconnectInitial: time.Millisecond,
		ReconnectMax:     time.Millisecond,
	})
	require.NoError(t, err)
	require.NoError(t, u.Connect(t.Context()))
	t.Cleanup(func() { _ = u.Close(context.Background()) })

	_, err = u.CallTool(context.Background(), &mcp.CallToolParams{Name: "slow"})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	// The timed-out call must not stay counted, or every later drain would
	// wait out its full timeout for a call that is long gone.
	u.mu.RLock()
	inflight := u.inflight
	u.mu.RUnlock()
	require.Zero(t, inflight)
}

// Zero means unlimited, so a long-running tool is never cut off.
func TestUpstream_CallTimeoutUnlimited(t *testing.T) {
	release := make(chan struct{})
	buildFn, _ := blockingUpstream(t, release)
	u, err := NewUpstream(upstreamCfg("u1"), UpstreamOptions{
		TransportBuilder: buildFn,
		ReconnectInitial: time.Millisecond,
		ReconnectMax:     time.Millisecond,
	})
	require.NoError(t, err)
	require.NoError(t, u.Connect(t.Context()))
	t.Cleanup(func() { _ = u.Close(context.Background()) })

	errCh := startCall(t, u)
	select {
	case err := <-errCh:
		t.Fatalf("call was cut off with no timeout configured: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-errCh)
}

func TestUpstreamConfig_CallTimeout(t *testing.T) {
	uc := upstreamCfg("u1")
	uc.CallTimeout = "nonsense"
	cfg := &Config{Server: ServerConfig{Name: "gw"}, Upstreams: []UpstreamConfig{uc}}
	require.ErrorContains(t, cfg.Validate(), "call_timeout")

	cfg.Upstreams[0].CallTimeout = "90s"
	require.NoError(t, cfg.Validate())
	g, err := New(cfg, Options{TransportBuilder: fakeUpstreams(map[string][]string{"u1": {"a"}})})
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close(context.Background()) })
	require.Equal(t, 90*time.Second, g.upstreamByName("u1").callTimeout)
}
