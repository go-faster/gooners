package sandbox_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/go-faster/gooners/internal/sandbox"
	"github.com/go-faster/gooners/internal/sandbox/agent"
	"github.com/go-faster/gooners/internal/sandbox/streamconn"
	"github.com/go-faster/gooners/internal/session"
)

// fakeRunner is a hand-rolled, in-memory sandbox.Runner double: Manager's
// contract with a Runner (Create/Dial/Destroy/List/Close) is small enough
// that a real implementation adds nothing a fake can't exercise, and this
// keeps Manager's tests Docker-free.
type fakeRunner struct {
	mu        sync.Mutex
	nextID    int
	created   []string
	destroyed []string

	createErr error
	dialConn  func(id string) (net.Conn, error) // returns the "manager side" conn for Dial
	sandboxes []*sandbox.Sandbox                // canned response for List
}

func (f *fakeRunner) Create(_ context.Context, spec sandbox.Spec) (*sandbox.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.nextID++
	id := "fake-" + string(rune('a'+f.nextID))
	f.created = append(f.created, id)
	return &sandbox.Sandbox{ID: id, Image: spec.Image, Network: spec.Network, CreatedAt: time.Now()}, nil
}

func (f *fakeRunner) Dial(_ context.Context, id string) (net.Conn, error) {
	if f.dialConn == nil {
		return nil, errors.New("fakeRunner: no dialConn configured")
	}
	return f.dialConn(id)
}

func (f *fakeRunner) Destroy(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyed = append(f.destroyed, id)
	return nil
}

func (f *fakeRunner) List(_ context.Context, _ sandbox.Filter) ([]*sandbox.Sandbox, error) {
	return f.sandboxes, nil
}

func (f *fakeRunner) Close() error { return nil }

func (f *fakeRunner) destroyedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.destroyed))
	copy(out, f.destroyed)
	return out
}

func (f *fakeRunner) createdIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.created))
	copy(out, f.created)
	return out
}

// fakeAgentConn starts a real agent.Serve goroutine wired to the returned
// net.Conn via an in-memory pipe pair, exactly mirroring
// internal/sandbox/agent/serve_test.go's harness: the "manager side" is what
// Runner.Dial would hand back for a real container's hijacked exec stream.
func fakeAgentConn(t *testing.T) net.Conn {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("agent.Serve shells out to /bin/bash or /bin/sh, unavailable on windows")
	}

	agentIn, mgrOut := io.Pipe()
	mgrIn, agentOut := io.Pipe()

	mgrConn := streamconn.New(mgrIn, mgrOut, streamconn.Options{
		Close: func() error {
			_ = mgrIn.Close()
			return mgrOut.Close()
		},
	})

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	go func() {
		r := bufio.NewReader(agentIn)
		p, err := agent.ReadPreamble(r)
		if err != nil {
			return
		}
		hostKey, err := ssh.ParsePrivateKey([]byte(p.HostKeyPEM))
		if err != nil {
			return
		}
		authKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(p.AuthorizedKey)) //nolint:dogsled // ssh.ParseAuthorizedKey's comment/options/rest are unused
		if err != nil {
			return
		}
		agentConn := streamconn.New(r, agentOut, streamconn.Options{
			Close: func() error {
				_ = agentIn.Close()
				return agentOut.Close()
			},
		})
		_ = agent.Serve(ctx, agentConn, agent.Config{
			HostKey:       hostKey,
			AuthorizedKey: authKey,
			Version:       "test",
			Logger:        slog.New(slog.DiscardHandler),
		})
	}()

	return mgrConn
}

// brokenAgentConn returns a conn whose "agent side" never speaks SSH, so the
// handshake that Manager.Open drives will fail - simulating a live container
// whose agent binary crashed or was never injected correctly.
func brokenAgentConn(t *testing.T) net.Conn {
	t.Helper()
	client, remote := net.Pipe()
	t.Cleanup(func() { _ = remote.Close() })
	go func() {
		// Drain and discard anything written (the preamble), never respond.
		_, _ = io.Copy(io.Discard, remote)
	}()
	return client
}

func newTestManager(t *testing.T, runner *fakeRunner) (*sandbox.Manager, *session.Pool) {
	t.Helper()
	pool := session.NewPool(session.PoolOptions{Logger: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go pool.RunLoop(ctx)

	m := sandbox.NewManager(sandbox.ManagerOptions{
		Runner: runner,
		Pool:   pool,
		Policy: sandbox.Policy{},
		Logger: slog.New(slog.DiscardHandler),
	})
	return m, pool
}

func TestManager_Open_Success(t *testing.T) {
	runner := &fakeRunner{dialConn: func(string) (net.Conn, error) { return fakeAgentConn(t), nil }}
	m, pool := newTestManager(t, runner)

	res, err := m.Open(t.Context(), sandbox.Spec{})
	require.NoError(t, err)
	require.NotEmpty(t, res.ID)

	client, err := pool.Get(t.Context(), res.ID)
	require.NoError(t, err)
	require.NotNil(t, client)

	require.Empty(t, runner.destroyedIDs(), "a successful Open must not destroy its own sandbox")

	require.NoError(t, m.Close(t.Context(), res.ID))
	require.Eventually(t, func() bool {
		return len(runner.destroyedIDs()) == 1
	}, 2*time.Second, 10*time.Millisecond, "Manager.Close must destroy exactly once, via the pool's OnClose")
}

func TestManager_Open_DialFailure_DestroysOnError(t *testing.T) {
	runner := &fakeRunner{dialConn: func(string) (net.Conn, error) { return nil, errors.New("dial boom") }}
	m, _ := newTestManager(t, runner)

	_, err := m.Open(t.Context(), sandbox.Spec{})
	require.Error(t, err)

	created := runner.createdIDs()
	require.Len(t, created, 1)
	require.Equal(t, created, runner.destroyedIDs(), "a failed Dial must destroy the container Create just made")
}

func TestManager_Open_HandshakeFailure_DestroysOnError(t *testing.T) {
	runner := &fakeRunner{dialConn: func(string) (net.Conn, error) { return brokenAgentConn(t), nil }}
	m, _ := newTestManager(t, runner)

	openCtx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	_, err := m.Open(openCtx, sandbox.Spec{})
	require.Error(t, err)

	created := runner.createdIDs()
	require.Len(t, created, 1)
	require.Eventually(t, func() bool {
		return len(runner.destroyedIDs()) == 1
	}, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, created, runner.destroyedIDs())
}

func TestManager_Open_CreateFailure_NeverDials(t *testing.T) {
	runner := &fakeRunner{createErr: errors.New("create boom")}
	m, _ := newTestManager(t, runner)

	_, err := m.Open(t.Context(), sandbox.Spec{})
	require.Error(t, err)
	require.Empty(t, runner.createdIDs())
	require.Empty(t, runner.destroyedIDs())
}

func TestManager_RunLoop_SweepsOrphansFromOtherInstances(t *testing.T) {
	runner := &fakeRunner{
		sandboxes: []*sandbox.Sandbox{
			{ID: "same-instance", Labels: map[string]string{sandbox.LabelInstance: "self"}},
			{ID: "other-instance-1", Labels: map[string]string{sandbox.LabelInstance: "other"}},
			{ID: "other-instance-2", Labels: map[string]string{sandbox.LabelInstance: "other2"}},
		},
	}
	pool := session.NewPool(session.PoolOptions{Logger: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go pool.RunLoop(ctx)

	m := sandbox.NewManager(sandbox.ManagerOptions{
		Runner:   runner,
		Pool:     pool,
		Policy:   sandbox.Policy{},
		Instance: "self",
		Logger:   slog.New(slog.DiscardHandler),
	})

	runCtx, runCancel := context.WithCancel(ctx)
	go m.RunLoop(runCtx)
	t.Cleanup(runCancel)

	require.Eventually(t, func() bool {
		destroyed := runner.destroyedIDs()
		return len(destroyed) == 2
	}, 2*time.Second, 10*time.Millisecond)

	destroyed := runner.destroyedIDs()
	require.ElementsMatch(t, []string{"other-instance-1", "other-instance-2"}, destroyed)
}
