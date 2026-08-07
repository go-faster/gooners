package fs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/blob"
	"github.com/go-faster/gooners/internal/effect"
)

// blobStore builds a store holding one object, standing in for a file another
// server produced and this one has no filesystem access to.
func blobStore(t *testing.T, name, content string) (blob.Store, blob.Blob) {
	t.Helper()

	store, err := blob.NewHTTP(blob.HTTPOptions{
		BaseURL: "http://blob.invalid/files",
		FS:      blob.Dir(t.TempDir()),
	})
	require.NoError(t, err)

	b, err := store.Put(t.Context(), strings.NewReader(content), blob.PutOptions{Name: name})
	require.NoError(t, err)
	return store, b
}

// TestUploadFileFromBlob is the workflow the blob argument exists for: bytes
// reach the remote without ever being a file on this host.
func TestUploadFileFromBlob(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(string) (string, int) { return "", 0 })
	defer cleanup()

	store, b := blobStore(t, "kubeconfig.yaml", "kubeconfig-bytes")
	// The root is empty and stays empty: a blob upload has no local path, so
	// nothing here is ever consulted.
	root := t.TempDir()
	remote := filepath.Join(t.TempDir(), "uploaded.yaml")

	handler := uploadFileHandler(&dummyPool{client: client, localFS: effect.Root(root)}, store)
	_, out, err := handler(t.Context(), &mcp.CallToolRequest{}, uploadFileParams{
		Source:     blob.Source{Blob: b.ID},
		SessionID:  "test_id",
		RemotePath: remote,
	})
	require.NoError(t, err)
	require.True(t, out.OK)
	require.NotEmpty(t, out.UploadID, "a blob upload is the same asynchronous job as a local one")

	got, err := os.ReadFile(remote)
	require.NoError(t, err)
	require.Equal(t, "kubeconfig-bytes", string(got))

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries, "the bytes never landed on this host")
}

func TestUploadFileArgumentValidation(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(string) (string, int) { return "", 0 })
	defer cleanup()

	store, b := blobStore(t, "f.txt", "content")
	handler := uploadFileHandler(&dummyPool{client: client, localFS: effect.Root(t.TempDir())}, store)

	for _, tt := range []struct {
		name string
		args uploadFileParams
	}{
		{"NeitherSource", uploadFileParams{SessionID: "test_id", RemotePath: "/tmp/x"}},
		{
			name: "BothSources",
			args: uploadFileParams{
				Source:     blob.Source{Blob: b.ID},
				SessionID:  "test_id",
				LocalPath:  "f.txt",
				RemotePath: "/tmp/x",
			},
		},
		{"NoSession", uploadFileParams{Source: blob.Source{Blob: b.ID}, RemotePath: "/tmp/x"}},
		{"NoRemotePath", uploadFileParams{Source: blob.Source{Blob: b.ID}, SessionID: "test_id"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := handler(t.Context(), &mcp.CallToolRequest{}, tt.args)
			require.Error(t, err)
		})
	}
}

// TestUploadFileRejectsAnUnknownBlob: an id arrives from the model, so the
// refusal has to be an error rather than an empty upload.
func TestUploadFileRejectsAnUnknownBlob(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(string) (string, int) { return "", 0 })
	defer cleanup()

	store, _ := blobStore(t, "f.txt", "content")
	remote := filepath.Join(t.TempDir(), "never.txt")

	handler := uploadFileHandler(&dummyPool{client: client, localFS: effect.Root(t.TempDir())}, store)
	_, _, err := handler(t.Context(), &mcp.CallToolRequest{}, uploadFileParams{
		Source:     blob.Source{Blob: "0192f4a0-1b2c-4d3e-8f90-a1b2c3d4e5f6"},
		SessionID:  "test_id",
		RemotePath: remote,
	})
	require.ErrorIs(t, err, blob.ErrNotFound)
	require.NoFileExists(t, remote)
}

// TestUploadFileWithoutABlobStore: a server started without one refuses with an
// error naming the flag, rather than silently ignoring the argument.
func TestUploadFileWithoutABlobStore(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(string) (string, int) { return "", 0 })
	defer cleanup()

	handler := uploadFileHandler(&dummyPool{client: client, localFS: effect.Root(t.TempDir())}, nil)
	_, _, err := handler(t.Context(), &mcp.CallToolRequest{}, uploadFileParams{
		Source:     blob.Source{Blob: "ssh-mcp/0192f4a0-1b2c-4d3e-8f90-a1b2c3d4e5f6"},
		SessionID:  "test_id",
		RemotePath: filepath.Join(t.TempDir(), "never.txt"),
	})
	require.ErrorIs(t, err, blob.ErrDenied)
}
