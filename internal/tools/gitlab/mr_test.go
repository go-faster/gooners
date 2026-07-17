package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestListMergeRequests(t *testing.T) {
	t.Run("maps filters onto the API query", func(t *testing.T) {
		var got gotRequest
		c := newTestClient(t, Config{}, recordJSON(t, &got, `[
			{"id": 1, "iid": 3, "title": "Add thing", "state": "opened", "draft": true,
			 "source_branch": "feat", "target_branch": "main",
			 "author": {"username": "alice"},
			 "reviewers": [{"username": "bob"}]}
		]`))

		_, res, err := listMergeRequestsHandler(c)(context.Background(), &mcp.CallToolRequest{}, ListMergeRequestsArgs{
			Project:      "g/p",
			State:        "opened",
			Reviewer:     "bob",
			TargetBranch: "main",
		})
		require.NoError(t, err)
		require.Equal(t, "/api/v4/projects/g/p/merge_requests", got.Path)
		require.Equal(t, "opened", got.Query.Get("state"))
		require.Equal(t, "bob", got.Query.Get("reviewer_username"))
		require.Equal(t, "main", got.Query.Get("target_branch"))

		require.Len(t, res.MergeRequests, 1)
		require.True(t, res.MergeRequests[0].Draft)
		require.Equal(t, []UserSummary{{Username: "bob"}}, res.MergeRequests[0].Reviewers)
	})

	// This endpoint has no assignee_username, unlike the issue one, so the
	// handler has to resolve the name first.
	t.Run("resolves an assignee username to an id", func(t *testing.T) {
		var mrQuery string
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v4/users":
				require.Equal(t, "bob", r.URL.Query().Get("username"))
				_, _ = w.Write([]byte(`[{"id": 42, "username": "bob"}]`))
			case "/api/v4/projects/g/p/merge_requests":
				mrQuery = r.URL.Query().Get("assignee_id")
				_, _ = w.Write([]byte(`[]`))
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
			}
		}))

		_, _, err := listMergeRequestsHandler(c)(context.Background(), &mcp.CallToolRequest{}, ListMergeRequestsArgs{
			Project: "g/p", Assignee: "bob",
		})
		require.NoError(t, err)
		require.Equal(t, "42", mrQuery)
	})

	t.Run("an unknown assignee is an error, not a silent empty filter", func(t *testing.T) {
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/api/v4/users", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		}))

		_, _, err := listMergeRequestsHandler(c)(context.Background(), &mcp.CallToolRequest{}, ListMergeRequestsArgs{
			Project: "g/p", Assignee: "nobody",
		})
		require.ErrorContains(t, err, `no such user: "nobody"`)
	})
}

func TestMergeRequestDiff(t *testing.T) {
	diffsResponse := func(t *testing.T, diffs []map[string]any) http.HandlerFunc {
		t.Helper()
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(diffs))
		}
	}

	t.Run("returns every file's diff when it fits", func(t *testing.T) {
		c := newTestClient(t, Config{}, diffsResponse(t, []map[string]any{
			{"old_path": "a.go", "new_path": "a.go", "diff": "@@ -1 +1 @@\n-x\n+y\n"},
			{"old_path": "b.go", "new_path": "b.go", "diff": "@@ -2 +2 @@\n-p\n+q\n", "new_file": true},
		}))

		_, res, err := mergeRequestDiffHandler(c)(context.Background(), &mcp.CallToolRequest{}, MergeRequestDiffArgs{
			Project: "g/p", IID: 3,
		})
		require.NoError(t, err)
		require.Len(t, res.Diffs, 2)
		require.False(t, res.Truncated)
		require.Contains(t, res.Diffs[0].Diff, "+y")
		require.True(t, res.Diffs[1].NewFile)
	})

	t.Run("paths narrow the result", func(t *testing.T) {
		c := newTestClient(t, Config{}, diffsResponse(t, []map[string]any{
			{"old_path": "a.go", "new_path": "a.go", "diff": "one"},
			{"old_path": "b.go", "new_path": "b.go", "diff": "two"},
		}))

		_, res, err := mergeRequestDiffHandler(c)(context.Background(), &mcp.CallToolRequest{}, MergeRequestDiffArgs{
			Project: "g/p", IID: 3, Paths: []string{"b.go"},
		})
		require.NoError(t, err)
		require.Len(t, res.Diffs, 1)
		require.Equal(t, "b.go", res.Diffs[0].NewPath)
	})

	t.Run("a renamed file matches on its old path too", func(t *testing.T) {
		c := newTestClient(t, Config{}, diffsResponse(t, []map[string]any{
			{"old_path": "old.go", "new_path": "new.go", "diff": "d", "renamed_file": true},
		}))

		_, res, err := mergeRequestDiffHandler(c)(context.Background(), &mcp.CallToolRequest{}, MergeRequestDiffArgs{
			Project: "g/p", IID: 3, Paths: []string{"old.go"},
		})
		require.NoError(t, err)
		require.Len(t, res.Diffs, 1)
		require.True(t, res.Diffs[0].RenamedFile)
	})

	// The point of the budget is that the file list survives it: an agent that
	// sees a name can ask for that file, one that never saw it cannot.
	t.Run("over budget the names stay and the text goes", func(t *testing.T) {
		var diffs []map[string]any
		for i := range 5 {
			diffs = append(diffs, map[string]any{
				"old_path": fmt.Sprintf("f%d.go", i),
				"new_path": fmt.Sprintf("f%d.go", i),
				"diff":     strings.Repeat("x", diffLimit/2),
			})
		}
		c := newTestClient(t, Config{}, diffsResponse(t, diffs))

		_, res, err := mergeRequestDiffHandler(c)(context.Background(), &mcp.CallToolRequest{}, MergeRequestDiffArgs{
			Project: "g/p", IID: 3,
		})
		require.NoError(t, err)
		require.Len(t, res.Diffs, 5, "every file must be listed")
		require.True(t, res.Truncated)

		var withText, omitted int
		for _, d := range res.Diffs {
			require.NotEmpty(t, d.NewPath)
			if d.Omitted {
				omitted++
				require.Empty(t, d.Diff)
			} else {
				withText++
			}
		}
		require.Equal(t, 2, withText, "two half-budget diffs fit")
		require.Equal(t, 3, omitted)
	})

	t.Run("no matching paths yields an empty list, not null", func(t *testing.T) {
		c := newTestClient(t, Config{}, diffsResponse(t, []map[string]any{
			{"old_path": "a.go", "new_path": "a.go", "diff": "one"},
		}))

		_, res, err := mergeRequestDiffHandler(c)(context.Background(), &mcp.CallToolRequest{}, MergeRequestDiffArgs{
			Project: "g/p", IID: 3, Paths: []string{"nope.go"},
		})
		require.NoError(t, err)
		require.NotNil(t, res.Diffs)
		require.Empty(t, res.Diffs)
	})

	t.Run("requires an iid", func(t *testing.T) {
		c := newTestClient(t, Config{}, recordJSON(t, &gotRequest{}, `[]`))
		_, _, err := mergeRequestDiffHandler(c)(context.Background(), &mcp.CallToolRequest{}, MergeRequestDiffArgs{Project: "g/p"})
		require.ErrorContains(t, err, "iid is required")
	})
}

func TestCreateMergeRequest(t *testing.T) {
	t.Run("defaults the target to the project's default branch", func(t *testing.T) {
		var body map[string]any
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/api/v4/projects/g/p" && r.Method == http.MethodGet:
				_, _ = w.Write([]byte(`{"id": 1, "default_branch": "trunk"}`))
			case r.URL.Path == "/api/v4/projects/g/p/merge_requests":
				_ = json.NewDecoder(r.Body).Decode(&body)
				_, _ = w.Write([]byte(`{"id": 1, "iid": 3, "title": "T", "target_branch": "trunk"}`))
			default:
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			}
		}))

		_, res, err := createMergeRequestHandler(c)(context.Background(), &mcp.CallToolRequest{}, CreateMergeRequestArgs{
			Project: "g/p", SourceBranch: "feat", Title: "T",
		})
		require.NoError(t, err)
		require.Equal(t, "trunk", body["target_branch"])
		require.Equal(t, int64(3), res.MergeRequest.IID)
	})

	t.Run("an explicit target skips the project lookup", func(t *testing.T) {
		var paths []string
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": 1, "iid": 3, "title": "T"}`))
		}))

		_, _, err := createMergeRequestHandler(c)(context.Background(), &mcp.CallToolRequest{}, CreateMergeRequestArgs{
			Project: "g/p", SourceBranch: "feat", TargetBranch: "main", Title: "T",
		})
		require.NoError(t, err)
		require.Equal(t, []string{"/api/v4/projects/g/p/merge_requests"}, paths)
	})

	t.Run("requires a source branch", func(t *testing.T) {
		c := newTestClient(t, Config{}, recordJSON(t, &gotRequest{}, `{}`))
		_, _, err := createMergeRequestHandler(c)(context.Background(), &mcp.CallToolRequest{}, CreateMergeRequestArgs{
			Project: "g/p", Title: "T",
		})
		require.ErrorContains(t, err, "source_branch is required")
	})
}

func TestUpdateMergeRequest(t *testing.T) {
	t.Run("rejects an empty update", func(t *testing.T) {
		called := false
		c := newTestClient(t, Config{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

		_, _, err := updateMergeRequestHandler(c)(context.Background(), &mcp.CallToolRequest{}, UpdateMergeRequestArgs{
			Project: "g/p", IID: 3,
		})
		require.ErrorContains(t, err, "no changes requested")
		require.False(t, called)
	})

	t.Run("retargets", func(t *testing.T) {
		var got gotRequest
		c := newTestClient(t, Config{}, recordJSON(t, &got, `{"id": 1, "iid": 3, "title": "T"}`))

		_, _, err := updateMergeRequestHandler(c)(context.Background(), &mcp.CallToolRequest{}, UpdateMergeRequestArgs{
			Project: "g/p", IID: 3, TargetBranch: "release",
		})
		require.NoError(t, err)
		require.Equal(t, "release", got.Body["target_branch"])
	})
}

func TestViewMergeRequest(t *testing.T) {
	const mrJSON = `{"id": 1, "iid": 3, "title": "Add thing", "state": "opened",
		"description": "why", "source_branch": "feat", "target_branch": "main",
		"detailed_merge_status": "ci_still_running", "has_conflicts": false,
		"changes_count": "4", "sha": "abc123",
		"head_pipeline": {"id": 77, "status": "running", "ref": "feat", "web_url": "https://x/p/77"}}`

	t.Run("reports merge status and head pipeline", func(t *testing.T) {
		var paths []string
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(mrJSON))
		}))

		_, res, err := viewMergeRequestHandler(c)(context.Background(), &mcp.CallToolRequest{}, ViewMergeRequestArgs{
			Project: "g/p", IID: 3,
		})
		require.NoError(t, err)
		require.Equal(t, "why", res.MergeRequest.Description)
		require.Equal(t, "ci_still_running", res.MergeRequest.DetailedMergeStatus)
		require.Equal(t, "abc123", res.MergeRequest.SHA)
		require.Equal(t, "4", res.MergeRequest.ChangesCount)
		require.NotNil(t, res.MergeRequest.Pipeline)
		require.Equal(t, "running", res.MergeRequest.Pipeline.Status)

		// Approvals and notes each cost a call, so neither happens by default.
		require.Nil(t, res.MergeRequest.Approvals)
		require.Empty(t, res.MergeRequest.Notes)
		require.Equal(t, []string{"/api/v4/projects/g/p/merge_requests/3"}, paths)
	})

	t.Run("fetches approvals when asked", func(t *testing.T) {
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v4/projects/g/p/merge_requests/3":
				_, _ = w.Write([]byte(mrJSON))
			case "/api/v4/projects/g/p/merge_requests/3/approvals":
				_, _ = w.Write([]byte(`{"id": 1, "iid": 3, "approved": false, "approvals_left": 1,
					"approved_by": [{"user": {"username": "alice", "name": "Alice"}}]}`))
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
			}
		}))

		_, res, err := viewMergeRequestHandler(c)(context.Background(), &mcp.CallToolRequest{}, ViewMergeRequestArgs{
			Project: "g/p", IID: 3, WithApprovals: true,
		})
		require.NoError(t, err)
		require.NotNil(t, res.MergeRequest.Approvals)
		require.False(t, res.MergeRequest.Approvals.Approved)
		require.Equal(t, int64(1), res.MergeRequest.Approvals.ApprovalsLeft)
		require.Equal(t, []UserSummary{{Username: "alice", Name: "Alice"}}, res.MergeRequest.Approvals.ApprovedBy)
	})

	t.Run("fetches notes when asked", func(t *testing.T) {
		c := newTestClient(t, Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/notes") {
				_, _ = w.Write([]byte(`[{"id": 1, "body": "lgtm", "author": {"username": "bob"}}]`))
				return
			}
			_, _ = w.Write([]byte(mrJSON))
		}))

		_, res, err := viewMergeRequestHandler(c)(context.Background(), &mcp.CallToolRequest{}, ViewMergeRequestArgs{
			Project: "g/p", IID: 3, WithNotes: true,
		})
		require.NoError(t, err)
		require.Len(t, res.MergeRequest.Notes, 1)
		require.Equal(t, "lgtm", res.MergeRequest.Notes[0].Body)
	})

	t.Run("requires an iid", func(t *testing.T) {
		c := newTestClient(t, Config{}, recordJSON(t, &gotRequest{}, `{}`))
		_, _, err := viewMergeRequestHandler(c)(context.Background(), &mcp.CallToolRequest{}, ViewMergeRequestArgs{Project: "g/p"})
		require.ErrorContains(t, err, "iid is required")
	})
}

func TestCreateMergeRequestNote(t *testing.T) {
	t.Run("posts the comment", func(t *testing.T) {
		var got gotRequest
		c := newTestClient(t, Config{}, recordJSON(t, &got, `{"id": 9, "body": "nit", "author": {"username": "alice"}}`))

		_, res, err := createMergeRequestNoteHandler(c)(context.Background(), &mcp.CallToolRequest{}, CreateMergeRequestNoteArgs{
			Project: "g/p", IID: 3, Body: "nit",
		})
		require.NoError(t, err)
		require.Equal(t, http.MethodPost, got.Method)
		require.Equal(t, "/api/v4/projects/g/p/merge_requests/3/notes", got.Path)
		require.Equal(t, "nit", got.Body["body"])
		require.Equal(t, int64(9), res.Note.ID)
	})

	t.Run("requires a body", func(t *testing.T) {
		c := newTestClient(t, Config{}, recordJSON(t, &gotRequest{}, `{}`))
		_, _, err := createMergeRequestNoteHandler(c)(context.Background(), &mcp.CallToolRequest{}, CreateMergeRequestNoteArgs{
			Project: "g/p", IID: 3,
		})
		require.ErrorContains(t, err, "body is required")
	})
}

// Each optional argument is a separate branch mapping onto an API field, and a
// mis-mapped field is a silent bug: the call succeeds and the change is lost.
func TestUpdateMergeRequestFieldMapping(t *testing.T) {
	for _, tt := range []struct {
		name string
		args UpdateMergeRequestArgs
		want map[string]any
	}{
		{"title", UpdateMergeRequestArgs{Title: "T"}, map[string]any{"title": "T"}},
		{"description", UpdateMergeRequestArgs{Description: "d"}, map[string]any{"description": "d"}},
		{"labels replace", UpdateMergeRequestArgs{Labels: []string{"a", "b"}}, map[string]any{"labels": "a,b"}},
		{"add_labels", UpdateMergeRequestArgs{AddLabels: []string{"a"}}, map[string]any{"add_labels": "a"}},
		{"remove_labels", UpdateMergeRequestArgs{RemoveLabels: []string{"a"}}, map[string]any{"remove_labels": "a"}},
		{"unassign", UpdateMergeRequestArgs{Unassign: true}, map[string]any{"assignee_ids": []any{}}},
		{"close", UpdateMergeRequestArgs{State: "close"}, map[string]any{"state_event": "close"}},
		{"reopen", UpdateMergeRequestArgs{State: "reopen"}, map[string]any{"state_event": "reopen"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got gotRequest
			c := newTestClient(t, Config{}, recordJSON(t, &got, `{"id": 1, "iid": 3, "title": "T"}`))

			args := tt.args
			args.Project, args.IID = "g/p", 3
			_, _, err := updateMergeRequestHandler(c)(context.Background(), &mcp.CallToolRequest{}, args)
			require.NoError(t, err)
			require.Equal(t, "/api/v4/projects/g/p/merge_requests/3", got.Path)
			for k, want := range tt.want {
				require.Equal(t, want, got.Body[k], "field %q", k)
			}
		})
	}

	t.Run("unassign wins over an assignees list", func(t *testing.T) {
		var got gotRequest
		c := newTestClient(t, Config{}, recordJSON(t, &got, `{"id": 1, "iid": 3, "title": "T"}`))

		_, _, err := updateMergeRequestHandler(c)(context.Background(), &mcp.CallToolRequest{}, UpdateMergeRequestArgs{
			Project: "g/p", IID: 3, Unassign: true, Assignees: []string{"alice"},
		})
		require.NoError(t, err)
		require.Equal(t, []any{}, got.Body["assignee_ids"], "no user lookup should happen")
	})

	t.Run("rejects an unknown state", func(t *testing.T) {
		c := newTestClient(t, Config{}, recordJSON(t, &gotRequest{}, `{}`))
		_, _, err := updateMergeRequestHandler(c)(context.Background(), &mcp.CallToolRequest{}, UpdateMergeRequestArgs{
			Project: "g/p", IID: 3, State: "merge",
		})
		require.ErrorContains(t, err, "state must be close or reopen")
	})
}

func TestCreateMergeRequestFieldMapping(t *testing.T) {
	var got gotRequest
	c := newTestClient(t, Config{}, recordJSON(t, &got, `{"id": 1, "iid": 3, "title": "T"}`))

	_, _, err := createMergeRequestHandler(c)(context.Background(), &mcp.CallToolRequest{}, CreateMergeRequestArgs{
		Project: "g/p", SourceBranch: "feat", TargetBranch: "main", Title: "Draft: T",
		Description: "body", Labels: []string{"a", "b"}, RemoveSource: true, Squash: true,
	})
	require.NoError(t, err)
	require.Equal(t, "feat", got.Body["source_branch"])
	require.Equal(t, "main", got.Body["target_branch"])
	require.Equal(t, "Draft: T", got.Body["title"])
	require.Equal(t, "body", got.Body["description"])
	require.Equal(t, "a,b", got.Body["labels"])
	require.Equal(t, true, got.Body["remove_source_branch"])
	require.Equal(t, true, got.Body["squash"])
}
