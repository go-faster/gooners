package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// gotRequest records what a handler received, so a test can assert on the
// query GitLab was actually asked, not just on what came back.
type gotRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   map[string]any
}

func recordJSON(t *testing.T, got *gotRequest, response string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		got.Method = r.Method
		got.Path = r.URL.Path
		got.Query = r.URL.Query()
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&got.Body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}
}

func TestListIssues(t *testing.T) {
	t.Run("maps filters onto the API query", func(t *testing.T) {
		var got gotRequest
		c := newTestClient(t, Config{}, recordJSON(t, &got, `[
			{"id": 100, "iid": 7, "title": "Fix the thing", "state": "opened",
			 "labels": ["bug"], "web_url": "https://gitlab.example.com/g/p/-/issues/7",
			 "author": {"username": "alice", "name": "Alice"},
			 "assignees": [{"username": "bob", "name": "Bob"}],
			 "milestone": {"title": "v1.0"}}
		]`))

		_, res, err := listIssuesHandler(c)(context.Background(), &mcp.CallToolRequest{}, ListIssuesArgs{
			Project:  "group/proj",
			State:    "opened",
			Labels:   "bug, ux",
			Assignee: "bob",
			Author:   "alice",
			ListArgs: ListArgs{PerPage: 5},
		})
		require.NoError(t, err)

		require.Equal(t, "/api/v4/projects/group/proj/issues", got.Path)
		require.Equal(t, "opened", got.Query.Get("state"))
		require.Equal(t, "bug,ux", got.Query.Get("labels"))
		require.Equal(t, "bob", got.Query.Get("assignee_username"))
		require.Equal(t, "alice", got.Query.Get("author_username"))
		require.Equal(t, "5", got.Query.Get("per_page"))

		require.Len(t, res.Issues, 1)
		issue := res.Issues[0]
		require.Equal(t, int64(7), issue.IID)
		require.Equal(t, "Fix the thing", issue.Title)
		require.Equal(t, "alice", issue.Author.Username)
		require.Equal(t, []UserSummary{{Username: "bob", Name: "Bob"}}, issue.Assignees)
		require.Equal(t, "v1.0", issue.Milestone)
	})

	t.Run("uses the default project", func(t *testing.T) {
		var got gotRequest
		c := newTestClient(t, Config{DefaultProject: "group/default"}, recordJSON(t, &got, `[]`))

		_, res, err := listIssuesHandler(c)(context.Background(), &mcp.CallToolRequest{}, ListIssuesArgs{})
		require.NoError(t, err)
		require.Equal(t, "/api/v4/projects/group/default/issues", got.Path)
		require.Empty(t, res.Issues)
	})

	t.Run("without a project or a default it errors before calling out", func(t *testing.T) {
		called := false
		c := newTestClient(t, Config{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			called = true
		}))

		_, _, err := listIssuesHandler(c)(context.Background(), &mcp.CallToolRequest{}, ListIssuesArgs{})
		require.ErrorContains(t, err, "project is required")
		require.False(t, called, "no request should be made without a project")
	})

	t.Run("surfaces an API error", func(t *testing.T) {
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message": "403 Forbidden"}`))
		}))

		_, _, err := listIssuesHandler(c)(context.Background(), &mcp.CallToolRequest{}, ListIssuesArgs{Project: "group/proj"})
		require.ErrorContains(t, err, "list issues of group/proj")
	})
}

func TestViewIssue(t *testing.T) {
	t.Run("requires an iid", func(t *testing.T) {
		c := newTestClient(t, Config{}, recordJSON(t, &gotRequest{}, `{}`))
		_, _, err := viewIssueHandler(c)(context.Background(), &mcp.CallToolRequest{}, ViewIssueArgs{Project: "g/p"})
		require.ErrorContains(t, err, "iid is required")
	})

	t.Run("omits notes unless asked", func(t *testing.T) {
		var paths []string
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": 100, "iid": 7, "title": "T", "description": "body"}`))
		}))

		_, res, err := viewIssueHandler(c)(context.Background(), &mcp.CallToolRequest{}, ViewIssueArgs{Project: "g/p", IID: 7})
		require.NoError(t, err)
		require.Equal(t, "body", res.Issue.Description)
		require.Empty(t, res.Issue.Notes)
		require.Equal(t, []string{"/api/v4/projects/g/p/issues/7"}, paths)
	})

	t.Run("fetches notes when asked", func(t *testing.T) {
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v4/projects/g/p/issues/7":
				_, _ = w.Write([]byte(`{"id": 100, "iid": 7, "title": "T"}`))
			case "/api/v4/projects/g/p/issues/7/notes":
				require.Equal(t, "asc", r.URL.Query().Get("sort"))
				_, _ = w.Write([]byte(`[
					{"id": 1, "body": "first", "author": {"username": "alice"}},
					{"id": 2, "body": "closed", "system": true, "author": {"username": "bob"}}
				]`))
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
			}
		}))

		_, res, err := viewIssueHandler(c)(context.Background(), &mcp.CallToolRequest{}, ViewIssueArgs{
			Project: "g/p", IID: 7, WithNotes: true,
		})
		require.NoError(t, err)
		require.Len(t, res.Issue.Notes, 2)
		require.Equal(t, "first", res.Issue.Notes[0].Body)
		require.True(t, res.Issue.Notes[1].System)
	})
}

func TestUpdateIssue(t *testing.T) {
	t.Run("rejects an empty update rather than making a no-op call", func(t *testing.T) {
		called := false
		c := newTestClient(t, Config{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			called = true
		}))

		_, _, err := updateIssueHandler(c)(context.Background(), &mcp.CallToolRequest{}, UpdateIssueArgs{
			Project: "g/p", IID: 7,
		})
		require.ErrorContains(t, err, "no changes requested")
		require.False(t, called)
	})

	t.Run("rejects an unknown state", func(t *testing.T) {
		c := newTestClient(t, Config{}, recordJSON(t, &gotRequest{}, `{}`))
		_, _, err := updateIssueHandler(c)(context.Background(), &mcp.CallToolRequest{}, UpdateIssueArgs{
			Project: "g/p", IID: 7, State: "merged",
		})
		require.ErrorContains(t, err, "state must be close or reopen")
	})

	t.Run("add_labels edits rather than replaces", func(t *testing.T) {
		var got gotRequest
		c := newTestClient(t, Config{}, recordJSON(t, &got, `{"id": 100, "iid": 7, "title": "T"}`))

		_, _, err := updateIssueHandler(c)(context.Background(), &mcp.CallToolRequest{}, UpdateIssueArgs{
			Project: "g/p", IID: 7, AddLabels: []string{"bug"}, State: "close",
		})
		require.NoError(t, err)
		require.Equal(t, http.MethodPut, got.Method)
		require.Equal(t, "bug", got.Body["add_labels"])
		require.Equal(t, "close", got.Body["state_event"])
		require.NotContains(t, got.Body, "labels", "add_labels must not send a replacing label set")
	})

	t.Run("unassign clears the assignee list", func(t *testing.T) {
		var got gotRequest
		c := newTestClient(t, Config{}, recordJSON(t, &got, `{"id": 100, "iid": 7, "title": "T"}`))

		_, _, err := updateIssueHandler(c)(context.Background(), &mcp.CallToolRequest{}, UpdateIssueArgs{
			Project: "g/p", IID: 7, Unassign: true,
		})
		require.NoError(t, err)
		require.Equal(t, []any{}, got.Body["assignee_ids"])
	})
}

func TestCreateIssueNote(t *testing.T) {
	t.Run("requires a body", func(t *testing.T) {
		c := newTestClient(t, Config{}, recordJSON(t, &gotRequest{}, `{}`))
		_, _, err := createIssueNoteHandler(c)(context.Background(), &mcp.CallToolRequest{}, CreateIssueNoteArgs{
			Project: "g/p", IID: 7,
		})
		require.ErrorContains(t, err, "body is required")
	})

	t.Run("posts the comment", func(t *testing.T) {
		var got gotRequest
		c := newTestClient(t, Config{}, recordJSON(t, &got, `{"id": 9, "body": "hi", "author": {"username": "alice"}}`))

		_, res, err := createIssueNoteHandler(c)(context.Background(), &mcp.CallToolRequest{}, CreateIssueNoteArgs{
			Project: "g/p", IID: 7, Body: "hi",
		})
		require.NoError(t, err)
		require.Equal(t, http.MethodPost, got.Method)
		require.Equal(t, "/api/v4/projects/g/p/issues/7/notes", got.Path)
		require.Equal(t, "hi", got.Body["body"])
		require.Equal(t, int64(9), res.Note.ID)
	})
}

func TestCreateIssue(t *testing.T) {
	t.Run("requires a title", func(t *testing.T) {
		called := false
		c := newTestClient(t, Config{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

		_, _, err := createIssueHandler(c)(context.Background(), &mcp.CallToolRequest{}, CreateIssueArgs{Project: "g/p"})
		require.ErrorContains(t, err, "title is required")
		require.False(t, called)
	})

	t.Run("creates with labels and confidentiality", func(t *testing.T) {
		var got gotRequest
		c := newTestClient(t, Config{}, recordJSON(t, &got, `{"id": 100, "iid": 7, "title": "T", "state": "opened"}`))

		_, res, err := createIssueHandler(c)(context.Background(), &mcp.CallToolRequest{}, CreateIssueArgs{
			Project: "g/p", Title: "T", Description: "body",
			Labels: []string{"bug", "ux"}, Confidential: true,
		})
		require.NoError(t, err)
		require.Equal(t, http.MethodPost, got.Method)
		require.Equal(t, "/api/v4/projects/g/p/issues", got.Path)
		require.Equal(t, "T", got.Body["title"])
		require.Equal(t, "body", got.Body["description"])
		require.Equal(t, "bug,ux", got.Body["labels"])
		require.Equal(t, true, got.Body["confidential"])
		require.Equal(t, int64(7), res.Issue.IID)
	})

	// Usernames and milestone titles are what an agent has; the API wants IDs.
	t.Run("resolves assignee usernames and a milestone title to ids", func(t *testing.T) {
		var body map[string]any
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v4/users":
				switch r.URL.Query().Get("username") {
				case "alice":
					_, _ = w.Write([]byte(`[{"id": 11, "username": "alice"}]`))
				case "bob":
					_, _ = w.Write([]byte(`[{"id": 22, "username": "bob"}]`))
				default:
					t.Errorf("unexpected username %s", r.URL.Query().Get("username"))
				}
			case "/api/v4/projects/g/p/milestones":
				require.Equal(t, "v1.0", r.URL.Query().Get("title"))
				require.Equal(t, "true", r.URL.Query().Get("include_ancestors"), "a group milestone must resolve too")
				_, _ = w.Write([]byte(`[{"id": 99, "title": "v1.0"}]`))
			case "/api/v4/projects/g/p/issues":
				require.NoError(t, decodeJSON(r, &body))
				_, _ = w.Write([]byte(`{"id": 100, "iid": 7, "title": "T"}`))
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
			}
		}))

		_, _, err := createIssueHandler(c)(context.Background(), &mcp.CallToolRequest{}, CreateIssueArgs{
			Project: "g/p", Title: "T",
			Assignees: []string{"alice", "@bob"}, Milestone: "v1.0",
		})
		require.NoError(t, err)
		require.Equal(t, []any{float64(11), float64(22)}, body["assignee_ids"], "a leading @ must be tolerated")
		require.Equal(t, float64(99), body["milestone_id"])
	})

	t.Run("an unknown milestone is an error, not a silently dropped field", func(t *testing.T) {
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			require.Equal(t, "/api/v4/projects/g/p/milestones", r.URL.Path)
			_, _ = w.Write([]byte(`[]`))
		}))

		_, _, err := createIssueHandler(c)(context.Background(), &mcp.CallToolRequest{}, CreateIssueArgs{
			Project: "g/p", Title: "T", Milestone: "nope",
		})
		require.ErrorContains(t, err, `no such milestone in g/p: "nope"`)
	})
}

func TestUpdateIssueFieldMapping(t *testing.T) {
	for _, tt := range []struct {
		name string
		args UpdateIssueArgs
		want map[string]any
	}{
		{"title", UpdateIssueArgs{Title: "T"}, map[string]any{"title": "T"}},
		{"description", UpdateIssueArgs{Description: "d"}, map[string]any{"description": "d"}},
		{"labels replace", UpdateIssueArgs{Labels: []string{"a", "b"}}, map[string]any{"labels": "a,b"}},
		{"remove_labels", UpdateIssueArgs{RemoveLabels: []string{"a"}}, map[string]any{"remove_labels": "a"}},
		{"reopen", UpdateIssueArgs{State: "reopen"}, map[string]any{"state_event": "reopen"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got gotRequest
			c := newTestClient(t, Config{}, recordJSON(t, &got, `{"id": 100, "iid": 7, "title": "T"}`))

			args := tt.args
			args.Project, args.IID = "g/p", 7
			_, _, err := updateIssueHandler(c)(context.Background(), &mcp.CallToolRequest{}, args)
			require.NoError(t, err)
			require.Equal(t, "/api/v4/projects/g/p/issues/7", got.Path)
			for k, want := range tt.want {
				require.Equal(t, want, got.Body[k], "field %q", k)
			}
		})
	}

	t.Run("resolves assignee usernames", func(t *testing.T) {
		var body map[string]any
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v4/users":
				_, _ = w.Write([]byte(`[{"id": 11, "username": "alice"}]`))
			default:
				require.NoError(t, decodeJSON(r, &body))
				_, _ = w.Write([]byte(`{"id": 100, "iid": 7, "title": "T"}`))
			}
		}))

		_, _, err := updateIssueHandler(c)(context.Background(), &mcp.CallToolRequest{}, UpdateIssueArgs{
			Project: "g/p", IID: 7, Assignees: []string{"alice"},
		})
		require.NoError(t, err)
		require.Equal(t, []any{float64(11)}, body["assignee_ids"])
	})
}
