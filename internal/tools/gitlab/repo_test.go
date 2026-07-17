package gitlab

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestReadmeName(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   string
		want string
	}{
		{"blob url", "https://gitlab.example.com/g/p/-/blob/main/README.md", "README.md"},
		{"nested group", "https://gitlab.example.com/g/s/p/-/blob/main/docs/README.rst", "README.rst"},
		{"no slash", "README.md", "README.md"},
		{"empty", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, readmeName(tt.in))
		})
	}
}

func TestViewProject(t *testing.T) {
	t.Run("skips the readme unless asked", func(t *testing.T) {
		var paths []string
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": 1, "name": "p", "path_with_namespace": "g/p",
				"default_branch": "main", "visibility": "private",
				"readme_url": "https://x/g/p/-/blob/main/README.md"}`))
		}))

		_, res, err := viewProjectHandler(c)(context.Background(), &mcp.CallToolRequest{}, ViewProjectArgs{Project: "g/p"})
		require.NoError(t, err)
		require.Equal(t, "g/p", res.Project.PathWithNS)
		require.Equal(t, "private", res.Project.Visibility)
		require.Empty(t, res.Project.Readme)
		require.Equal(t, []string{"/api/v4/projects/g/p"}, paths)
	})

	t.Run("fetches the readme at the default branch when asked", func(t *testing.T) {
		var ref string
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/repository/files/") {
				ref = r.URL.Query().Get("ref")
				_, _ = w.Write([]byte("# Title\n"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": 1, "path_with_namespace": "g/p", "default_branch": "trunk",
				"readme_url": "https://x/g/p/-/blob/trunk/README.md"}`))
		}))

		_, res, err := viewProjectHandler(c)(context.Background(), &mcp.CallToolRequest{}, ViewProjectArgs{
			Project: "g/p", WithReadme: true,
		})
		require.NoError(t, err)
		require.Equal(t, "# Title\n", res.Project.Readme)
		require.Equal(t, "trunk", ref)
	})
}

func TestSearchProjects(t *testing.T) {
	t.Run("requires a search term", func(t *testing.T) {
		called := false
		c := newTestClient(t, Config{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

		_, _, err := searchProjectsHandler(c)(context.Background(), &mcp.CallToolRequest{}, SearchProjectsArgs{})
		require.ErrorContains(t, err, "search is required")
		require.False(t, called)
	})

	// repo_search is the tool that answers "what is the project path", so the
	// path is the field that has to come back.
	t.Run("returns the path other tools take", func(t *testing.T) {
		var got gotRequest
		c := newTestClient(t, Config{}, recordJSON(t, &got, `[
			{"id": 1, "name": "cli", "path_with_namespace": "gitlab-org/cli"}
		]`))

		_, res, err := searchProjectsHandler(c)(context.Background(), &mcp.CallToolRequest{}, SearchProjectsArgs{
			Search: "cli", Membership: true,
		})
		require.NoError(t, err)
		require.Equal(t, "cli", got.Query.Get("search"))
		require.Equal(t, "true", got.Query.Get("membership"))
		require.Len(t, res.Projects, 1)
		require.Equal(t, "gitlab-org/cli", res.Projects[0].PathWithNS)
	})
}

func TestTree(t *testing.T) {
	var got gotRequest
	c := newTestClient(t, Config{}, recordJSON(t, &got, `[
		{"id": "a", "name": "cmd", "type": "tree", "path": "cmd"},
		{"id": "b", "name": "main.go", "type": "blob", "path": "cmd/main.go"}
	]`))

	_, res, err := treeHandler(c)(context.Background(), &mcp.CallToolRequest{}, TreeArgs{
		Project: "g/p", Path: "cmd", Ref: "main", Recursive: true,
	})
	require.NoError(t, err)
	require.Equal(t, "cmd", got.Query.Get("path"))
	require.Equal(t, "main", got.Query.Get("ref"))
	require.Equal(t, "true", got.Query.Get("recursive"))
	require.Equal(t, []TreeEntry{
		{Name: "cmd", Path: "cmd", Type: "tree"},
		{Name: "main.go", Path: "cmd/main.go", Type: "blob"},
	}, res.Entries)
}

func TestFile(t *testing.T) {
	t.Run("requires a path", func(t *testing.T) {
		c := newTestClient(t, Config{}, recordJSON(t, &gotRequest{}, `{}`))
		_, _, err := fileHandler(c)(context.Background(), &mcp.CallToolRequest{}, FileArgs{Project: "g/p"})
		require.ErrorContains(t, err, "path is required")
	})

	t.Run("reads at an explicit ref without looking up the project", func(t *testing.T) {
		var paths []string
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			_, _ = w.Write([]byte("hello"))
		}))

		_, res, err := fileHandler(c)(context.Background(), &mcp.CallToolRequest{}, FileArgs{
			Project: "g/p", Path: "a.txt", Ref: "v1",
		})
		require.NoError(t, err)
		require.Equal(t, "hello", res.Content)
		require.Equal(t, "v1", res.Ref)
		require.Equal(t, int64(5), res.Size)
		require.Len(t, paths, 1)
	})

	t.Run("resolves the default branch when no ref is given", func(t *testing.T) {
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/repository/files/") {
				require.Equal(t, "trunk", r.URL.Query().Get("ref"))
				_, _ = w.Write([]byte("hello"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": 1, "default_branch": "trunk"}`))
		}))

		_, res, err := fileHandler(c)(context.Background(), &mcp.CallToolRequest{}, FileArgs{
			Project: "g/p", Path: "a.txt",
		})
		require.NoError(t, err)
		require.Equal(t, "trunk", res.Ref)
	})

	// Escaped bytes in a JSON string help nobody; the size and the flag do.
	t.Run("a binary file reports itself instead of returning bytes", func(t *testing.T) {
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte{0x00, 0xff, 0xfe})
		}))

		_, res, err := fileHandler(c)(context.Background(), &mcp.CallToolRequest{}, FileArgs{
			Project: "g/p", Path: "logo.png", Ref: "main",
		})
		require.NoError(t, err)
		require.True(t, res.Binary)
		require.Empty(t, res.Content)
		require.Equal(t, int64(3), res.Size)
	})

	t.Run("a large file is truncated and says so", func(t *testing.T) {
		body := strings.Repeat("a", fileLimit+100)
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))

		_, res, err := fileHandler(c)(context.Background(), &mcp.CallToolRequest{}, FileArgs{
			Project: "g/p", Path: "big.txt", Ref: "main",
		})
		require.NoError(t, err)
		require.True(t, res.Truncated)
		require.Len(t, res.Content, fileLimit)
		require.Equal(t, int64(len(body)), res.Size, "size reports the real file, not the truncation")
	})
}
