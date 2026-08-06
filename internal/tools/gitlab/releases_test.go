package gitlab

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/blob"
	"github.com/go-faster/gooners/internal/effect"
)

func TestViewRelease(t *testing.T) {
	t.Run("an empty tag reads the latest release", func(t *testing.T) {
		var paths []string
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tag_name": "v2.0", "name": "Two"}`))
		}))

		_, res, err := viewReleaseHandler(c)(context.Background(), &mcp.CallToolRequest{}, ViewReleaseArgs{Project: "g/p"})
		require.NoError(t, err)
		require.Equal(t, "v2.0", res.Release.TagName)
		require.Equal(t, []string{"/api/v4/projects/g/p/releases/permalink/latest"}, paths)
	})

	t.Run("maps assets and sources", func(t *testing.T) {
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"tag_name": "v1.0", "description": "notes",
				"milestones": [{"title": "m1"}],
				"assets": {
					"count": 2,
					"links": [{"id": 5, "name": "bin.tar.gz", "url": "https://x/u/bin.tar.gz",
					           "direct_asset_url": "https://x/d/bin.tar.gz", "link_type": "package"}],
					"sources": [{"format": "zip", "url": "https://x/src.zip"}]
				}
			}`))
		}))

		_, res, err := viewReleaseHandler(c)(context.Background(), &mcp.CallToolRequest{}, ViewReleaseArgs{
			Project: "g/p", TagName: "v1.0",
		})
		require.NoError(t, err)
		require.Equal(t, "notes", res.Release.Description)
		require.Equal(t, []string{"m1"}, res.Release.Milestones)
		require.Len(t, res.Release.Assets, 1)
		require.Equal(t, "bin.tar.gz", res.Release.Assets[0].Name)
		require.Equal(t, "package", res.Release.Assets[0].LinkType)
		require.Equal(t, []ReleaseAssetLink{{Format: "zip", URL: "https://x/src.zip"}}, res.Release.Sources)
	})
}

func TestCreateRelease(t *testing.T) {
	t.Run("requires a tag", func(t *testing.T) {
		c := newTestClient(t, Config{}, recordJSON(t, &gotRequest{}, `{}`))
		_, _, err := createReleaseHandler(c)(context.Background(), &mcp.CallToolRequest{}, CreateReleaseArgs{Project: "g/p"})
		require.ErrorContains(t, err, "tag_name is required")
	})

	t.Run("sends ref and notes", func(t *testing.T) {
		var got gotRequest
		c := newTestClient(t, Config{}, recordJSON(t, &got, `{"tag_name": "v1.0"}`))

		_, res, err := createReleaseHandler(c)(context.Background(), &mcp.CallToolRequest{}, CreateReleaseArgs{
			Project: "g/p", TagName: "v1.0", Ref: "main", Description: "notes",
		})
		require.NoError(t, err)
		require.Equal(t, "v1.0", got.Body["tag_name"])
		require.Equal(t, "main", got.Body["ref"])
		require.Equal(t, "notes", got.Body["description"])
		require.Equal(t, "v1.0", res.Release.TagName)
	})

	t.Run("rejects an unparseable released_at", func(t *testing.T) {
		c := newTestClient(t, Config{}, recordJSON(t, &gotRequest{}, `{}`))
		_, _, err := createReleaseHandler(c)(context.Background(), &mcp.CallToolRequest{}, CreateReleaseArgs{
			Project: "g/p", TagName: "v1.0", ReleasedAt: "next tuesday",
		})
		require.ErrorContains(t, err, "parse released_at")
	})
}

// The asset tools are the only ones that touch host files, so what they can
// reach must come from the provider the server was built with, never from the
// path the caller passed.
func TestReleaseAssetsFilesystemGate(t *testing.T) {
	t.Run("upload is denied when no assets directory was configured", func(t *testing.T) {
		called := false
		c := newTestClient(t, Config{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

		_, _, err := uploadReleaseAssetHandler(c)(context.Background(), &mcp.CallToolRequest{}, UploadReleaseAssetArgs{
			Project: "g/p", TagName: "v1.0", Path: "anything.zip",
		})
		require.Error(t, err)
		require.False(t, called, "nothing should be sent when the file cannot be read")
	})

	t.Run("download refuses to escape the assets directory", func(t *testing.T) {
		called := false
		c := newTestClient(t, Config{FS: effect.Root(t.TempDir())}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			called = true
		}))

		_, _, err := downloadReleaseAssetHandler(c)(context.Background(), &mcp.CallToolRequest{}, DownloadReleaseAssetArgs{
			Project: "g/p", TagName: "v1.0", Name: "bin", Path: "../escape.bin",
		})
		require.Error(t, err)
		require.False(t, called, "the destination must be rejected before any transfer")
	})

	t.Run("upload refuses to escape the assets directory", func(t *testing.T) {
		dir := t.TempDir()
		outside := filepath.Join(filepath.Dir(dir), "outside.txt")
		require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))
		t.Cleanup(func() { _ = os.Remove(outside) })

		called := false
		c := newTestClient(t, Config{FS: effect.Root(dir)}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			called = true
		}))

		_, _, err := uploadReleaseAssetHandler(c)(context.Background(), &mcp.CallToolRequest{}, UploadReleaseAssetArgs{
			Project: "g/p", TagName: "v1.0", Path: "../outside.txt",
		})
		require.Error(t, err)
		require.False(t, called)
	})
}

func TestUploadReleaseAsset(t *testing.T) {
	t.Run("uploads then links, defaulting the name to the file's", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "bin.tar.gz"), []byte("payload"), 0o600))

		var linkBody map[string]any
		c := newTestClient(t, Config{FS: effect.Root(dir)}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v4/projects/g/p/uploads":
				_, _ = w.Write([]byte(`{"id": 1, "url": "/uploads/abc/bin.tar.gz", "full_path": "/-/project/1/uploads/abc/bin.tar.gz"}`))
			case "/api/v4/projects/g/p/releases/v1.0/assets/links":
				require.NoError(t, decodeJSON(r, &linkBody))
				_, _ = w.Write([]byte(`{"id": 9, "name": "bin.tar.gz", "url": "https://x/a", "link_type": "other"}`))
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
			}
		}))

		_, res, err := uploadReleaseAssetHandler(c)(context.Background(), &mcp.CallToolRequest{}, UploadReleaseAssetArgs{
			Project: "g/p", TagName: "v1.0", Path: "bin.tar.gz",
		})
		require.NoError(t, err)
		require.Equal(t, "bin.tar.gz", linkBody["name"])
		require.Contains(t, linkBody["url"], "/-/project/1/uploads/abc/bin.tar.gz")
		require.Equal(t, int64(9), res.Asset.ID)
		require.Equal(t, int64(7), res.Size)
	})
}

func TestDownloadReleaseAsset(t *testing.T) {
	t.Run("writes the asset under the assets directory", func(t *testing.T) {
		dir := t.TempDir()
		c := newTestClient(t, Config{FS: effect.Root(dir), Token: "tok"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v4/projects/g/p/releases/v1.0":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"tag_name": "v1.0", "assets": {"count": 1, "links": [
					{"id": 5, "name": "bin", "url": "` + baseOf(r) + `/plain/bin", "direct_asset_url": "` + baseOf(r) + `/direct/bin"}
				]}}`))
			case "/direct/bin":
				// The token must ride along, or a private instance 404s.
				require.Equal(t, "tok", r.Header.Get("PRIVATE-TOKEN"))
				_, _ = w.Write([]byte("binary-content"))
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
			}
		}))

		_, res, err := downloadReleaseAssetHandler(c)(context.Background(), &mcp.CallToolRequest{}, DownloadReleaseAssetArgs{
			Project: "g/p", TagName: "v1.0", Name: "bin", Path: "out.bin",
		})
		require.NoError(t, err)
		require.Equal(t, int64(len("binary-content")), res.Size)

		got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
		require.NoError(t, err)
		require.Equal(t, "binary-content", string(got))
	})

	t.Run("an unknown asset name is an error", func(t *testing.T) {
		c := newTestClient(t, Config{FS: effect.Root(t.TempDir())}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tag_name": "v1.0", "assets": {"count": 0, "links": []}}`))
		}))

		_, _, err := downloadReleaseAssetHandler(c)(context.Background(), &mcp.CallToolRequest{}, DownloadReleaseAssetArgs{
			Project: "g/p", TagName: "v1.0", Name: "missing", Path: "out.bin",
		})
		require.ErrorContains(t, err, `no asset named "missing"`)
	})

	t.Run("a failed download leaves no file behind", func(t *testing.T) {
		dir := t.TempDir()
		c := newTestClient(t, Config{FS: effect.Root(dir)}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v4/projects/g/p/releases/v1.0" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"tag_name": "v1.0", "assets": {"count": 1, "links": [
					{"id": 5, "name": "bin", "url": "` + baseOf(r) + `/gone/bin"}
				]}}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))

		_, _, err := downloadReleaseAssetHandler(c)(context.Background(), &mcp.CallToolRequest{}, DownloadReleaseAssetArgs{
			Project: "g/p", TagName: "v1.0", Name: "bin", Path: "out.bin",
		})
		require.ErrorContains(t, err, "unexpected status")

		_, statErr := os.Stat(filepath.Join(dir, "out.bin"))
		require.True(t, os.IsNotExist(statErr), "no partial file should be written")
	})
}

// assetServer serves one release with one asset named "bin" holding body.
func assetServer(t *testing.T, body string) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/g/p/releases/v1.0":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tag_name": "v1.0", "assets": {"count": 1, "links": [
				{"id": 5, "name": "bin", "url": "` + baseOf(r) + `/plain/bin", "direct_asset_url": "` + baseOf(r) + `/direct/bin"}
			]}}`))
		case "/direct/bin":
			_, _ = w.Write([]byte(body))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
}

// newBlobStore returns a store whose URLs are stable enough to assert on.
func newBlobStore(t *testing.T) *blob.HTTP {
	t.Helper()

	s, err := blob.NewHTTP(blob.HTTPOptions{
		BaseURL: "http://blob.invalid/files",
		FS:      blob.Dir(t.TempDir()),
	})
	require.NoError(t, err)
	return s
}

// TestDownloadReleaseAssetToBlob covers the path an agent takes when it cannot
// read this server's filesystem: no path argument, and the answer is a URL.
func TestDownloadReleaseAssetToBlob(t *testing.T) {
	ctx := context.Background()

	t.Run("returns a resource link", func(t *testing.T) {
		store := newBlobStore(t)
		// No FS at all: a URL must not depend on -assets-dir.
		c := newTestClient(t, Config{Blob: store}, assetServer(t, "binary-content"))

		res, out, err := downloadReleaseAssetHandler(c)(ctx, &mcp.CallToolRequest{}, DownloadReleaseAssetArgs{
			Project: "g/p", TagName: "v1.0", Name: "bin",
		})
		require.NoError(t, err)
		require.Empty(t, out.Path)
		require.Equal(t, int64(len("binary-content")), out.Size)
		require.NotEmpty(t, out.URL)
		require.NotNil(t, out.ExpiresAt)

		require.NotNil(t, res)
		require.Len(t, res.Content, 1)
		link, ok := res.Content[0].(*mcp.ResourceLink)
		require.True(t, ok, "got %T", res.Content[0])
		require.Equal(t, out.URL, link.URI)
		require.Equal(t, "bin", link.Name)

		// The link resolves to the asset's bytes.
		id, _, _ := strings.Cut(strings.TrimPrefix(link.URI, "http://blob.invalid/files/"), "/")
		f, _, err := store.Open(ctx, id)
		require.NoError(t, err)
		defer func() { _ = f.Close() }()
		got, err := io.ReadAll(f)
		require.NoError(t, err)
		require.Equal(t, "binary-content", string(got))
	})

	// A small text asset is worth more inline than as a URL the agent has to
	// go and fetch.
	t.Run("inlines a small text asset", func(t *testing.T) {
		c := newTestClient(t, Config{Blob: newBlobStore(t)}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v4/projects/g/p/releases/v1.0":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"tag_name": "v1.0", "assets": {"count": 1, "links": [
					{"id": 5, "name": "notes.txt", "url": "` + baseOf(r) + `/direct/notes"}
				]}}`))
			case "/direct/notes":
				_, _ = w.Write([]byte("release notes"))
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
			}
		}))

		res, out, err := downloadReleaseAssetHandler(c)(ctx, &mcp.CallToolRequest{}, DownloadReleaseAssetArgs{
			Project: "g/p", TagName: "v1.0", Name: "notes.txt",
		})
		require.NoError(t, err)
		require.Empty(t, out.URL, "an inlined asset was never stored")

		text, ok := res.Content[0].(*mcp.TextContent)
		require.True(t, ok, "got %T", res.Content[0])
		require.Equal(t, "release notes", text.Text)
	})

	// Without a store the tool says which flag is missing, instead of writing
	// somewhere the agent cannot read or returning a URL that resolves nowhere.
	t.Run("without a store the error names the flag", func(t *testing.T) {
		c := newTestClient(t, Config{}, assetServer(t, "binary-content"))

		_, _, err := downloadReleaseAssetHandler(c)(ctx, &mcp.CallToolRequest{}, DownloadReleaseAssetArgs{
			Project: "g/p", TagName: "v1.0", Name: "bin",
		})
		require.ErrorIs(t, err, blob.ErrDenied)
		require.ErrorContains(t, err, "-blob-addr")
	})

	// The size cap holds on the blob path too, and it is an error rather than
	// a silently truncated object.
	t.Run("an oversized asset is refused", func(t *testing.T) {
		store := newBlobStore(t)
		c := newTestClient(t, Config{Blob: store}, assetServer(t, strings.Repeat("x", maxAssetSize+1)))

		_, _, err := downloadReleaseAssetHandler(c)(ctx, &mcp.CallToolRequest{}, DownloadReleaseAssetArgs{
			Project: "g/p", TagName: "v1.0", Name: "bin",
		})
		require.ErrorContains(t, err, "over the")
	})
}

func TestListReleases(t *testing.T) {
	var got gotRequest
	c := newTestClient(t, Config{}, recordJSON(t, &got, `[
		{"tag_name": "v2.0", "name": "Two", "assets": {"count": 3},
		 "author": {"username": "alice"}, "commit": {"id": "abc123"}},
		{"tag_name": "v1.0", "name": "One", "upcoming_release": false, "assets": {"count": 0}}
	]`))

	_, res, err := listReleasesHandler(c)(context.Background(), &mcp.CallToolRequest{}, ListReleasesArgs{
		Project: "g/p", ListArgs: ListArgs{PerPage: 2},
	})
	require.NoError(t, err)
	require.Equal(t, "/api/v4/projects/g/p/releases", got.Path)
	require.Equal(t, "2", got.Query.Get("per_page"))
	require.Len(t, res.Releases, 2)
	require.Equal(t, "v2.0", res.Releases[0].TagName)
	require.Equal(t, int64(3), res.Releases[0].AssetsCount)
	require.Equal(t, "abc123", res.Releases[0].CommitSHA)
	require.Equal(t, "alice", res.Releases[0].Author.Username)
}
