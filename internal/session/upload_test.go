package session

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	// transferFileSize is large enough that the transfer is still in flight when the test
	// interferes: it dwarfs everything the SFTP and SSH windows can buffer ahead.
	transferFileSize = 32 << 20
	// interfereAfter is how much of the transfer the server accepts before the connection
	// is stalled or killed. Well past the SSH handshake, nowhere near the end.
	interfereAfter = 256 << 10

	// waitBudget bounds every "must not hang" assertion. The real thing hung for 30
	// minutes, so anything in this ballpark is a pass.
	waitBudget = 30 * time.Second
)

// disconnectLog captures Pool.OnDisconnect notifications.
type disconnectLog struct {
	ch chan error
}

func newDisconnectLog() *disconnectLog {
	return &disconnectLog{ch: make(chan error, 8)}
}

func (d *disconnectLog) add(_ string, err error) {
	select {
	case d.ch <- err:
	default:
	}
}

// wait returns the next disconnect notification, failing the test if none arrives.
func (d *disconnectLog) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-d.ch:
		return err
	case <-time.After(waitBudget):
		t.Fatal("no disconnect notification")
		return nil
	}
}

// startTransferPool opens a pool with a fast keepalive, plus a session to srv.
func startTransferPool(t *testing.T, srv *sftpTestServer, dl *disconnectLog) (pool *Pool, sessionID string) {
	t.Helper()

	p := NewPool(PoolOptions{
		Logger: slog.New(slog.DiscardHandler),
		// Small enough to keep the test quick, large enough not to trip on a busy runner.
		KeepaliveInterval: 100 * time.Millisecond,
		KeepaliveTimeout:  500 * time.Millisecond,
		OnDisconnect:      dl.add,
	})
	go p.RunLoop(t.Context())

	res, err := p.OpenCfg(t.Context(), Config{
		Machine:    srv.addr,
		User:       "test",
		KnownHosts: "insecure",
		HomeDir:    t.TempDir(),
	})
	require.NoError(t, err)
	return p, res.ID
}

// newSparseFile creates a size-byte file without writing size bytes of data.
func newSparseFile(t *testing.T, size int64) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "src.bin")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	require.NoError(t, f.Close())
	return path
}

func skipUnsupported(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("SFTP test server uses POSIX absolute paths; unsupported on Windows")
	}
}

// TestUploadConnectionKilledMidTransfer covers a connection reset while bytes are still
// flowing: the upload must land in a terminal failed state instead of stalling at a
// fixed byte count forever.
func TestUploadConnectionKilledMidTransfer(t *testing.T) {
	skipUnsupported(t)

	srv := newSFTPTestServer(t)
	srv.afterBytes(interfereAfter, (*controlConn).Kill)

	dl := newDisconnectLog()
	p, sessionID := startTransferPool(t, srv, dl)

	local := newSparseFile(t, transferFileSize)
	remote := filepath.Join(srv.root, "out.bin")

	uploadID, err := p.Upload(t.Context(), sessionID, local, remote)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), waitBudget)
	defer cancel()

	st, err := p.UploadWait(ctx, sessionID, uploadID)
	require.Error(t, err, "a killed upload must report an error")
	require.NotErrorIs(t, err, context.DeadlineExceeded, "upload_wait must resolve on its own, not time out")
	require.True(t, st.Done)
	require.Equal(t, TransferFailed, st.Status)
	require.Less(t, st.BytesUploaded, st.TotalBytes, "upload must not claim more progress than it made")

	// The watcher reaps the session, and the client is told why.
	require.Error(t, dl.wait(t))

	// The job outlives its session: polling it must yield the failure, not the old
	// "session not found", which told the caller nothing about the upload.
	after, err := p.UploadStatus(t.Context(), sessionID, uploadID)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "session not found")
	require.Equal(t, TransferFailed, after.Status)
	require.Equal(t, uploadID, after.UploadID)
}

// TestUploadConnectionStalledMidTransfer covers the reported failure: the link goes
// silent, with no FIN and no RST. Nothing errors on its own, so the pool has to notice
// via a bounded keepalive and fail the upload itself.
func TestUploadConnectionStalledMidTransfer(t *testing.T) {
	skipUnsupported(t)

	srv := newSFTPTestServer(t)
	srv.afterBytes(interfereAfter, (*controlConn).Stall)

	dl := newDisconnectLog()
	p, sessionID := startTransferPool(t, srv, dl)

	local := newSparseFile(t, transferFileSize)
	remote := filepath.Join(srv.root, "out.bin")

	uploadID, err := p.Upload(t.Context(), sessionID, local, remote)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), waitBudget)
	defer cancel()

	st, err := p.UploadWait(ctx, sessionID, uploadID)
	require.Error(t, err, "a stalled upload must report an error, not hang")
	require.NotErrorIs(t, err, context.DeadlineExceeded, "upload_wait must resolve on its own, not time out")
	require.True(t, st.Done)
	require.Equal(t, TransferFailed, st.Status)
	require.Contains(t, err.Error(), "keepalive", "the failure must name the stalled connection as the cause")
	require.Less(t, st.BytesUploaded, st.TotalBytes)

	require.ErrorContains(t, dl.wait(t), "keepalive")
}

// TestUploadCancelWhileStalled covers canceling an upload whose connection has gone
// silent. Cancelation alone cannot unblock the SFTP write path, so this only terminates
// if the pool tears the connection down too.
func TestUploadCancelWhileStalled(t *testing.T) {
	skipUnsupported(t)

	srv := newSFTPTestServer(t)
	stalled := make(chan struct{})
	srv.afterBytes(interfereAfter, func(c *controlConn) {
		c.Stall()
		close(stalled)
	})

	dl := newDisconnectLog()
	p, sessionID := startTransferPool(t, srv, dl)

	local := newSparseFile(t, transferFileSize)
	remote := filepath.Join(srv.root, "out.bin")

	uploadID, err := p.Upload(t.Context(), sessionID, local, remote)
	require.NoError(t, err)

	select {
	case <-stalled:
	case <-time.After(waitBudget):
		t.Fatal("upload never reached the stall point")
	}

	ctx, cancel := context.WithTimeout(t.Context(), waitBudget)
	defer cancel()

	st, err := p.UploadCancel(ctx, sessionID, uploadID)
	require.Error(t, err)
	require.NotErrorIs(t, err, context.DeadlineExceeded, "upload_cancel must resolve on its own, not time out")
	require.True(t, st.Done)
	// The caller asked for the cancel first, so that is what the job reports — the
	// connection teardown that follows must not relabel it as a failure.
	require.Equal(t, TransferCanceled, st.Status)
	require.ErrorIs(t, err, ErrTransferCanceled)
}

// TestUploadCompletedThenConnectionKilled covers a transfer that finishes and only then
// loses its connection. The success must survive the session it was made on.
func TestUploadCompletedThenConnectionKilled(t *testing.T) {
	skipUnsupported(t)

	const size = 1 << 20

	srv := newSFTPTestServer(t)
	dl := newDisconnectLog()
	p, sessionID := startTransferPool(t, srv, dl)

	local := newSparseFile(t, size)
	remote := filepath.Join(srv.root, "out.bin")

	uploadID, err := p.Upload(t.Context(), sessionID, local, remote)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), waitBudget)
	defer cancel()

	st, err := p.UploadWait(ctx, sessionID, uploadID)
	require.NoError(t, err)
	require.Equal(t, TransferCompleted, st.Status)
	require.Equal(t, int64(size), st.BytesUploaded)
	require.InDelta(t, 100.0, st.Percent, 0.001)

	uploaded, err := os.Stat(remote)
	require.NoError(t, err)
	require.Equal(t, int64(size), uploaded.Size())

	// Drop the connection after the fact; the session goes away with it.
	srv.killAll()
	require.Error(t, dl.wait(t))

	after, err := p.UploadStatus(t.Context(), sessionID, uploadID)
	require.NoError(t, err, "a finished upload must stay queryable after its session dies")
	require.Equal(t, TransferCompleted, after.Status)
	require.True(t, after.Done)
	require.Equal(t, int64(size), after.BytesUploaded)
}

// TestDownloadConnectionKilledMidTransfer mirrors the upload case.
func TestDownloadConnectionKilledMidTransfer(t *testing.T) {
	skipUnsupported(t)

	srv := newSFTPTestServer(t)

	remote := filepath.Join(srv.root, "src.bin")
	f, err := os.Create(remote)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(transferFileSize))
	require.NoError(t, f.Close())

	// Downloads flow the other way, so gate on what the server sends, not what it reads.
	srv.afterBytes(1, func(c *controlConn) {
		go func() {
			// Let some of the file through before pulling the plug.
			time.Sleep(50 * time.Millisecond)
			c.Kill()
		}()
	})

	dl := newDisconnectLog()
	p, sessionID := startTransferPool(t, srv, dl)

	local := filepath.Join(t.TempDir(), "out.bin")
	downloadID, err := p.Download(t.Context(), sessionID, remote, local)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), waitBudget)
	defer cancel()

	st, err := p.DownloadWait(ctx, sessionID, downloadID)
	require.Error(t, err)
	require.NotErrorIs(t, err, context.DeadlineExceeded, "download_wait must resolve on its own, not time out")
	require.True(t, st.Done)
	require.Equal(t, TransferFailed, st.Status)

	// A failed download must not leave a truncated file behind.
	_, err = os.Stat(local)
	require.ErrorIs(t, err, os.ErrNotExist)

	after, err := p.DownloadStatus(t.Context(), sessionID, downloadID)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "session not found")
	require.Equal(t, TransferFailed, after.Status)
}
