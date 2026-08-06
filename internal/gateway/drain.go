package gateway

import (
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
