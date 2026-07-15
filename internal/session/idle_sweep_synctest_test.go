package session

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

// Why this file does *not* run Pool.RunLoop against a real, live *ssh.Client
// inside a synctest.Test bubble
//
// The obvious design - dial a real *ssh.Client (as every other test in this
// package does via newTestServer/dialTestClient), Adopt it into a Pool whose
// RunLoop runs inside a synctest bubble, advance virtual time past
// IdleTimeout, and assert the ticker reaped it - was tried first and
// rejected after two separate empirical failures:
//
//  1. The handshake itself cannot happen inside the bubble. A probe driving
//     ssh.NewServerConn/ssh.NewClientConn over a net.Pipe from within
//     synctest.Test deadlocks immediately ("deadlock: all goroutines in
//     bubble are blocked"): golang.org/x/crypto/ssh's version exchange has
//     both sides call Write before either calls Read, which needs a
//     concurrently-scheduled reader on the other end of net.Pipe's blocking,
//     unbuffered channel. The same deadlock reproduces with a real
//     net.Listen+net.Dial pair outside any bubble too, confirming it's an
//     inherent net.Pipe/protocol-ordering issue, not a synctest artifact -
//     so net.Pipe is a dead end for this handshake regardless.
//
//  2. Doing the handshake outside the bubble (as the task's own suggested
//     fallback describes) and only running RunLoop inside the bubble is
//     *also* unreliable - not flaky, but a deterministic fatal error every
//     time: golang.org/x/crypto/ssh.Client.Wait (which Pool.watchSession
//     calls in a goroutine it starts, and that goroutine is therefore part
//     of the bubble because it's started from RunLoop, itself started
//     inside the bubble) blocks on a sync.Cond owned by the connection's
//     mux. That mux's loop goroutine was necessarily started *before* the
//     bubble (during the handshake), so it is not a bubble member. The
//     instant anything closes the client - idle sweep's closeSession, an
//     explicit Close, or even just ctx cancellation at shutdown - the mux
//     loop's Cond.Broadcast reaches into the bubble to wake watchSession's
//     Wait goroutine, and the runtime aborts the whole process with "fatal
//     error: semaphore wake of synctest goroutine from outside bubble".
//     This reproduced on every single run, including in the degenerate case
//     of adopting a client and never touching it again beyond the deferred
//     ctx cancellation used to shut RunLoop down at the end of the test.
//
// In short: a genuinely live *ssh.Client always has an external (pre-bubble)
// goroutine bridging it via sync.Cond, and Pool's own watchSession always
// waits on that Cond from a bubble goroutine - so there is no way to let a
// real adopted client actually get closed while a synctest bubble is
// running, regardless of what triggers the close. This is a structural
// incompatibility between golang.org/x/crypto/ssh's Client.Wait and
// testing/synctest, not something a differently-shaped test can route
// around.
//
// Given that, this file splits the coverage TestPool_SweepIdle (in
// pool_test.go, which calls sweepIdle directly) is missing into two
// narrower, still-real, hermetic pieces:
//
//   - TestIdleSweepInterval_TickerCadence below proves, inside a synctest
//     bubble with a virtual clock, that the actual production
//     idleSweepInterval helper combined with a real time.Ticker/select loop
//     (the same shape RunLoop uses) fires at the cadence RunLoop assumes -
//     with no ssh.Client involved at all, so it can't hit the crash above.
//   - TestPool_IdleSweep_EndToEnd (in idle_sweep_endtoend_test.go) proves,
//     using real wall-clock time and a real adopted *ssh.Client (like every
//     other Pool test), that RunLoop's actual ticker branch really does
//     reap an adopted session end-to-end - the one thing synctest can't
//     safely observe here.
func TestIdleSweepInterval_TickerCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const idleTimeout = 2 * time.Second
		interval := idleSweepInterval(idleTimeout)
		require.Positive(t, interval)
		require.LessOrEqual(t, interval, idleTimeout)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		const wantTicks = 5
		start := time.Now()
		for range wantTicks {
			<-ticker.C
		}
		elapsed := time.Since(start)

		// The fake clock advances in exact ticker-interval increments, so
		// after wantTicks fires, elapsed must be exactly wantTicks*interval
		// - proving the ticker built from idleSweepInterval is what's
		// actually driving the bubble's virtual clock forward, the same
		// way it drives RunLoop's real select loop.
		require.Equal(t, time.Duration(wantTicks)*interval, elapsed)
	})
}
