package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"github.com/yosida95/uritemplate/v3"
)

// The URI templates carry the project path, whose segment count is unbounded,
// so what they do and do not match is the whole design.
func TestResourceURITemplates(t *testing.T) {
	for _, tt := range []struct {
		name     string
		tmpl     string
		uri      string
		wantVars map[string]string
	}{
		{
			name:     "issue",
			tmpl:     issueURITemplate,
			uri:      "gitlab:///group/proj/-/issues/12",
			wantVars: map[string]string{"project": "group/proj", "iid": "12"},
		},
		{
			name:     "issue in a nested group",
			tmpl:     issueURITemplate,
			uri:      "gitlab:///group/sub/deeper/proj/-/issues/12",
			wantVars: map[string]string{"project": "group/sub/deeper/proj", "iid": "12"},
		},
		{
			name:     "merge request",
			tmpl:     mrURITemplate,
			uri:      "gitlab:///group/proj/-/merge_requests/3",
			wantVars: map[string]string{"project": "group/proj", "iid": "3"},
		},
		{
			name:     "release",
			tmpl:     releaseURITemplate,
			uri:      "gitlab:///group/proj/-/releases/v1.2.3",
			wantVars: map[string]string{"project": "group/proj", "tag": "v1.2.3"},
		},
		{
			name:     "blob with a nested path",
			tmpl:     blobURITemplate,
			uri:      "gitlab:///group/proj/-/blob/main/cmd/app/main.go",
			wantVars: map[string]string{"project": "group/proj", "ref": "main", "path": "cmd/app/main.go"},
		},
		{
			name:     "blob at a sha",
			tmpl:     blobURITemplate,
			uri:      "gitlab:///group/proj/-/blob/8f2a1c9/README.md",
			wantVars: map[string]string{"project": "group/proj", "ref": "8f2a1c9", "path": "README.md"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			vals, err := resourceVars(tt.tmpl, tt.uri)
			require.NoError(t, err)
			for k, want := range tt.wantVars {
				require.Equal(t, want, vals.Get(k).String(), "variable %q", k)
			}
		})
	}
}

func TestResourceURITemplatesDoNotCrossMatch(t *testing.T) {
	// Each template must claim only its own URIs: the server picks a handler by
	// matching, so an overlap would route a read to the wrong resource.
	uris := []string{
		"gitlab:///group/proj/-/issues/12",
		"gitlab:///group/proj/-/merge_requests/12",
		"gitlab:///group/proj/-/releases/v1.0",
		"gitlab:///group/proj/-/blob/main/main.go",
	}
	tmpls := []string{issueURITemplate, mrURITemplate, releaseURITemplate, blobURITemplate}

	for i, tmpl := range tmpls {
		parsed, err := uritemplate.New(tmpl)
		require.NoError(t, err)
		for j, uri := range uris {
			matched := parsed.Regexp().MatchString(uri)
			require.Equal(t, i == j, matched, "template %s vs uri %s", tmpl, uri)
		}
	}
}

func TestResourceVarsRejectsForeignURI(t *testing.T) {
	_, err := resourceVars(issueURITemplate, "https://example.com/whatever")
	require.Error(t, err)
}

func TestIssueResource(t *testing.T) {
	c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v4/projects/group/sub/proj/issues/12", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id": 1, "iid": 12, "title": "T", "description": "body", "state": "opened"}`))
	}))

	uri := "gitlab:///group/sub/proj/-/issues/12"
	res, err := issueResourceHandler(c)(context.Background(), &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: uri},
	})
	require.NoError(t, err)
	require.Len(t, res.Contents, 1)
	require.Equal(t, uri, res.Contents[0].URI)
	require.Equal(t, "application/json", res.Contents[0].MIMEType)

	var got IssueDetail
	require.NoError(t, json.Unmarshal([]byte(res.Contents[0].Text), &got))
	require.Equal(t, int64(12), got.IID)
	require.Equal(t, "body", got.Description)
}

func TestBlobResource(t *testing.T) {
	t.Run("text is served as itself, not wrapped in JSON", func(t *testing.T) {
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The file path is a single escaped path segment, which is what
			// lets a nested path address a file at all.
			require.Equal(t, "/api/v4/projects/g%2Fp/repository/files/cmd%2Fmain%2Ego/raw", r.URL.EscapedPath())
			require.Equal(t, "main", r.URL.Query().Get("ref"))
			_, _ = w.Write([]byte("package main\n"))
		}))

		uri := "gitlab:///g/p/-/blob/main/cmd/main.go"
		res, err := blobResourceHandler(c)(context.Background(), &mcp.ReadResourceRequest{
			Params: &mcp.ReadResourceParams{URI: uri},
		})
		require.NoError(t, err)
		require.Equal(t, "text/plain", res.Contents[0].MIMEType)
		require.Equal(t, "package main\n", res.Contents[0].Text)
		require.Empty(t, res.Contents[0].Blob)
	})

	t.Run("binary is served as a blob", func(t *testing.T) {
		raw := []byte{0x00, 0xff, 0xfe, 0x01}
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(raw)
		}))

		res, err := blobResourceHandler(c)(context.Background(), &mcp.ReadResourceRequest{
			Params: &mcp.ReadResourceParams{URI: "gitlab:///g/p/-/blob/main/logo.png"},
		})
		require.NoError(t, err)
		require.Equal(t, "application/octet-stream", res.Contents[0].MIMEType)
		require.Equal(t, raw, res.Contents[0].Blob)
		require.Empty(t, res.Contents[0].Text)
	})
}

func TestRegisterResources(t *testing.T) {
	t.Run("the default project is listed as a concrete resource", func(t *testing.T) {
		s := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
		c, err := NewClient(Config{DefaultProject: "group/proj"})
		require.NoError(t, err)
		registerResources(s, c)

		// A client with no project in hand must still find an entry point.
		require.NotPanics(t, func() { s.RemoveResources("gitlab:///group/proj") })
	})

	t.Run("without a default project only templates are registered", func(t *testing.T) {
		s := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
		c, err := NewClient(Config{})
		require.NoError(t, err)
		require.NotPanics(t, func() { registerResources(s, c) })
	})
}

func TestMergeRequestResource(t *testing.T) {
	c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v4/projects/g/p/merge_requests/3", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id": 1, "iid": 3, "title": "Add thing", "state": "opened",
			"description": "why", "detailed_merge_status": "mergeable", "sha": "abc"}`))
	}))

	uri := "gitlab:///g/p/-/merge_requests/3"
	res, err := mergeRequestResourceHandler(c)(context.Background(), &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: uri},
	})
	require.NoError(t, err)
	require.Equal(t, "application/json", res.Contents[0].MIMEType)

	var got MergeRequestDetail
	require.NoError(t, json.Unmarshal([]byte(res.Contents[0].Text), &got))
	require.Equal(t, int64(3), got.IID)
	require.Equal(t, "mergeable", got.DetailedMergeStatus)
}

func TestReleaseResource(t *testing.T) {
	c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v4/projects/g/p/releases/v1.2.3", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name": "v1.2.3", "name": "One", "description": "notes"}`))
	}))

	res, err := releaseResourceHandler(c)(context.Background(), &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: "gitlab:///g/p/-/releases/v1.2.3"},
	})
	require.NoError(t, err)

	var got ReleaseDetail
	require.NoError(t, json.Unmarshal([]byte(res.Contents[0].Text), &got))
	require.Equal(t, "v1.2.3", got.TagName)
	require.Equal(t, "notes", got.Description)
}

func TestProjectResource(t *testing.T) {
	c := newTestClient(t, Config{DefaultProject: "g/p"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The handler serves the configured project, not one parsed from the URI.
		require.Equal(t, "/api/v4/projects/g/p", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id": 1, "name": "p", "path_with_namespace": "g/p", "default_branch": "main"}`))
	}))

	res, err := projectResourceHandler(c)(context.Background(), &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: "gitlab:///g/p"},
	})
	require.NoError(t, err)

	var got ProjectSummary
	require.NoError(t, json.Unmarshal([]byte(res.Contents[0].Text), &got))
	require.Equal(t, "g/p", got.PathWithNS)
	require.Equal(t, "main", got.DefaultBranch)
}

func TestResourceErrorsSurface(t *testing.T) {
	c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message": "404 Not found"}`))
	}))

	_, err := issueResourceHandler(c)(context.Background(), &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: "gitlab:///g/p/-/issues/999"},
	})
	require.ErrorContains(t, err, "get issue g/p#999")
}
