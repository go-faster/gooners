package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestPool_IdleSweep_EndToEnd exercises the real ticker/select wiring inside
// Pool.RunLoop (not just sweepIdle called directly, as TestPool_SweepIdle in
// pool_test.go does) against a genuinely adopted, live *ssh.Client. See the
// long comment on TestIdleSweepInterval_TickerCadence in
// idle_sweep_synctest_test.go for why this can't be done inside a
// testing/synctest bubble: real time and a bounded poll are used instead.
func TestPool_IdleSweep_EndToEnd(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	client := dialTestClient(t, srv.addr)

	const idleTimeout = 200 * time.Millisecond
	p := NewPool(PoolOptions{IdleTimeout: idleTimeout})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.RunLoop(ctx)

	oc := newOnCloseCounter()
	res, err := p.Adopt(ctx, AdoptRequest{Machine: "sandbox/idle-sweep", Client: client, OnClose: oc.fn()})
	require.NoError(t, err)
	require.NotEmpty(t, res.ID)

	// Freshly adopted: must be live immediately.
	_, err = p.Get(ctx, res.ID)
	require.NoError(t, err)

	// RunLoop's real ticker fires every idleSweepInterval(idleTimeout); wait
	// for it to actually reap the session, bounded well above worst-case
	// scheduling jitter so this stays non-flaky under -race/CI load.
	oc.waitOnce(t)
	require.EqualValues(t, 1, oc.calls.Load())

	_, err = p.Get(ctx, res.ID)
	require.Error(t, err, "session must be reaped by the idle sweep ticker")
}
