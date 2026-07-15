package session

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// dialTestClient opens a raw *ssh.Client to addr, suitable for [Pool.Adopt]:
// Adopt takes ownership of an already-connected client, so tests build one
// the same way a future sandbox Runner would (minus the container part).
func dialTestClient(t *testing.T, addr string) *ssh.Client {
	t.Helper()
	client, _, _, err := dialInsecure(t, addr).dial()
	require.NoError(t, err)
	return client
}

func TestPool_Adopt_SessionIsUsable(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	client := dialTestClient(t, srv.addr)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := NewPool(PoolOptions{})
	go p.RunLoop(ctx)

	res, err := p.Adopt(ctx, AdoptRequest{Machine: "sandbox/quiet-hopper", Client: client})
	require.NoError(t, err)
	require.NotEmpty(t, res.ID)
	require.NotEmpty(t, res.Label)
	t.Cleanup(func() { _ = p.Close(context.Background(), res.ID) })

	machine, err := p.Machine(ctx, res.ID)
	require.NoError(t, err)
	require.Equal(t, "sandbox/quiet-hopper", machine)

	execRes := p.Exec(ctx, ExecRequest{SessionID: res.ID, Command: testCmd})
	require.NoError(t, execRes.Err)
	require.Equal(t, testOut, execRes.Stdout)
}

func TestPool_Adopt_RequiresClient(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := NewPool(PoolOptions{})
	go p.RunLoop(ctx)

	_, err := p.Adopt(ctx, AdoptRequest{Machine: "sandbox/x"})
	require.Error(t, err)
}

// onCloseCounter is an OnClose callback that records how many times it ran
// and lets a test block it open (to verify it never runs on the actor
// goroutine) or observe completion without a real sleep.
type onCloseCounter struct {
	calls  atomic.Int64
	done   chan struct{}
	unlock chan struct{} // if non-nil, onClose blocks on this before finishing
}

func newOnCloseCounter() *onCloseCounter {
	return &onCloseCounter{done: make(chan struct{}, 16)}
}

func (c *onCloseCounter) fn() func() {
	return func() {
		if c.unlock != nil {
			<-c.unlock
		}
		c.calls.Add(1)
		c.done <- struct{}{}
	}
}

func (c *onCloseCounter) waitOnce(t *testing.T) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("onClose did not fire in time")
	}
}

func TestPool_Adopt_OnCloseFiresOnExplicitClose(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	client := dialTestClient(t, srv.addr)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := NewPool(PoolOptions{})
	go p.RunLoop(ctx)

	oc := newOnCloseCounter()
	res, err := p.Adopt(ctx, AdoptRequest{Machine: "sandbox/x", Client: client, OnClose: oc.fn()})
	require.NoError(t, err)

	require.NoError(t, p.Close(ctx, res.ID))
	oc.waitOnce(t)
	require.EqualValues(t, 1, oc.calls.Load())

	// Closing an already-closed (unknown) session must not fire OnClose again.
	_ = p.Close(ctx, res.ID)
	select {
	case <-oc.done:
		t.Fatal("OnClose fired a second time")
	case <-time.After(50 * time.Millisecond):
	}
	require.EqualValues(t, 1, oc.calls.Load())
}

func TestPool_Adopt_OnCloseFiresOnShutdown(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	client := dialTestClient(t, srv.addr)

	ctx, cancel := context.WithCancel(context.Background())
	p := NewPool(PoolOptions{})
	go p.RunLoop(ctx)

	oc := newOnCloseCounter()
	_, err := p.Adopt(ctx, AdoptRequest{Machine: "sandbox/x", Client: client, OnClose: oc.fn()})
	require.NoError(t, err)

	cancel() // shut the pool down
	oc.waitOnce(t)
	require.EqualValues(t, 1, oc.calls.Load())
}

func TestPool_Adopt_OnCloseDoesNotBlockActor(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	blockedClient := dialTestClient(t, srv.addr)
	otherClient := dialTestClient(t, srv.addr)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := NewPool(PoolOptions{})
	go p.RunLoop(ctx)

	oc := newOnCloseCounter()
	oc.unlock = make(chan struct{})
	blocked, err := p.Adopt(ctx, AdoptRequest{Machine: "sandbox/blocked", Client: blockedClient, OnClose: oc.fn()})
	require.NoError(t, err)

	other, err := p.Adopt(ctx, AdoptRequest{Machine: "sandbox/other", Client: otherClient})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close(context.Background(), other.ID) })

	// Close the session whose OnClose blocks forever until we release it below.
	require.NoError(t, p.Close(ctx, blocked.ID))

	// The actor loop must still service unrelated requests immediately.
	shortCtx, shortCancel := context.WithTimeout(ctx, 2*time.Second)
	defer shortCancel()
	execRes := p.Exec(shortCtx, ExecRequest{SessionID: other.ID, Command: testCmd})
	require.NoError(t, execRes.Err)
	require.Equal(t, testOut, execRes.Stdout)

	// Release the blocked OnClose so its goroutine doesn't leak past the test.
	close(oc.unlock)
	oc.waitOnce(t)
}

func TestPool_SweepIdle(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	p := NewPool(PoolOptions{IdleTimeout: time.Minute})

	freshClient := dialTestClient(t, srv.addr)
	t.Cleanup(func() { _ = freshClient.Close() })
	freshOC := newOnCloseCounter()
	fresh := newSession(context.Background(), "fresh-id", "fresh", "fresh-label",
		freshClient, nil, "", "", "linux", freshOC.fn())
	fresh.lastUsed.Store(time.Now().UnixNano())

	idleOC := newOnCloseCounter()
	idle := newSession(context.Background(), "idle-id", "idle", "idle-label",
		dialTestClient(t, srv.addr), nil, "", "", "linux", idleOC.fn())
	idle.lastUsed.Store(time.Now().Add(-time.Hour).UnixNano())

	sessions := map[string]*Session{
		fresh.ID: fresh,
		idle.ID:  idle,
	}

	p.sweepIdle(sessions, time.Now())

	_, freshStillThere := sessions[fresh.ID]
	require.True(t, freshStillThere, "recently used session must survive the sweep")
	select {
	case <-freshOC.done:
		t.Fatal("OnClose must not fire for a fresh session")
	case <-time.After(50 * time.Millisecond):
	}

	_, idleStillThere := sessions[idle.ID]
	require.False(t, idleStillThere, "idle session must be closed by the sweep")
	idleOC.waitOnce(t)
	require.EqualValues(t, 1, idleOC.calls.Load())

	fresh.cancel()
}
