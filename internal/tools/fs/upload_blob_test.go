package fs

import (
	"io"
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

// TestDownloadFileToBlob is workflow 4: a file on the remote host becomes a
// URL, without landing on this server on the way.
func TestDownloadFileToBlob(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(string) (string, int) { return "", 0 })
	defer cleanup()

	store, _ := blobStore(t, "unused.txt", "unused")
	remote := filepath.Join(t.TempDir(), "server.log")
	require.NoError(t, os.WriteFile(remote, []byte("log-bytes"), 0o600))

	root := t.TempDir()
	pool := &dummyPool{client: client, localFS: effect.Root(root)}
	handler := downloadFileHandler(pool, store)

	_, out, err := handler(t.Context(), &mcp.CallToolRequest{}, downloadFileParams{
		SessionID:  "test_id",
		RemotePath: remote,
		ToBlob:     true,
	})
	require.NoError(t, err)
	require.True(t, out.OK)
	require.NotEmpty(t, out.DownloadID, "a blob download is the same asynchronous job as a local one")

	require.Equal(t, "server.log", pool.lastBlob.Name, "the remote base name by default")
	r, _, err := store.Open(t.Context(), pool.lastBlob.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, "log-bytes", string(got))

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries, "the bytes never landed on this host")
}

func TestDownloadFileArgumentValidation(t *testing.T) {
	client, cleanup := setupMockSSHServer(t, func(string) (string, int) { return "", 0 })
	defer cleanup()

	store, _ := blobStore(t, "f.txt", "content")
	pool := &dummyPool{client: client, localFS: effect.Root(t.TempDir())}

	for _, tt := range []struct {
		name  string
		store blob.Store
		args  downloadFileParams
	}{
		{
			name:  "NeitherDestination",
			store: store,
			args:  downloadFileParams{SessionID: "test_id", RemotePath: "/tmp/x"},
		},
		{
			name:  "BothDestinations",
			store: store,
			args:  downloadFileParams{SessionID: "test_id", RemotePath: "/tmp/x", LocalPath: "f.txt", ToBlob: true},
		},
		{
			name:  "NoRemotePath",
			store: store,
			args:  downloadFileParams{SessionID: "test_id", ToBlob: true},
		},
		{
			// The operator forgot the flag, and saying so beats an empty result.
			name:  "NoBlobStore",
			store: nil,
			args:  downloadFileParams{SessionID: "test_id", RemotePath: "/tmp/x", ToBlob: true},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := downloadFileHandler(pool, tt.store)(t.Context(), &mcp.CallToolRequest{}, tt.args)
			require.Error(t, err)
		})
	}
}
