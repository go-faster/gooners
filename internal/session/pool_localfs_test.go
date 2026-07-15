package session

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/effect"
)

// newSpoolPool starts a pool with a real spool FS and the given LocalFS, and an
// adopted session holding one registered spool file.
func newSpoolPool(t *testing.T, localFS effect.FS, content string) (pool *Pool, sessionID, spoolID string) {
	t.Helper()

	spoolFS := effect.Root(t.TempDir())
	p := NewPool(PoolOptions{
		Logger:  slog.New(slog.DiscardHandler),
		LocalFS: localFS,
		SpoolFS: spoolFS,
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.RunLoop(ctx)

	srv := newTestServer(t)
	res, err := p.OpenCfg(ctx, dialInsecure(t, srv.addr))
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close(context.Background(), res.ID) })

	spoolID = "stdout"
	spoolPath := filepath.Join(res.ID, spoolID+".out")
	require.NoError(t, spoolFS.MkdirAll(res.ID, 0o700))
	require.NoError(t, spoolFS.WriteFile(spoolPath, []byte(content), 0o600))
	require.NoError(t, p.RegisterSpool(ctx, res.ID, spoolID, spoolPath))

	return p, res.ID, spoolID
}

// TestPoolSaveSpoolConfinesDestination is the regression test for the
// arbitrary host write that motivated the effect providers: ssh_save_output
// wrote attacker-controlled spool content to any path the process could reach,
// because confinement was the tool's job and that tool never did it.
//
// The destination is now confined by the pool's LocalFS, so the tool cannot
// reintroduce the bug by omission — it has no path check to omit.
func TestPoolSaveSpoolConfinesDestination(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	p, sessionID, spoolID := newSpoolPool(t, effect.Root(root), "spool content")

	escape := filepath.Join(outside, "authorized_keys")
	err := p.SaveSpool(context.Background(), sessionID, spoolID, escape)
	require.ErrorIs(t, err, effect.ErrOutsideRoot)
	require.NoFileExists(t, escape)

	// Traversal out of the root, spelled relatively.
	err = p.SaveSpool(context.Background(), sessionID, spoolID, filepath.Join("..", "..", "escape.txt"))
	require.ErrorIs(t, err, effect.ErrOutsideRoot)

	// A destination inside the root still works.
	require.NoError(t, p.SaveSpool(context.Background(), sessionID, spoolID, "out/saved.txt"))
	data, err := os.ReadFile(filepath.Join(root, "out", "saved.txt"))
	require.NoError(t, err)
	require.Equal(t, "spool content", string(data))
}

// TestPoolSaveSpoolDeniedWithoutLocalFS: a pool with no LocalFS (sandbox-mcp)
// refuses to write a host file at all, whatever it is asked for.
func TestPoolSaveSpoolDeniedWithoutLocalFS(t *testing.T) {
	p, sessionID, spoolID := newSpoolPool(t, nil, "spool content")

	dst := filepath.Join(t.TempDir(), "saved.txt")
	err := p.SaveSpool(context.Background(), sessionID, spoolID, dst)
	require.ErrorIs(t, err, effect.ErrDenied)
	require.NoFileExists(t, dst)
}

// TestPoolTransferConfinesLocalPath: upload's source and download's
// destination are confined by the same LocalFS, and rejected up front rather
// than as a silent async job failure.
func TestPoolTransferConfinesLocalPath(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))

	p, sessionID, _ := newSpoolPool(t, effect.Root(root), "")
	ctx := context.Background()

	_, err := p.Upload(ctx, sessionID, outside, "/tmp/remote.txt")
	require.ErrorIs(t, err, effect.ErrOutsideRoot)

	_, err = p.Download(ctx, sessionID, "/tmp/remote.txt", outside)
	require.ErrorIs(t, err, effect.ErrOutsideRoot)
	require.Equal(t, "secret", readFile(t, outside), "download must not have overwritten the file")
}

// TestPoolTransferDeniedWithoutLocalFS: without a LocalFS, file transfer is
// refused outright rather than reaching the host filesystem.
func TestPoolTransferDeniedWithoutLocalFS(t *testing.T) {
	p, sessionID, _ := newSpoolPool(t, nil, "")
	ctx := context.Background()

	_, err := p.Upload(ctx, sessionID, filepath.Join(t.TempDir(), "src.txt"), "/tmp/remote.txt")
	require.ErrorIs(t, err, effect.ErrDenied)

	_, err = p.Download(ctx, sessionID, "/tmp/remote.txt", filepath.Join(t.TempDir(), "dst.txt"))
	require.ErrorIs(t, err, effect.ErrDenied)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// TestPoolOpenSpoolReadsThroughSpoolFS: reading spooled output does not depend
// on LocalFS, so a server that grants no host file access can still return
// truncated command output.
func TestPoolOpenSpoolReadsThroughSpoolFS(t *testing.T) {
	p, sessionID, spoolID := newSpoolPool(t, nil, "spool content")

	f, err := p.OpenSpool(context.Background(), sessionID, spoolID)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	data := make([]byte, len("spool content"))
	_, err = f.Read(data)
	require.NoError(t, err)
	require.Equal(t, "spool content", string(data))
}
