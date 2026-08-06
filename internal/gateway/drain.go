package gateway

import (
	"context"
	"time"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultDrainTimeout bounds each phase of a closing upstream: waiting for
// in-flight calls, and then the session close itself. It is a compromise: long
// enough that an ordinary tool call finishes, short enough that a reload
// removing several upstreams, or a process shutting down, is not held hostage
// by one wedged request. Worst case a single upstream takes twice this, and
// upstreams close concurrently.
const defaultDrainTimeout = 5 * time.Second

// waitTimeout reports whether ch closed within timeout. A negative timeout does
// not wait at all.
func waitTimeout(ch <-chan struct{}, timeout time.Duration) bool {
	if timeout < 0 {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}

// errUpstreamClosed is returned once an upstream stops accepting calls, which
// happens before its session is torn down.
var errUpstreamClosed = errors.New("upstream closed")

// enter claims the session for one call and counts it as in flight, so
// [Upstream.Close] can wait for it. Every caller must pair it with a deferred
// leave.
func (u *Upstream) enter() (*mcp.ClientSession, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return nil, errUpstreamClosed
	}
	if u.session == nil {
		return nil, errors.New("not connected")
	}
	u.inflight++
	return u.session, nil
}

// withCallTimeout bounds one request to this upstream. A zero timeout means no
// limit: an upstream whose tools legitimately run for minutes must not be cut
// off, and shutdown is bounded separately by the drain timeout, which does not
// depend on calls being short.
func (u *Upstream) withCallTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if u.callTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, u.callTimeout)
}

// withListTimeout bounds one feature listing — tools, prompts, resources or
// resource templates.
//
// Listing is not a tool call, and it must not inherit call_timeout's "no limit"
// default. It happens during Build and after every reconnect, before the
// gateway can serve anything, so an upstream that completes the handshake and
// then never answers tools/list would otherwise park startup forever: the
// connect timeout has already been satisfied, and nothing else bounds it.
//
// An explicit call_timeout still wins, so an operator who set one gets it
// everywhere rather than having listing quietly use a larger bound.
func (u *Upstream) withListTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if u.callTimeout > 0 {
		return context.WithTimeout(ctx, u.callTimeout)
	}
	timeout := u.connectTimeout
	if timeout <= 0 {
		timeout = defaultConnectTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// leave releases a claim taken by enter, waking a waiting drain once the last
// call finishes.
func (u *Upstream) leave() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.inflight--
	if u.inflight == 0 && u.drained != nil {
		close(u.drained)
		u.drained = nil
	}
}

// drain waits for in-flight calls to finish, and reports whether they all did.
// It must run before the supervisor is canceled: canceling closes the session,
// which is exactly what the in-flight calls still need.
//
// It deliberately takes no context. Draining happens on the shutdown path, where
// the caller's context is usually already canceled — honoring it would turn
// every drain into an immediate abort, which is the behavior draining exists to
// avoid.
func (u *Upstream) drain(timeout time.Duration) bool {
	if timeout < 0 {
		return true
	}
	u.mu.Lock()
	if u.inflight == 0 {
		u.mu.Unlock()
		return true
	}
	if u.drained == nil {
		u.drained = make(chan struct{})
	}
	ch := u.drained
	inflight := u.inflight
	u.mu.Unlock()

	u.logger.Info("draining upstream", "inflight", inflight, "timeout", timeout)
	return waitTimeout(ch, timeout)
}
