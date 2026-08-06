package blob_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/blob"
)

// mount writes files into a directory and returns an FS confined to it, the
// way a gateway sees a volume another process wrote.
func mount(t *testing.T, files map[string]string) (fs blob.FS, dir string) {
	t.Helper()

	dir = t.TempDir()
	for name, content := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	return blob.Dir(dir), dir
}

func TestAttach(t *testing.T) {
	ctx := t.Context()
	s, _, srv := newStore(t, blob.HTTPOptions{})
	src, dir := mount(t, map[string]string{"out.png": "image-bytes"})

	b, err := s.Attach(ctx, src, "out.png", blob.PutOptions{})
	require.NoError(t, err)
	require.Equal(t, "out.png", b.Name)
	require.Equal(t, "image/png", b.MIMEType, "guessed from the name")
	require.Equal(t, int64(len("image-bytes")), b.Size)
	require.Equal(t, "http://blob.invalid/files/"+b.ID+"/out.png", b.URL)

	resp := fetch(t, srv, b.URL, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "image-bytes", string(body))

	// Nothing was copied into the store's own directory.
	require.FileExists(t, filepath.Join(dir, "out.png"))
}

// TestAttachDoesNotOwnTheBytes is the invariant a shared volume depends on:
// the reference expires, the file does not.
func TestAttachDoesNotOwnTheBytes(t *testing.T) {
	ctx := t.Context()
	s, c, srv := newStore(t, blob.HTTPOptions{TTL: time.Minute})
	src, dir := mount(t, map[string]string{"out.bin": "upstream-data"})

	b, err := s.Attach(ctx, src, "out.bin", blob.PutOptions{})
	require.NoError(t, err)

	for _, tt := range []struct {
		name string
		drop func()
	}{
		{name: "Expiry", drop: func() { c.advance(2 * time.Minute) }},
		{name: "Delete", drop: func() { require.NoError(t, s.Delete(ctx, b.ID)) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Re-attach for each case; the previous subtest dropped the last one.
			b, err = s.Attach(ctx, src, "out.bin", blob.PutOptions{})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, fetch(t, srv, b.URL, nil).StatusCode)

			tt.drop()

			require.Equal(t, http.StatusNotFound, fetch(t, srv, b.URL, nil).StatusCode)

			got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
			require.NoError(t, err, "the file belongs to whoever wrote it")
			require.Equal(t, "upstream-data", string(got))
		})
	}
}

// TestAttachShutdownKeepsFiles: a gateway restart must not empty the volume it
// was serving.
func TestAttachShutdownKeepsFiles(t *testing.T) {
	s, _, _ := newStore(t, blob.HTTPOptions{})
	src, dir := mount(t, map[string]string{"keep.txt": "still here"})

	_, err := s.Attach(t.Context(), src, "keep.txt", blob.PutOptions{})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	cancel()
	<-done

	got, err := os.ReadFile(filepath.Join(dir, "keep.txt"))
	require.NoError(t, err)
	require.Equal(t, "still here", string(got))
}

func TestAttachRejects(t *testing.T) {
	ctx := t.Context()
	src, dir := mount(t, map[string]string{"sub/file.txt": "x", "big.bin": "0123456789"})

	t.Run("MissingFile", func(t *testing.T) {
		s, _, _ := newStore(t, blob.HTTPOptions{})
		_, err := s.Attach(ctx, src, "nope.txt", blob.PutOptions{})
		require.Error(t, err)
	})

	t.Run("Directory", func(t *testing.T) {
		s, _, _ := newStore(t, blob.HTTPOptions{})
		_, err := s.Attach(ctx, src, "sub", blob.PutOptions{})
		require.ErrorContains(t, err, "directory")
	})

	// Nothing outside the mount is reachable, which is what makes the mount the
	// boundary rather than the caller's good behavior.
	t.Run("OutsideTheMount", func(t *testing.T) {
		s, _, _ := newStore(t, blob.HTTPOptions{})
		for _, name := range []string{"../escape.txt", "/etc/passwd"} {
			_, err := s.Attach(ctx, src, name, blob.PutOptions{})
			require.Error(t, err, name)
		}
	})

	// A symlink planted inside the mount does not lead out of it either.
	t.Run("SymlinkOutOfTheMount", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation needs privileges on Windows")
		}
		outside := filepath.Join(t.TempDir(), "secret.txt")
		require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))
		require.NoError(t, os.Symlink(outside, filepath.Join(dir, "link.txt")))

		s, _, _ := newStore(t, blob.HTTPOptions{})
		_, err := s.Attach(ctx, src, "link.txt", blob.PutOptions{})
		require.Error(t, err)
	})

	t.Run("OverMaxSize", func(t *testing.T) {
		s, _, _ := newStore(t, blob.HTTPOptions{MaxSize: 4})
		_, err := s.Attach(ctx, src, "big.bin", blob.PutOptions{})
		require.ErrorIs(t, err, blob.ErrTooLarge)
	})

	t.Run("NoSourceFS", func(t *testing.T) {
		s, _, _ := newStore(t, blob.HTTPOptions{})
		_, err := s.Attach(ctx, nil, "x", blob.PutOptions{})
		require.Error(t, err)
	})
}

// TestAttachManyMounts: one store serves several directories, each confined to
// itself, which is what lets a gateway carry one listener for every upstream.
func TestAttachManyMounts(t *testing.T) {
	ctx := t.Context()
	s, _, srv := newStore(t, blob.HTTPOptions{})
	first, _ := mount(t, map[string]string{"a.txt": "from-first"})
	second, _ := mount(t, map[string]string{"a.txt": "from-second"})

	for _, tt := range []struct {
		src  blob.FS
		want string
	}{
		{src: first, want: "from-first"},
		{src: second, want: "from-second"},
	} {
		b, err := s.Attach(ctx, tt.src, "a.txt", blob.PutOptions{})
		require.NoError(t, err)

		resp := fetch(t, srv, b.URL, nil)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, tt.want, string(body))
	}
}

// TestAttachOverridesName: the served name need not be the file's, so a
// generated path does not become the agent's filename.
func TestAttachOverridesName(t *testing.T) {
	s, _, _ := newStore(t, blob.HTTPOptions{})
	src, _ := mount(t, map[string]string{"tmp1234.dat": "x"})

	b, err := s.Attach(t.Context(), src, "tmp1234.dat", blob.PutOptions{
		Name:     "panel.png",
		MIMEType: "image/png",
	})
	require.NoError(t, err)
	require.Equal(t, "panel.png", b.Name)
	require.Equal(t, "image/png", b.MIMEType)
}
