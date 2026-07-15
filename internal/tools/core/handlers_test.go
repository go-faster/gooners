package core

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/effect"
	"github.com/go-faster/gooners/internal/session"
)

type mockCorePool struct {
	open      func(context.Context, string) (session.OpenResult, error)
	openCfg   func(context.Context, session.Config) (session.OpenResult, error)
	close     func(context.Context, string) error
	list      func(context.Context) ([]session.SessionInfo, error)
	machine   func(context.Context, string) (string, error)
	exec      func(context.Context, session.ExecRequest) session.ExecResponse
	ping      func(context.Context, string) (time.Duration, error)
	openSpool func(context.Context, string, string) (effect.File, error)
	saveSpool func(context.Context, string, string, string) error
	// localFS is what the pool would let a tool touch. Nil means the zero
	// value a production pool defaults to: no host file access at all.
	localFS effect.FS
}

func (m mockCorePool) Open(ctx context.Context, machine string) (session.OpenResult, error) {
	return m.open(ctx, machine)
}

func (m mockCorePool) OpenCfg(ctx context.Context, cfg session.Config) (session.OpenResult, error) {
	return m.openCfg(ctx, cfg)
}

func (m mockCorePool) Close(ctx context.Context, id string) error {
	return m.close(ctx, id)
}

func (m mockCorePool) List(ctx context.Context) ([]session.SessionInfo, error) {
	return m.list(ctx)
}

func (m mockCorePool) Machine(ctx context.Context, id string) (string, error) {
	return m.machine(ctx, id)
}

func (m mockCorePool) Exec(ctx context.Context, r session.ExecRequest) session.ExecResponse {
	return m.exec(ctx, r)
}

func (m mockCorePool) CommandTimeout() time.Duration { return time.Second }

func (m mockCorePool) Ping(ctx context.Context, id string) (time.Duration, error) {
	return m.ping(ctx, id)
}

func (m mockCorePool) OpenSpool(ctx context.Context, sessionID, spoolID string) (effect.File, error) {
	return m.openSpool(ctx, sessionID, spoolID)
}

func (m mockCorePool) SaveSpool(ctx context.Context, sessionID, spoolID, localPath string) error {
	return m.saveSpool(ctx, sessionID, spoolID, localPath)
}

func (m mockCorePool) LocalFS() effect.FS {
	if m.localFS == nil {
		return effect.Deny("host file access is not configured for this server")
	}
	return m.localFS
}

type mockPasswordProvider struct {
	password string
	err      error
}

func (m mockPasswordProvider) Password(context.Context, string) (string, error) {
	return m.password, m.err
}

func TestOpenHandler_UsesPasswordProvider(t *testing.T) {
	p := mockCorePool{openCfg: func(_ context.Context, cfg session.Config) (session.OpenResult, error) {
		require.Equal(t, "host", cfg.Machine)
		require.Equal(t, "secret", cfg.Password)
		return session.OpenResult{ID: "session-1", UserAgent: "SSH", Banner: "banner", Platform: "linux"}, nil
	}}

	_, out, err := openHandler(p, mockPasswordProvider{password: "secret"}, slog.Default())(
		context.Background(), &mcp.CallToolRequest{}, openParams{Machine: "host"},
	)
	require.NoError(t, err)
	require.Equal(t, "session-1", out.SessionID)
	require.Equal(t, "linux", out.Platform)
}

func TestOpenCfgHandler(t *testing.T) {
	p := mockCorePool{openCfg: func(_ context.Context, cfg session.Config) (session.OpenResult, error) {
		require.Equal(t, "host", cfg.Machine)
		require.Equal(t, "me", cfg.User)
		require.Equal(t, 2222, cfg.Port)
		require.Equal(t, 3, cfg.TimeoutSec)
		return session.OpenResult{ID: "session-2"}, nil
	}}

	_, out, err := openCfgHandler(p, slog.Default())(context.Background(), &mcp.CallToolRequest{}, openCfgParams{
		Machine:    "host",
		User:       "me",
		Port:       2222,
		TimeoutSec: 3,
	})
	require.NoError(t, err)
	require.Equal(t, "session-2", out.SessionID)
}

func TestExecHandler_SudoPasswordProvider(t *testing.T) {
	p := mockCorePool{
		machine: func(_ context.Context, id string) (string, error) {
			require.Equal(t, "session-1", id)
			return "host", nil
		},
		exec: func(_ context.Context, r session.ExecRequest) session.ExecResponse {
			require.Equal(t, "session-1", r.SessionID)
			require.Equal(t, "id", r.Command)
			require.True(t, r.Sudo)
			require.Equal(t, "secret", r.SudoPassword)
			return session.ExecResponse{Stdout: "ok"}
		},
	}

	res, out, err := execHandler(p, true, mockPasswordProvider{password: "secret"}, slog.Default())(
		context.Background(), &mcp.CallToolRequest{}, execParams{SessionID: "session-1", Command: "id"},
	)
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Equal(t, "ok", out.Stdout)
}

func TestExecHandler_StdinFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "stdin.txt"), []byte("hello from file"), 0o600))

	p := mockCorePool{
		localFS: effect.Root(root),
		exec: func(_ context.Context, r session.ExecRequest) session.ExecResponse {
			require.Equal(t, "session-1", r.SessionID)
			require.Equal(t, "cat", r.Command)
			require.Equal(t, "hello from file", r.Stdin)
			return session.ExecResponse{Stdout: r.Stdin}
		},
	}

	_, out, err := execHandler(p, false, nil, slog.Default())(
		context.Background(), &mcp.CallToolRequest{},
		execParams{SessionID: "session-1", Command: "cat", StdinFile: filepath.Join(root, "stdin.txt")},
	)
	require.NoError(t, err)
	require.Equal(t, "hello from file", out.Stdout)
}

// TestExecHandler_StdinFileOutsideRoot: stdin_file reads through the pool's
// confined filesystem, so it reaches exactly what upload_file can and nothing
// else. The handler does no path check of its own.
func TestExecHandler_StdinFileOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))

	p := mockCorePool{localFS: effect.Root(root)}
	_, _, err := execHandler(p, false, nil, slog.Default())(
		context.Background(), &mcp.CallToolRequest{},
		execParams{SessionID: "session-1", Command: "cat", StdinFile: outside},
	)
	require.ErrorIs(t, err, effect.ErrOutsideRoot)
}

// TestExecHandler_StdinFileDeniedWithoutLocalFS: a server that grants no host
// file access (sandbox-mcp) refuses stdin_file, without having to remember to
// strip the parameter from the tool.
func TestExecHandler_StdinFileDeniedWithoutLocalFS(t *testing.T) {
	stdinPath := filepath.Join(t.TempDir(), "stdin.txt")
	require.NoError(t, os.WriteFile(stdinPath, []byte("secret"), 0o600))

	_, _, err := execHandler(mockCorePool{}, false, nil, slog.Default())(
		context.Background(), &mcp.CallToolRequest{},
		execParams{SessionID: "session-1", Command: "cat", StdinFile: stdinPath},
	)
	require.ErrorIs(t, err, effect.ErrDenied)
}

func TestExecHandler_StdinMutuallyExclusive(t *testing.T) {
	_, _, err := execHandler(mockCorePool{}, false, nil, slog.Default())(
		context.Background(), &mcp.CallToolRequest{}, execParams{
			SessionID: "session-1",
			Command:   "cat",
			Stdin:     "inline",
			StdinFile: filepath.Join(t.TempDir(), "stdin.txt"),
		},
	)
	require.ErrorContains(t, err, "mutually exclusive")
}

func TestOnceHandler_ClosesSession(t *testing.T) {
	closed := false
	p := mockCorePool{
		open: func(_ context.Context, machine string) (session.OpenResult, error) {
			require.Equal(t, "host", machine)
			return session.OpenResult{ID: "session-1"}, nil
		},
		exec: func(_ context.Context, r session.ExecRequest) session.ExecResponse {
			require.Equal(t, "session-1", r.SessionID)
			require.Equal(t, "uptime", r.Command)
			return session.ExecResponse{Stdout: "up"}
		},
		close: func(_ context.Context, id string) error {
			require.Equal(t, "session-1", id)
			closed = true
			return nil
		},
	}

	_, out, err := onceHandler(p)(context.Background(), &mcp.CallToolRequest{}, onceParams{Machine: "host", Command: "uptime"})
	require.NoError(t, err)
	require.Equal(t, "up", out.Stdout)
	require.True(t, closed)
}

func TestPingHandler(t *testing.T) {
	p := mockCorePool{ping: func(_ context.Context, id string) (time.Duration, error) {
		require.Equal(t, "session-1", id)
		return 5 * time.Millisecond, nil
	}}

	_, out, err := pingHandler(p)(context.Background(), &mcp.CallToolRequest{}, pingParams{SessionID: "session-1"})
	require.NoError(t, err)
	require.Equal(t, "ok", out.Status)
	require.Equal(t, "5ms", out.Time)
}

func TestReadOutputHandler(t *testing.T) {
	p := mockCorePool{openSpool: func(_ context.Context, sessionID, spoolID string) (effect.File, error) {
		require.Equal(t, "session-1", sessionID)
		require.Equal(t, "stdout", spoolID)
		return spoolFile(t, "a\nb\nc\n"), nil
	}}

	_, out, err := readOutputHandler(p)(context.Background(), &mcp.CallToolRequest{}, readOutputParams{
		SessionID: "session-1",
		SpoolID:   "stdout",
		Lines:     2,
		FromEnd:   true,
	})
	require.NoError(t, err)
	require.Equal(t, "... [Output truncated due to size/line limit] ...\nb\nc", out.Text)
}

// TestSaveOutputHandler_Delegates: the handler hands local_path to the pool
// untouched. Where that path may point is [session.Pool.SaveSpool]'s call, not
// this handler's — see TestPoolSaveSpoolConfinesDestination.
func TestSaveOutputHandler_Delegates(t *testing.T) {
	saved := false
	p := mockCorePool{
		saveSpool: func(_ context.Context, sessionID, spoolID, localPath string) error {
			require.Equal(t, "session-1", sessionID)
			require.Equal(t, "stdout", spoolID)
			require.Equal(t, "out/saved.txt", localPath)
			saved = true
			return nil
		},
	}

	_, out, err := saveOutputHandler(p)(context.Background(), &mcp.CallToolRequest{}, saveOutputParams{
		SessionID: "session-1",
		SpoolID:   "stdout",
		LocalPath: "out/saved.txt",
	})
	require.NoError(t, err)
	require.True(t, out.OK)
	require.True(t, saved)
}

// TestSaveOutputHandler_PropagatesDenial: when the pool refuses the
// destination, the tool call fails rather than reporting success.
func TestSaveOutputHandler_PropagatesDenial(t *testing.T) {
	p := mockCorePool{
		saveSpool: func(context.Context, string, string, string) error {
			return effect.ErrOutsideRoot
		},
	}

	_, out, err := saveOutputHandler(p)(context.Background(), &mcp.CallToolRequest{}, saveOutputParams{
		SessionID: "session-1",
		SpoolID:   "stdout",
		LocalPath: "../../escape.txt",
	})
	require.ErrorIs(t, err, effect.ErrOutsideRoot)
	require.False(t, out.OK)
}

func TestExecHandler_Validation(t *testing.T) {
	_, _, err := execHandler(mockCorePool{}, false, nil, slog.Default())(
		context.Background(), &mcp.CallToolRequest{}, execParams{SessionID: "session-1"},
	)
	require.Error(t, err)

	p := mockCorePool{machine: func(context.Context, string) (string, error) { return "host", nil }}
	_, _, err = execHandler(p, true, mockPasswordProvider{err: fmt.Errorf("vault down")}, slog.Default())(
		context.Background(), &mcp.CallToolRequest{}, execParams{SessionID: "session-1", Command: "id"},
	)
	require.Error(t, err)
}
