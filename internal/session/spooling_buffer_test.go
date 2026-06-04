package session

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSpoolingBuffer(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	t.Run("within threshold", func(t *testing.T) {
		buf := NewSpoolingBuffer("test-sess-1", 10)
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

	t.Run("spill over", func(t *testing.T) {
		buf := NewSpoolingBuffer("test-sess-2", 10)
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

		// Read the file to ensure it has full content
		data, err := os.ReadFile(buf.FilePath())
		require.NoError(t, err)
		require.Equal(t, "hello world", string(data))

		err = buf.Close()
		require.NoError(t, err)

		// Clean up the spooled file
		err = os.Remove(buf.FilePath())
		require.NoError(t, err)
	})

	t.Run("fail fast", func(t *testing.T) {
		buf := NewSpoolingBuffer("test-sess-3", 10)
		// We set error early to simulate failure
		buf.err = os.ErrPermission
		n, err := buf.Write([]byte("hello"))
		require.ErrorIs(t, err, os.ErrPermission)
		require.Zero(t, n)
	})
}
