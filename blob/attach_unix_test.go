//go:build !windows

// Mkfifo has no Windows equivalent, so the irregular-file case lives here
// rather than being skipped at runtime in a file that would not compile.

package blob_test

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/blob"
)

// TestAttachRejectsIrregularFile: reading a fifo never ends, so attaching one
// would hold a connection open for as long as the client waits.
func TestAttachRejectsIrregularFile(t *testing.T) {
	s, _, _ := newStore(t, blob.HTTPOptions{})
	src, dir := mount(t, nil)
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600))

	_, err := s.Attach(t.Context(), src, "pipe", blob.PutOptions{})
	require.ErrorContains(t, err, "regular file")
}
