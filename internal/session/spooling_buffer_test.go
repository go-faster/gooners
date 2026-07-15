package session

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/effect"
)

func TestSpoolingBuffer(t *testing.T) {
	t.Run("WithinThreshold", func(t *testing.T) {
		buf := NewSpoolingBuffer(effect.Root(t.TempDir()), "test-sess-1", 10)
		require.NotEmpty(t, buf.SpoolID())
		require.False(t, buf.Spilled())
		require.Zero(t, buf.Size())

		n, err := buf.Write([]byte("hello"))
		require.NoError(t, err)
		require.Equal(t, 5, n)
		require.Equal(t, int64(5), buf.Size())
		require.False(t, buf.Spilled())
		require.Equal(t, "hello", buf.String())

		err = buf.Close()
		require.NoError(t, err)
	})

	t.Run("SpillOver", func(t *testing.T) {
		spool := effect.Root(t.TempDir())
		buf := NewSpoolingBuffer(spool, "test-sess-2", 10)
		require.False(t, buf.Spilled())

		// Write 5 bytes (fits)
		n, err := buf.Write([]byte("hello"))
		require.NoError(t, err)
		require.Equal(t, 5, n)
		require.False(t, buf.Spilled())

		// Write 6 bytes (exceeds threshold of 10)
		n, err = buf.Write([]byte(" world"))
		require.NoError(t, err)
		require.Equal(t, 6, n)
		require.True(t, buf.Spilled())
		require.NotEmpty(t, buf.FilePath())
		require.Equal(t, int64(11), buf.Size())

		// String should return the contents buffered up to the moment it spilled (which is "hello")
		require.Equal(t, "hello", buf.String())

		// The spool file is reachable only through the buffer's own FS: its
		// path is relative to that root, not a host path.
		data, err := spool.ReadFile(buf.FilePath())
		require.NoError(t, err)
		require.Equal(t, "hello world", string(data))

		err = buf.Close()
		require.NoError(t, err)

		require.NoError(t, spool.Remove(buf.FilePath()))
	})

	t.Run("FailFast", func(t *testing.T) {
		buf := NewSpoolingBuffer(effect.Root(t.TempDir()), "test-sess-3", 10)
		// We set error early to simulate failure
		buf.err = os.ErrPermission
		n, err := buf.Write([]byte("hello"))
		require.ErrorIs(t, err, os.ErrPermission)
		require.Zero(t, n)
	})

	// A pool with no host file access still spools: the spool FS is separate
	// from the tools' LocalFS precisely so that denying the latter does not
	// break command output.
	t.Run("SpoolsWhenLocalFSDenied", func(t *testing.T) {
		spool := effect.Root(t.TempDir())
		buf := NewSpoolingBuffer(spool, "test-sess-4", 4)

		_, err := buf.Write([]byte("hello world"))
		require.NoError(t, err)
		require.True(t, buf.Spilled())
		require.NoError(t, buf.Close())

		data, err := spool.ReadFile(buf.FilePath())
		require.NoError(t, err)
		require.Equal(t, "hello world", string(data))
	})
}
