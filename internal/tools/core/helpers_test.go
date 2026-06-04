package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadHead(t *testing.T) {
	t.Run("empty file", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "empty.txt")
		require.NoError(t, os.WriteFile(tmp, nil, 0o600))

		res, err := readHead(tmp, 10, 100)
		require.NoError(t, err)
		require.Equal(t, "", res)
	})

	t.Run("no trailing newline", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "none.txt")
		require.NoError(t, os.WriteFile(tmp, []byte("hello\nworld"), 0o600))

		res, err := readHead(tmp, 10, 100)
		require.NoError(t, err)
		require.Equal(t, "hello\nworld", res)
	})

	t.Run("exactly threshold lines", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "exact.txt")
		require.NoError(t, os.WriteFile(tmp, []byte("l1\nl2\nl3\n"), 0o600))

		res, err := readHead(tmp, 3, 100)
		require.NoError(t, err)
		require.Equal(t, "l1\nl2\nl3\n", res)
	})

	t.Run("maxLines hit before EOF", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "max_lines.txt")
		require.NoError(t, os.WriteFile(tmp, []byte("l1\nl2\nl3\nl4\n"), 0o600))

		res, err := readHead(tmp, 2, 100)
		require.NoError(t, err)
		require.Equal(t, "l1\nl2\n\n... [Output truncated due to size/line limit] ...", res)
	})

	t.Run("maxBytes hit before EOF", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "max_bytes.txt")
		require.NoError(t, os.WriteFile(tmp, []byte("l1\nl2\nl3\n"), 0o600))

		res, err := readHead(tmp, 10, 4)
		require.NoError(t, err)
		require.Equal(t, "l1\nl\n... [Output truncated due to size/line limit] ...", res)
	})
}

func TestReadTail(t *testing.T) {
	t.Run("empty file", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "empty.txt")
		require.NoError(t, os.WriteFile(tmp, nil, 0o600))

		res, err := readTail(tmp, 10, 100)
		require.NoError(t, err)
		require.Equal(t, "", res)
	})

	t.Run("no trailing newline", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "none.txt")
		require.NoError(t, os.WriteFile(tmp, []byte("hello\nworld"), 0o600))

		res, err := readTail(tmp, 10, 100)
		require.NoError(t, err)
		require.Equal(t, "hello\nworld", res)
	})

	t.Run("maxLines hit before EOF", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "max_lines.txt")
		require.NoError(t, os.WriteFile(tmp, []byte("l1\nl2\nl3\nl4\n"), 0o600))

		res, err := readTail(tmp, 2, 100)
		require.NoError(t, err)
		require.Equal(t, "... [Output truncated due to size/line limit] ...\nl3\nl4", res)
	})

	t.Run("maxBytes hit before EOF", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "max_bytes.txt")
		require.NoError(t, os.WriteFile(tmp, []byte("l1\nl2\nl3\n"), 0o600))

		res, err := readTail(tmp, 10, 4)
		require.NoError(t, err)
		require.Equal(t, "... [Output truncated due to size/line limit] ...\nl3", res)
	})
}

func TestRenameOrCopy(t *testing.T) {
	t.Run("same filesystem rename", func(t *testing.T) {
		tmpDir := t.TempDir()
		src := filepath.Join(tmpDir, "src.txt")
		dst := filepath.Join(tmpDir, "dst.txt")
		require.NoError(t, os.WriteFile(src, []byte("hello"), 0o600))

		require.NoError(t, renameOrCopy(src, dst))

		_, err := os.Stat(src)
		require.True(t, os.IsNotExist(err))

		data, err := os.ReadFile(dst)
		require.NoError(t, err)
		require.Equal(t, "hello", string(data))

		stat, err := os.Stat(dst)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), stat.Mode().Perm())
	})

	t.Run("copy fallback", func(t *testing.T) {
		tmpDir := t.TempDir()
		src := filepath.Join(tmpDir, "src.txt")
		dst := filepath.Join(tmpDir, "nested", "dst.txt")
		require.NoError(t, os.WriteFile(src, []byte("hello fallback"), 0o600))

		require.NoError(t, renameOrCopy(src, dst))

		_, err := os.Stat(src)
		require.True(t, os.IsNotExist(err))

		data, err := os.ReadFile(dst)
		require.NoError(t, err)
		require.Equal(t, "hello fallback", string(data))

		stat, err := os.Stat(dst)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), stat.Mode().Perm())
	})
}
