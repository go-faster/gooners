package gitlab

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// connect wires a real client to a server with everything registered, which is
// what actually exercises schema inference: a struct the SDK cannot build a
// schema for fails here and nowhere else.
func connect(t *testing.T, cfg Config) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	c, err := NewClient(cfg)
	require.NoError(t, err)

	s := mcp.NewServer(&mcp.Implementation{Name: "gitlab-mcp-test", Version: "test"}, nil)
	Register(s, c)

	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })

	return cs
}

func TestRegisterTools(t *testing.T) {
	cs := connect(t, Config{})

	res, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)

	tools := make(map[string]*mcp.Tool, len(res.Tools))
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}

	// The whole point of the rebuild is a small, deliberate tool set: glab's
	// MCP server exposes every annotated command instead.
	want := []string{
		"issue_list", "issue_view", "issue_create", "issue_update", "issue_note",
		"mr_list", "mr_view", "mr_diff", "mr_create", "mr_update", "mr_note",
		"release_list", "release_view", "release_create",
		"release_asset_upload", "release_asset_download",
		"repo_view", "repo_search", "repo_tree", "repo_file",
	}
	for _, name := range want {
		require.Contains(t, tools, name, "tool %s must be registered", name)
	}
	require.Len(t, res.Tools, len(want), "no tool should be registered that is not in the agreed set")

	t.Run("read-only tools are annotated as such", func(t *testing.T) {
		for _, name := range []string{
			"issue_list", "issue_view", "mr_list", "mr_view", "mr_diff",
			"release_list", "release_view", "repo_view", "repo_search", "repo_tree", "repo_file",
		} {
			require.True(t, tools[name].Annotations.ReadOnlyHint, "%s must be read-only", name)
		}
	})

	t.Run("writing tools are not marked read-only", func(t *testing.T) {
		for _, name := range []string{
			"issue_create", "issue_update", "issue_note",
			"mr_create", "mr_update", "mr_note", "release_create",
			"release_asset_upload", "release_asset_download",
		} {
			require.False(t, tools[name].Annotations.ReadOnlyHint, "%s must not be read-only", name)
		}
	})

	// Every tool taking a project is the fix for glab's hidden --repo flag, so
	// it is worth asserting rather than assuming.
	t.Run("every tool but repo_search accepts a project", func(t *testing.T) {
		for name, tool := range tools {
			if name == "repo_search" {
				continue
			}
			props, ok := tool.InputSchema.(map[string]any)["properties"].(map[string]any)
			require.True(t, ok, "%s has no properties", name)
			require.Contains(t, props, "project", "%s must take a project", name)
		}
	})

	t.Run("list tools expose pagination from the embedded ListArgs", func(t *testing.T) {
		for _, name := range []string{"issue_list", "mr_list", "release_list", "repo_search", "repo_tree"} {
			props := tools[name].InputSchema.(map[string]any)["properties"].(map[string]any)
			require.Contains(t, props, "page", "%s must expose page", name)
			require.Contains(t, props, "per_page", "%s must expose per_page", name)
		}
	})
}

func TestRegisterResourceTemplates(t *testing.T) {
	cs := connect(t, Config{})

	res, err := cs.ListResourceTemplates(context.Background(), nil)
	require.NoError(t, err)

	got := make(map[string]string, len(res.ResourceTemplates))
	for _, rt := range res.ResourceTemplates {
		got[rt.Name] = rt.URITemplate
	}
	require.Equal(t, map[string]string{
		"issue":         issueURITemplate,
		"merge_request": mrURITemplate,
		"release":       releaseURITemplate,
		"blob":          blobURITemplate,
	}, got)
}

func TestRegisterResourcesWithDefaultProject(t *testing.T) {
	t.Run("the default project appears in the resource list", func(t *testing.T) {
		cs := connect(t, Config{DefaultProject: "group/proj"})

		res, err := cs.ListResources(context.Background(), nil)
		require.NoError(t, err)
		require.Len(t, res.Resources, 1)
		require.Equal(t, "gitlab:///group/proj", res.Resources[0].URI)
	})

	t.Run("without one there is nothing concrete to list", func(t *testing.T) {
		cs := connect(t, Config{})

		res, err := cs.ListResources(context.Background(), nil)
		require.NoError(t, err)
		require.Empty(t, res.Resources)
	})
}
