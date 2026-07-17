package gitlab

import (
	"context"
	"time"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	gl "gitlab.com/gitlab-org/api/client-go/v2"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

// diffLimit caps the total diff text mr_diff returns. A refactor touching
// hundreds of files is exactly the merge request an agent is asked to review,
// and it is also the one that would blow the context in a single call.
const diffLimit = 100000

// MergeRequestSummary is the compact list view of a merge request.
type MergeRequestSummary struct {
	IID          int64         `json:"iid" jsonschema:"project-scoped merge request number, the one shown as !123"`
	Title        string        `json:"title"`
	State        string        `json:"state" jsonschema:"opened, closed, merged, or locked"`
	Draft        bool          `json:"draft,omitempty"`
	SourceBranch string        `json:"source_branch"`
	TargetBranch string        `json:"target_branch"`
	Author       *UserSummary  `json:"author,omitempty"`
	Assignees    []UserSummary `json:"assignees,omitempty"`
	Reviewers    []UserSummary `json:"reviewers,omitempty"`
	Labels       []string      `json:"labels,omitempty"`
	Milestone    string        `json:"milestone,omitempty"`
	UpdatedAt    *time.Time    `json:"updated_at,omitempty"`
	WebURL       string        `json:"web_url,omitempty"`
}

// MergeRequestDetail adds the fields only worth fetching for a single merge
// request.
type MergeRequestDetail struct {
	MergeRequestSummary
	Description          string     `json:"description,omitempty"`
	DescriptionTruncated bool       `json:"description_truncated,omitempty"`
	CreatedAt            *time.Time `json:"created_at,omitempty"`
	MergedAt             *time.Time `json:"merged_at,omitempty"`
	ClosedAt             *time.Time `json:"closed_at,omitempty"`
	SHA                  string     `json:"sha,omitempty" jsonschema:"head commit of the source branch"`
	// DetailedMergeStatus is GitLab's own summary of whether the merge request
	// can merge, e.g. mergeable, ci_still_running, not_approved.
	DetailedMergeStatus string           `json:"detailed_merge_status,omitempty"`
	HasConflicts        bool             `json:"has_conflicts,omitempty"`
	ChangesCount        string           `json:"changes_count,omitempty"`
	Pipeline            *PipelineSummary `json:"pipeline,omitempty"`
	Approvals           *ApprovalSummary `json:"approvals,omitempty"`
	Notes               []NoteSummary    `json:"notes,omitempty"`
}

// PipelineSummary is the head pipeline's outcome, the part of a merge request
// an agent asks about most.
type PipelineSummary struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Ref    string `json:"ref,omitempty"`
	WebURL string `json:"web_url,omitempty"`
}

// ApprovalSummary is who has approved and how many more are needed.
type ApprovalSummary struct {
	Approved      bool          `json:"approved"`
	ApprovalsLeft int64         `json:"approvals_left"`
	ApprovedBy    []UserSummary `json:"approved_by,omitempty"`
}

func mergeRequestSummary(mr *gl.BasicMergeRequest) MergeRequestSummary {
	s := MergeRequestSummary{
		IID:          mr.IID,
		Title:        mr.Title,
		State:        mr.State,
		Draft:        mr.Draft,
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
		Author:       userSummary(mr.Author),
		Assignees:    userSummaries(mr.Assignees),
		Reviewers:    userSummaries(mr.Reviewers),
		Labels:       mr.Labels,
		UpdatedAt:    mr.UpdatedAt,
		WebURL:       mr.WebURL,
	}
	if mr.Milestone != nil {
		s.Milestone = mr.Milestone.Title
	}
	return s
}

type ListMergeRequestsArgs struct {
	ListArgs
	Project      string `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	State        string `json:"state,omitempty" jsonschema:"opened, closed, merged, locked, or all; defaults to opened"`
	Labels       string `json:"labels,omitempty" jsonschema:"comma-separated label names the merge request must have all of"`
	Author       string `json:"author,omitempty" jsonschema:"filter by author username"`
	Assignee     string `json:"assignee,omitempty" jsonschema:"filter by assignee username"`
	Reviewer     string `json:"reviewer,omitempty" jsonschema:"filter by reviewer username"`
	SourceBranch string `json:"source_branch,omitempty"`
	TargetBranch string `json:"target_branch,omitempty"`
	Milestone    string `json:"milestone,omitempty" jsonschema:"filter by milestone title"`
	Search       string `json:"search,omitempty" jsonschema:"free-text search over title and description"`
	OrderBy      string `json:"order_by,omitempty" jsonschema:"created_at or updated_at; defaults to created_at"`
	Sort         string `json:"sort,omitempty" jsonschema:"asc or desc; defaults to desc"`
}

type ListMergeRequestsRes struct {
	MergeRequests []MergeRequestSummary `json:"merge_requests"`
}

func listMergeRequestsHandler(c *Client) mcp.ToolHandlerFor[ListMergeRequestsArgs, ListMergeRequestsRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ListMergeRequestsArgs) (*mcp.CallToolResult, ListMergeRequestsRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, ListMergeRequestsRes{}, err
		}

		opt := &gl.ListProjectMergeRequestsOptions{ListOptions: args.ListArgs.listOptions()}
		if args.State != "" {
			opt.State = new(args.State)
		}
		if args.Labels != "" {
			opt.Labels = new(gl.LabelOptions(splitCSV(args.Labels)))
		}
		if args.Author != "" {
			opt.AuthorUsername = new(args.Author)
		}
		if args.Assignee != "" {
			// Unlike the issue list, this endpoint filters by assignee ID only,
			// so the username the caller has must be resolved first.
			ids, err := c.userIDs(ctx, []string{args.Assignee})
			if err != nil {
				return nil, ListMergeRequestsRes{}, err
			}
			opt.AssigneeID = gl.AssigneeID(ids[0])
		}
		if args.Reviewer != "" {
			opt.ReviewerUsername = new(args.Reviewer)
		}
		if args.SourceBranch != "" {
			opt.SourceBranch = new(args.SourceBranch)
		}
		if args.TargetBranch != "" {
			opt.TargetBranch = new(args.TargetBranch)
		}
		if args.Milestone != "" {
			opt.Milestone = new(args.Milestone)
		}
		if args.Search != "" {
			opt.Search = new(args.Search)
		}
		if args.OrderBy != "" {
			opt.OrderBy = new(args.OrderBy)
		}
		if args.Sort != "" {
			opt.Sort = new(args.Sort)
		}

		mrs, _, err := c.gl.MergeRequests.ListProjectMergeRequests(pid, opt, gl.WithContext(ctx))
		if err != nil {
			return nil, ListMergeRequestsRes{}, errors.Wrapf(err, "list merge requests of %s", pid)
		}

		res := ListMergeRequestsRes{MergeRequests: make([]MergeRequestSummary, 0, len(mrs))}
		for _, mr := range mrs {
			res.MergeRequests = append(res.MergeRequests, mergeRequestSummary(mr))
		}
		return nil, res, nil
	}
}

type ViewMergeRequestArgs struct {
	Project       string `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	IID           int64  `json:"iid" jsonschema:"project-scoped merge request number, the one shown as !123"`
	WithNotes     bool   `json:"with_notes,omitempty" jsonschema:"include the comment thread; off by default because it can be long"`
	WithApprovals bool   `json:"with_approvals,omitempty" jsonschema:"include who approved and how many approvals remain; costs one extra API call"`
}

type ViewMergeRequestRes struct {
	MergeRequest MergeRequestDetail `json:"merge_request"`
}

func viewMergeRequestHandler(c *Client) mcp.ToolHandlerFor[ViewMergeRequestArgs, ViewMergeRequestRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ViewMergeRequestArgs) (*mcp.CallToolResult, ViewMergeRequestRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, ViewMergeRequestRes{}, err
		}
		if args.IID <= 0 {
			return nil, ViewMergeRequestRes{}, errors.New("iid is required")
		}

		mr, _, err := c.gl.MergeRequests.GetMergeRequest(pid, args.IID, nil, gl.WithContext(ctx))
		if err != nil {
			return nil, ViewMergeRequestRes{}, errors.Wrapf(err, "get merge request %s!%d", pid, args.IID)
		}

		detail := MergeRequestDetail{
			MergeRequestSummary: mergeRequestSummary(&mr.BasicMergeRequest),
			CreatedAt:           mr.CreatedAt,
			MergedAt:            mr.MergedAt,
			ClosedAt:            mr.ClosedAt,
			SHA:                 mr.SHA,
			DetailedMergeStatus: mr.DetailedMergeStatus,
			HasConflicts:        mr.HasConflicts,
			ChangesCount:        mr.ChangesCount,
		}
		detail.Description, detail.DescriptionTruncated = truncate(mr.Description, descriptionLimit)
		if mr.HeadPipeline != nil {
			detail.Pipeline = &PipelineSummary{
				ID:     mr.HeadPipeline.ID,
				Status: mr.HeadPipeline.Status,
				Ref:    mr.HeadPipeline.Ref,
				WebURL: mr.HeadPipeline.WebURL,
			}
		}

		if args.WithApprovals {
			approvals, _, err := c.gl.MergeRequests.GetMergeRequestApprovals(pid, args.IID, gl.WithContext(ctx))
			if err != nil {
				return nil, ViewMergeRequestRes{}, errors.Wrapf(err, "get approvals of merge request %s!%d", pid, args.IID)
			}
			summary := &ApprovalSummary{
				Approved:      approvals.Approved,
				ApprovalsLeft: approvals.ApprovalsLeft,
			}
			for _, a := range approvals.ApprovedBy {
				if a != nil && a.User != nil {
					summary.ApprovedBy = append(summary.ApprovedBy, UserSummary{Username: a.User.Username, Name: a.User.Name})
				}
			}
			detail.Approvals = summary
		}

		if args.WithNotes {
			notes, _, err := c.gl.Notes.ListMergeRequestNotes(pid, args.IID, &gl.ListMergeRequestNotesOptions{
				ListOptions: gl.ListOptions{PerPage: maxPerPage},
				Sort:        new("asc"),
			}, gl.WithContext(ctx))
			if err != nil {
				return nil, ViewMergeRequestRes{}, errors.Wrapf(err, "list notes of merge request %s!%d", pid, args.IID)
			}
			for _, n := range notes {
				detail.Notes = append(detail.Notes, noteSummary(n))
			}
		}

		return nil, ViewMergeRequestRes{MergeRequest: detail}, nil
	}
}

// FileDiff is one file's change within a merge request.
type FileDiff struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	NewFile     bool   `json:"new_file,omitempty"`
	RenamedFile bool   `json:"renamed_file,omitempty"`
	DeletedFile bool   `json:"deleted_file,omitempty"`
	Generated   bool   `json:"generated_file,omitempty" jsonschema:"GitLab considers this file generated, e.g. by a linguist-generated gitattribute"`
	// Diff is unified diff text. It is empty when the file was omitted: either
	// GitLab itself declined to send it (TooLarge) or the response hit the
	// tool's own budget.
	Diff     string `json:"diff,omitempty"`
	TooLarge bool   `json:"too_large,omitempty" jsonschema:"GitLab did not return this file's diff because it exceeds the instance limit"`
	Omitted  bool   `json:"omitted,omitempty" jsonschema:"the diff was dropped to keep the response within budget; fetch this file with repo_file instead"`
}

type MergeRequestDiffArgs struct {
	ListArgs
	Project string `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	IID     int64  `json:"iid" jsonschema:"project-scoped merge request number, the one shown as !123"`
	// Paths narrows a wide merge request to the files under review, which is
	// what makes a 200-file change readable at all.
	Paths []string `json:"paths,omitempty" jsonschema:"only return diffs whose new or old path is exactly one of these"`
}

type MergeRequestDiffRes struct {
	Diffs []FileDiff `json:"diffs"`
	// Truncated reports that at least one diff was dropped for budget.
	Truncated bool `json:"truncated,omitempty"`
}

func mergeRequestDiffHandler(c *Client) mcp.ToolHandlerFor[MergeRequestDiffArgs, MergeRequestDiffRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args MergeRequestDiffArgs) (*mcp.CallToolResult, MergeRequestDiffRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, MergeRequestDiffRes{}, err
		}
		if args.IID <= 0 {
			return nil, MergeRequestDiffRes{}, errors.New("iid is required")
		}

		diffs, _, err := c.gl.MergeRequests.ListMergeRequestDiffs(pid, args.IID, &gl.ListMergeRequestDiffsOptions{
			ListOptions: args.ListArgs.listOptions(),
		}, gl.WithContext(ctx))
		if err != nil {
			return nil, MergeRequestDiffRes{}, errors.Wrapf(err, "list diffs of merge request %s!%d", pid, args.IID)
		}

		want := make(map[string]struct{}, len(args.Paths))
		for _, p := range args.Paths {
			want[p] = struct{}{}
		}

		var (
			res    MergeRequestDiffRes
			budget = diffLimit
		)
		for _, d := range diffs {
			if len(want) > 0 {
				_, okNew := want[d.NewPath]
				_, okOld := want[d.OldPath]
				if !okNew && !okOld {
					continue
				}
			}

			f := FileDiff{
				OldPath:     d.OldPath,
				NewPath:     d.NewPath,
				NewFile:     d.NewFile,
				RenamedFile: d.RenamedFile,
				DeletedFile: d.DeletedFile,
				Generated:   d.GeneratedFile,
				TooLarge:    d.TooLarge,
			}
			// The file list stays complete even once the budget runs out: an
			// agent that knows a file changed can ask for it by path, whereas
			// one that never saw the name cannot.
			switch n := len([]rune(d.Diff)); {
			case n <= budget:
				f.Diff = d.Diff
				budget -= n
			default:
				f.Omitted = true
				res.Truncated = true
			}
			res.Diffs = append(res.Diffs, f)
		}
		if res.Diffs == nil {
			res.Diffs = []FileDiff{}
		}
		return nil, res, nil
	}
}

type CreateMergeRequestArgs struct {
	Project      string   `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	SourceBranch string   `json:"source_branch" jsonschema:"branch holding the changes"`
	TargetBranch string   `json:"target_branch,omitempty" jsonschema:"branch to merge into; defaults to the project's default branch"`
	Title        string   `json:"title" jsonschema:"merge request title; prefix with Draft: to open it as a draft"`
	Description  string   `json:"description,omitempty" jsonschema:"merge request body, in GitLab-flavored Markdown"`
	Labels       []string `json:"labels,omitempty" jsonschema:"label names to apply"`
	Assignees    []string `json:"assignees,omitempty" jsonschema:"usernames to assign"`
	Reviewers    []string `json:"reviewers,omitempty" jsonschema:"usernames to request review from"`
	Milestone    string   `json:"milestone,omitempty" jsonschema:"milestone title to attach"`
	RemoveSource bool     `json:"remove_source_branch,omitempty" jsonschema:"delete the source branch once merged"`
	Squash       bool     `json:"squash,omitempty" jsonschema:"squash commits when merging"`
}

type MergeRequestRes struct {
	MergeRequest MergeRequestSummary `json:"merge_request"`
}

func createMergeRequestHandler(c *Client) mcp.ToolHandlerFor[CreateMergeRequestArgs, MergeRequestRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args CreateMergeRequestArgs) (*mcp.CallToolResult, MergeRequestRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, MergeRequestRes{}, err
		}
		if args.SourceBranch == "" {
			return nil, MergeRequestRes{}, errors.New("source_branch is required")
		}
		if args.Title == "" {
			return nil, MergeRequestRes{}, errors.New("title is required")
		}

		target := args.TargetBranch
		if target == "" {
			project, _, err := c.gl.Projects.GetProject(pid, nil, gl.WithContext(ctx))
			if err != nil {
				return nil, MergeRequestRes{}, errors.Wrapf(err, "get default branch of %s", pid)
			}
			target = project.DefaultBranch
		}

		opt := &gl.CreateMergeRequestOptions{
			SourceBranch: new(args.SourceBranch),
			TargetBranch: new(target),
			Title:        new(args.Title),
		}
		if args.Description != "" {
			opt.Description = new(args.Description)
		}
		if len(args.Labels) > 0 {
			opt.Labels = new(gl.LabelOptions(args.Labels))
		}
		if args.RemoveSource {
			opt.RemoveSourceBranch = new(true)
		}
		if args.Squash {
			opt.Squash = new(true)
		}
		if len(args.Assignees) > 0 {
			ids, err := c.userIDs(ctx, args.Assignees)
			if err != nil {
				return nil, MergeRequestRes{}, err
			}
			opt.AssigneeIDs = new(ids)
		}
		if len(args.Reviewers) > 0 {
			ids, err := c.userIDs(ctx, args.Reviewers)
			if err != nil {
				return nil, MergeRequestRes{}, err
			}
			opt.ReviewerIDs = new(ids)
		}
		if args.Milestone != "" {
			id, err := c.milestoneID(ctx, pid, args.Milestone)
			if err != nil {
				return nil, MergeRequestRes{}, err
			}
			opt.MilestoneID = new(id)
		}

		mr, _, err := c.gl.MergeRequests.CreateMergeRequest(pid, opt, gl.WithContext(ctx))
		if err != nil {
			return nil, MergeRequestRes{}, errors.Wrapf(err, "create merge request in %s", pid)
		}
		return nil, MergeRequestRes{MergeRequest: mergeRequestSummary(&mr.BasicMergeRequest)}, nil
	}
}

type UpdateMergeRequestArgs struct {
	Project     string `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	IID         int64  `json:"iid" jsonschema:"project-scoped merge request number, the one shown as !123"`
	Title       string `json:"title,omitempty" jsonschema:"replace the title"`
	Description string `json:"description,omitempty" jsonschema:"replace the body; this overwrites, it does not append"`
	// As with issues, Labels replaces while AddLabels and RemoveLabels edit.
	Labels       []string `json:"labels,omitempty" jsonschema:"replace all labels with this set"`
	AddLabels    []string `json:"add_labels,omitempty" jsonschema:"add these labels, leaving existing ones"`
	RemoveLabels []string `json:"remove_labels,omitempty" jsonschema:"remove these labels, leaving the rest"`
	Assignees    []string `json:"assignees,omitempty" jsonschema:"replace assignees with these usernames; an empty list is ignored, use unassign"`
	Unassign     bool     `json:"unassign,omitempty" jsonschema:"remove all assignees"`
	Reviewers    []string `json:"reviewers,omitempty" jsonschema:"replace reviewers with these usernames"`
	TargetBranch string   `json:"target_branch,omitempty" jsonschema:"retarget the merge request at another branch"`
	Milestone    string   `json:"milestone,omitempty" jsonschema:"milestone title to attach"`
	State        string   `json:"state,omitempty" jsonschema:"close or reopen"`
}

func updateMergeRequestHandler(c *Client) mcp.ToolHandlerFor[UpdateMergeRequestArgs, MergeRequestRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args UpdateMergeRequestArgs) (*mcp.CallToolResult, MergeRequestRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, MergeRequestRes{}, err
		}
		if args.IID <= 0 {
			return nil, MergeRequestRes{}, errors.New("iid is required")
		}

		opt := &gl.UpdateMergeRequestOptions{}
		empty := true
		if args.Title != "" {
			opt.Title = new(args.Title)
			empty = false
		}
		if args.Description != "" {
			opt.Description = new(args.Description)
			empty = false
		}
		if len(args.Labels) > 0 {
			opt.Labels = new(gl.LabelOptions(args.Labels))
			empty = false
		}
		if len(args.AddLabels) > 0 {
			opt.AddLabels = new(gl.LabelOptions(args.AddLabels))
			empty = false
		}
		if len(args.RemoveLabels) > 0 {
			opt.RemoveLabels = new(gl.LabelOptions(args.RemoveLabels))
			empty = false
		}
		if args.TargetBranch != "" {
			opt.TargetBranch = new(args.TargetBranch)
			empty = false
		}
		switch {
		case args.Unassign:
			opt.AssigneeIDs = new([]int64{})
			empty = false
		case len(args.Assignees) > 0:
			ids, err := c.userIDs(ctx, args.Assignees)
			if err != nil {
				return nil, MergeRequestRes{}, err
			}
			opt.AssigneeIDs = new(ids)
			empty = false
		}
		if len(args.Reviewers) > 0 {
			ids, err := c.userIDs(ctx, args.Reviewers)
			if err != nil {
				return nil, MergeRequestRes{}, err
			}
			opt.ReviewerIDs = new(ids)
			empty = false
		}
		if args.Milestone != "" {
			id, err := c.milestoneID(ctx, pid, args.Milestone)
			if err != nil {
				return nil, MergeRequestRes{}, err
			}
			opt.MilestoneID = new(id)
			empty = false
		}
		if args.State != "" {
			switch args.State {
			case "close", "reopen":
				opt.StateEvent = new(args.State)
				empty = false
			default:
				return nil, MergeRequestRes{}, errors.Errorf("state must be close or reopen, got %q", args.State)
			}
		}
		if empty {
			return nil, MergeRequestRes{}, errors.New("no changes requested: set at least one of title, description, labels, add_labels, remove_labels, assignees, unassign, reviewers, target_branch, milestone, state")
		}

		mr, _, err := c.gl.MergeRequests.UpdateMergeRequest(pid, args.IID, opt, gl.WithContext(ctx))
		if err != nil {
			return nil, MergeRequestRes{}, errors.Wrapf(err, "update merge request %s!%d", pid, args.IID)
		}
		return nil, MergeRequestRes{MergeRequest: mergeRequestSummary(&mr.BasicMergeRequest)}, nil
	}
}

type CreateMergeRequestNoteArgs struct {
	Project string `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	IID     int64  `json:"iid" jsonschema:"project-scoped merge request number, the one shown as !123"`
	Body    string `json:"body" jsonschema:"comment text, in GitLab-flavored Markdown"`
}

func createMergeRequestNoteHandler(c *Client) mcp.ToolHandlerFor[CreateMergeRequestNoteArgs, NoteRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args CreateMergeRequestNoteArgs) (*mcp.CallToolResult, NoteRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, NoteRes{}, err
		}
		if args.IID <= 0 {
			return nil, NoteRes{}, errors.New("iid is required")
		}
		if args.Body == "" {
			return nil, NoteRes{}, errors.New("body is required")
		}

		note, _, err := c.gl.Notes.CreateMergeRequestNote(pid, args.IID, &gl.CreateMergeRequestNoteOptions{
			Body: new(args.Body),
		}, gl.WithContext(ctx))
		if err != nil {
			return nil, NoteRes{}, errors.Wrapf(err, "comment on merge request %s!%d", pid, args.IID)
		}
		return nil, NoteRes{Note: noteSummary(note)}, nil
	}
}

func registerMergeRequestTools(s *mcp.Server, c *Client) {
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "mr_list",
		Description: "Lists merge requests of a GitLab project, filtered by state, labels, author, assignee, reviewer, branch, milestone or free-text search.",
		Flags:       mcputil.ReadOnly,
	}, listMergeRequestsHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "mr_view",
		Description: "Reads one GitLab merge request by its project-scoped number: description, merge status, head pipeline, and optionally approvals and the comment thread.",
		Flags:       mcputil.ReadOnly,
	}, viewMergeRequestHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "mr_diff",
		Description: "Returns the per-file diffs of a GitLab merge request. Pass paths to narrow a wide change; the file list is always complete even when diff text is dropped to fit.",
		Flags:       mcputil.ReadOnly,
	}, mergeRequestDiffHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "mr_create",
		Description: "Opens a GitLab merge request from a source branch.",
	}, createMergeRequestHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "mr_update",
		Description: "Updates a GitLab merge request: title, description, labels, assignees, reviewers, target branch, milestone, or open/closed state. It cannot merge or approve.",
	}, updateMergeRequestHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "mr_note",
		Description: "Posts a comment on a GitLab merge request. This is a general discussion note, not an inline diff comment.",
	}, createMergeRequestNoteHandler(c))
}
