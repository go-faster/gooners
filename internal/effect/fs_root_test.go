package effect_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/effect"
)

func TestRootConfinement(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600))

	tests := []struct {
		name string
		path string
	}{
		{"parent traversal", filepath.Join("..", "secret.txt")},
		{"deep traversal", filepath.Join("a", "..", "..", "secret.txt")},
		{"absolute outside", filepath.Join(outside, "secret.txt")},
		{"root itself as parent", ".."},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := effect.Root(root)

			_, err := fsys.Open(tt.path)
			require.ErrorIs(t, err, effect.ErrOutsideRoot)

			_, err = fsys.Create(tt.path)
			require.ErrorIs(t, err, effect.ErrOutsideRoot)

			require.ErrorIs(t, fsys.WriteFile(tt.path, []byte("pwned"), 0o600), effect.ErrOutsideRoot)
			require.ErrorIs(t, fsys.Remove(tt.path), effect.ErrOutsideRoot)
			require.ErrorIs(t, fsys.RemoveAll(tt.path), effect.ErrOutsideRoot)
			require.ErrorIs(t, fsys.Rename("inside.txt", tt.path), effect.ErrOutsideRoot)
		})
	}

	// Nothing above may have touched the file outside the root.
	data, err := os.ReadFile(filepath.Join(outside, "secret.txt"))
	require.NoError(t, err)
	require.Equal(t, "secret", string(data))
}

// TestRootSymlinkEscape is the case a lexical path check cannot catch: the
// path stays inside the root, but a symlink inside the root points out of it.
// [os.Root] refuses to follow it; the WithinDir check this package replaced
// would have written straight through.
func TestRootSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation on Windows requires elevated privileges")
	}
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "authorized_keys")
	require.NoError(t, os.WriteFile(target, []byte("original"), 0o600))

	// An "escape hatch" symlink inside the root, as an attacker who can create
	// files in the root (e.g. via download_file) would plant it.
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "hatch")))

	fsys := effect.Root(root)
	escape := filepath.Join("hatch", "authorized_keys")

	err := fsys.WriteFile(escape, []byte("pwned"), 0o600)
	require.Error(t, err)

	_, err = fsys.Create(escape)
	require.Error(t, err)

	_, err = fsys.ReadFile(escape)
	require.Error(t, err)

	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "original", string(data), "symlinked write escaped the root")
}

func TestRootAllowsPathsInside(t *testing.T) {
	root := t.TempDir()
	fsys := effect.Root(root)

	require.NoError(t, fsys.MkdirAll(filepath.Join("nested", "dir"), 0o700))
	rel := filepath.Join("nested", "dir", "file.txt")
	require.NoError(t, fsys.WriteFile(rel, []byte("hello"), 0o600))

	data, err := fsys.ReadFile(rel)
	require.NoError(t, err)
	require.Equal(t, "hello", string(data))

	// An absolute path inside the root resolves the same as the relative one.
	data, err = fsys.ReadFile(filepath.Join(root, rel))
	require.NoError(t, err)
	require.Equal(t, "hello", string(data))

	info, err := fsys.Stat(rel)
	require.NoError(t, err)
	require.Equal(t, int64(5), info.Size())

	require.NoError(t, fsys.Rename(rel, filepath.Join("nested", "moved.txt")))
	_, err = fsys.Stat(rel)
	require.Error(t, err)

	f, err := fsys.Open(filepath.Join("nested", "moved.txt"))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	require.NoError(t, fsys.Remove(filepath.Join("nested", "moved.txt")))
	require.NoError(t, fsys.RemoveAll("nested"))
}

func TestRootMissingDirIsCreatedLazily(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not", "yet", "there")
	fsys := effect.Root(dir)

	require.NoError(t, fsys.WriteFile("f.txt", []byte("x"), 0o600))
	require.FileExists(t, filepath.Join(dir, "f.txt"))
}

func TestDeny(t *testing.T) {
	fsys := effect.Deny("file transfer is not configured")

	_, err := fsys.Open("anything")
	require.ErrorIs(t, err, effect.ErrDenied)
	require.Contains(t, err.Error(), "file transfer is not configured")

	_, err = fsys.Create("anything")
	require.ErrorIs(t, err, effect.ErrDenied)
	_, err = fsys.ReadFile("anything")
	require.ErrorIs(t, err, effect.ErrDenied)
	_, err = fsys.Stat("anything")
	require.ErrorIs(t, err, effect.ErrDenied)
	require.ErrorIs(t, fsys.WriteFile("a", nil, 0o600), effect.ErrDenied)
	require.ErrorIs(t, fsys.MkdirAll("a", 0o700), effect.ErrDenied)
	require.ErrorIs(t, fsys.Remove("a"), effect.ErrDenied)
	require.ErrorIs(t, fsys.RemoveAll("a"), effect.ErrDenied)
	require.ErrorIs(t, fsys.Rename("a", "b"), effect.ErrDenied)
}

// FuzzRootEscape asserts the invariant the whole package exists for: whatever
// path arrives from a tool call, a confined FS either refuses it or acts
// strictly inside its root. Nothing it writes may land outside.
func FuzzRootEscape(f *testing.F) {
	for _, seed := range []string{
		"file.txt",
		"../escape",
		"../../escape",
		"/etc/passwd",
		"a/../../b",
		"./../x",
		"",
		".",
		"..",
		"hatch/authorized_keys",
		"nested/./../../out",
		`..\windows`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, path string) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation on Windows requires elevated privileges")
		}
		root := t.TempDir()
		outside := t.TempDir()
		canary := filepath.Join(outside, "canary")
		require.NoError(t, os.WriteFile(canary, []byte("canary"), 0o600))
		// Symlinks inside the root pointing out of it: the FS must not follow
		// either one, whatever path the fuzzer hands it.
		require.NoError(t, os.Symlink(outside, filepath.Join(root, "hatch")))
		require.NoError(t, os.Symlink("/", filepath.Join(root, "slash")))

		fsys := effect.Root(root)
		if err := fsys.WriteFile(path, []byte("pwned"), 0o600); err == nil {
			// The write was allowed, so its target must have landed inside the
			// root once every symlink on the way is resolved.
			abs := path
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(root, path)
			}
			resolved, err := filepath.EvalSymlinks(abs)
			require.NoError(t, err)
			require.True(t, isInside(t, root, resolved),
				"write to %q landed outside the root, at %q", path, resolved)
		}

		data, err := os.ReadFile(canary)
		require.NoError(t, err)
		require.Equal(t, "canary", string(data), "path %q escaped the root", path)
	})
}

// isInside reports whether path is root or lives under it, with symlinks in
// root itself (macOS /var -> /private/var) resolved on both sides.
func isInside(t *testing.T, root, path string) bool {
	t.Helper()
	root, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
