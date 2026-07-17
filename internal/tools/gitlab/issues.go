package gitlab

import (
	"context"
	"time"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	gl "gitlab.com/gitlab-org/api/client-go/v2"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

// descriptionLimit caps an issue or merge request body. Long-form descriptions
// with embedded logs are common and would otherwise crowd out everything else.
const descriptionLimit = 20000

// IssueSummary is the compact list view of an issue.
type IssueSummary struct {
	IID       int64         `json:"iid" jsonschema:"project-scoped issue number, the one shown as #123"`
	Title     string        `json:"title"`
	State     string        `json:"state"`
	Author    *UserSummary  `json:"author,omitempty"`
	Assignees []UserSummary `json:"assignees,omitempty"`
	Labels    []string      `json:"labels,omitempty"`
	Milestone string        `json:"milestone,omitempty"`
	UpdatedAt *time.Time    `json:"updated_at,omitempty"`
	WebURL    string        `json:"web_url,omitempty"`
}

// IssueDetail adds the fields only worth fetching for a single issue.
type IssueDetail struct {
	IssueSummary
	Description string `json:"description,omitempty"`
	// DescriptionTruncated reports that Description was cut at
	// [descriptionLimit] runes.
	DescriptionTruncated bool          `json:"description_truncated,omitempty"`
	Confidential         bool          `json:"confidential,omitempty"`
	CreatedAt            *time.Time    `json:"created_at,omitempty"`
	ClosedAt             *time.Time    `json:"closed_at,omitempty"`
	Notes                []NoteSummary `json:"notes,omitempty"`
}

func issueSummary(i *gl.Issue) IssueSummary {
	s := IssueSummary{
		IID:       i.IID,
		Title:     i.Title,
		State:     i.State,
		Labels:    i.Labels,
		UpdatedAt: i.UpdatedAt,
		WebURL:    i.WebURL,
	}
	if i.Author != nil {
		s.Author = &UserSummary{Username: i.Author.Username, Name: i.Author.Name}
	}
	for _, a := range i.Assignees {
		if a != nil {
			s.Assignees = append(s.Assignees, UserSummary{Username: a.Username, Name: a.Name})
		}
	}
	if i.Milestone != nil {
		s.Milestone = i.Milestone.Title
	}
	return s
}

type ListIssuesArgs struct {
	ListArgs
	Project string `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	State   string `json:"state,omitempty" jsonschema:"opened, closed, or all; defaults to opened"`
	Labels  string `json:"labels,omitempty" jsonschema:"comma-separated label names the issue must have all of"`
	// Assignee and Author take usernames because that is what an agent reading
	// an issue thread has; the API's *_id variants would need a lookup first.
	Assignee  string `json:"assignee,omitempty" jsonschema:"filter by assignee username"`
	Author    string `json:"author,omitempty" jsonschema:"filter by author username"`
	Milestone string `json:"milestone,omitempty" jsonschema:"filter by milestone title"`
	Search    string `json:"search,omitempty" jsonschema:"free-text search over title and description"`
	OrderBy   string `json:"order_by,omitempty" jsonschema:"created_at, updated_at, or priority; defaults to created_at"`
	Sort      string `json:"sort,omitempty" jsonschema:"asc or desc; defaults to desc"`
}

type ListIssuesRes struct {
	Issues []IssueSummary `json:"issues"`
}

func listIssuesHandler(c *Client) mcp.ToolHandlerFor[ListIssuesArgs, ListIssuesRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ListIssuesArgs) (*mcp.CallToolResult, ListIssuesRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, ListIssuesRes{}, err
		}

		opt := &gl.ListProjectIssuesOptions{ListOptions: args.ListArgs.listOptions()}
		if args.State != "" {
			opt.State = new(args.State)
		}
		if args.Labels != "" {
			opt.Labels = new(gl.LabelOptions(splitCSV(args.Labels)))
		}
		if args.Assignee != "" {
			opt.AssigneeUsername = new(args.Assignee)
		}
		if args.Author != "" {
			opt.AuthorUsername = new(args.Author)
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

		issues, _, err := c.gl.Issues.ListProjectIssues(pid, opt, gl.WithContext(ctx))
		if err != nil {
			return nil, ListIssuesRes{}, errors.Wrapf(err, "list issues of %s", pid)
		}

		res := ListIssuesRes{Issues: make([]IssueSummary, 0, len(issues))}
		for _, i := range issues {
			res.Issues = append(res.Issues, issueSummary(i))
		}
		return nil, res, nil
	}
}

type ViewIssueArgs struct {
	Project   string `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	IID       int64  `json:"iid" jsonschema:"project-scoped issue number, the one shown as #123"`
	WithNotes bool   `json:"with_notes,omitempty" jsonschema:"include the comment thread; off by default because it can be long"`
}

type ViewIssueRes struct {
	Issue IssueDetail `json:"issue"`
}

func viewIssueHandler(c *Client) mcp.ToolHandlerFor[ViewIssueArgs, ViewIssueRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ViewIssueArgs) (*mcp.CallToolResult, ViewIssueRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, ViewIssueRes{}, err
		}
		if args.IID <= 0 {
			return nil, ViewIssueRes{}, errors.New("iid is required")
		}

		issue, _, err := c.gl.Issues.GetIssue(pid, args.IID, gl.WithContext(ctx))
		if err != nil {
			return nil, ViewIssueRes{}, errors.Wrapf(err, "get issue %s#%d", pid, args.IID)
		}

		detail := IssueDetail{
			IssueSummary: issueSummary(issue),
			Confidential: issue.Confidential,
			CreatedAt:    issue.CreatedAt,
			ClosedAt:     issue.ClosedAt,
		}
		detail.Description, detail.DescriptionTruncated = truncate(issue.Description, descriptionLimit)

		if args.WithNotes {
			notes, _, err := c.gl.Notes.ListIssueNotes(pid, args.IID, &gl.ListIssueNotesOptions{
				ListOptions: gl.ListOptions{PerPage: maxPerPage},
				Sort:        new("asc"),
			}, gl.WithContext(ctx))
			if err != nil {
				return nil, ViewIssueRes{}, errors.Wrapf(err, "list notes of issue %s#%d", pid, args.IID)
			}
			for _, n := range notes {
				detail.Notes = append(detail.Notes, noteSummary(n))
			}
		}

		return nil, ViewIssueRes{Issue: detail}, nil
	}
}

type CreateIssueArgs struct {
	Project      string   `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	Title        string   `json:"title" jsonschema:"issue title"`
	Description  string   `json:"description,omitempty" jsonschema:"issue body, in GitLab-flavored Markdown"`
	Labels       []string `json:"labels,omitempty" jsonschema:"label names to apply"`
	Assignees    []string `json:"assignees,omitempty" jsonschema:"usernames to assign"`
	Milestone    string   `json:"milestone,omitempty" jsonschema:"milestone title to attach"`
	Confidential bool     `json:"confidential,omitempty" jsonschema:"make the issue visible only to project members"`
}

type IssueRes struct {
	Issue IssueSummary `json:"issue"`
}

func createIssueHandler(c *Client) mcp.ToolHandlerFor[CreateIssueArgs, IssueRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args CreateIssueArgs) (*mcp.CallToolResult, IssueRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, IssueRes{}, err
		}
		if args.Title == "" {
			return nil, IssueRes{}, errors.New("title is required")
		}

		opt := &gl.CreateIssueOptions{Title: new(args.Title)}
		if args.Description != "" {
			opt.Description = new(args.Description)
		}
		if len(args.Labels) > 0 {
			opt.Labels = new(gl.LabelOptions(args.Labels))
		}
		if args.Confidential {
			opt.Confidential = new(true)
		}
		if len(args.Assignees) > 0 {
			ids, err := c.userIDs(ctx, args.Assignees)
			if err != nil {
				return nil, IssueRes{}, err
			}
			opt.AssigneeIDs = new(ids)
		}
		if args.Milestone != "" {
			id, err := c.milestoneID(ctx, pid, args.Milestone)
			if err != nil {
				return nil, IssueRes{}, err
			}
			opt.MilestoneID = new(id)
		}

		issue, _, err := c.gl.Issues.CreateIssue(pid, opt, gl.WithContext(ctx))
		if err != nil {
			return nil, IssueRes{}, errors.Wrapf(err, "create issue in %s", pid)
		}
		return nil, IssueRes{Issue: issueSummary(issue)}, nil
	}
}

type UpdateIssueArgs struct {
	Project     string `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	IID         int64  `json:"iid" jsonschema:"project-scoped issue number, the one shown as #123"`
	Title       string `json:"title,omitempty" jsonschema:"replace the title"`
	Description string `json:"description,omitempty" jsonschema:"replace the body; this overwrites, it does not append"`
	// Labels replaces the set, while AddLabels and RemoveLabels edit it. The
	// distinction matters: an agent that means to add one label and sends
	// Labels drops every other label on the issue.
	Labels       []string `json:"labels,omitempty" jsonschema:"replace all labels with this set"`
	AddLabels    []string `json:"add_labels,omitempty" jsonschema:"add these labels, leaving existing ones"`
	RemoveLabels []string `json:"remove_labels,omitempty" jsonschema:"remove these labels, leaving the rest"`
	Assignees    []string `json:"assignees,omitempty" jsonschema:"replace assignees with these usernames; an empty list is ignored, use unassign"`
	Unassign     bool     `json:"unassign,omitempty" jsonschema:"remove all assignees"`
	Milestone    string   `json:"milestone,omitempty" jsonschema:"milestone title to attach"`
	State        string   `json:"state,omitempty" jsonschema:"close or reopen"`
}

func updateIssueHandler(c *Client) mcp.ToolHandlerFor[UpdateIssueArgs, IssueRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args UpdateIssueArgs) (*mcp.CallToolResult, IssueRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, IssueRes{}, err
		}
		if args.IID <= 0 {
			return nil, IssueRes{}, errors.New("iid is required")
		}

		opt := &gl.UpdateIssueOptions{}
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
		switch {
		case args.Unassign:
			opt.AssigneeIDs = new([]int64{})
			empty = false
		case len(args.Assignees) > 0:
			ids, err := c.userIDs(ctx, args.Assignees)
			if err != nil {
				return nil, IssueRes{}, err
			}
			opt.AssigneeIDs = new(ids)
			empty = false
		}
		if args.Milestone != "" {
			id, err := c.milestoneID(ctx, pid, args.Milestone)
			if err != nil {
				return nil, IssueRes{}, err
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
				return nil, IssueRes{}, errors.Errorf("state must be close or reopen, got %q", args.State)
			}
		}
		if empty {
			return nil, IssueRes{}, errors.New("no changes requested: set at least one of title, description, labels, add_labels, remove_labels, assignees, unassign, milestone, state")
		}

		issue, _, err := c.gl.Issues.UpdateIssue(pid, args.IID, opt, gl.WithContext(ctx))
		if err != nil {
			return nil, IssueRes{}, errors.Wrapf(err, "update issue %s#%d", pid, args.IID)
		}
		return nil, IssueRes{Issue: issueSummary(issue)}, nil
	}
}

type CreateIssueNoteArgs struct {
	Project string `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	IID     int64  `json:"iid" jsonschema:"project-scoped issue number, the one shown as #123"`
	Body    string `json:"body" jsonschema:"comment text, in GitLab-flavored Markdown"`
}

type NoteRes struct {
	Note NoteSummary `json:"note"`
}

func createIssueNoteHandler(c *Client) mcp.ToolHandlerFor[CreateIssueNoteArgs, NoteRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args CreateIssueNoteArgs) (*mcp.CallToolResult, NoteRes, error) {
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

		note, _, err := c.gl.Notes.CreateIssueNote(pid, args.IID, &gl.CreateIssueNoteOptions{
			Body: new(args.Body),
		}, gl.WithContext(ctx))
		if err != nil {
			return nil, NoteRes{}, errors.Wrapf(err, "comment on issue %s#%d", pid, args.IID)
		}
		return nil, NoteRes{Note: noteSummary(note)}, nil
	}
}

func registerIssueTools(s *mcp.Server, c *Client) {
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "issue_list",
		Description: "Lists issues of a GitLab project, filtered by state, labels, assignee, author, milestone or free-text search.",
		Flags:       mcputil.ReadOnly,
	}, listIssuesHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "issue_view",
		Description: "Reads one GitLab issue by its project-scoped number, optionally with its comment thread.",
		Flags:       mcputil.ReadOnly,
	}, viewIssueHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "issue_create",
		Description: "Creates a GitLab issue.",
	}, createIssueHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "issue_update",
		Description: "Updates a GitLab issue: title, description, labels, assignees, milestone, or open/closed state.",
	}, updateIssueHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "issue_note",
		Description: "Posts a comment on a GitLab issue.",
	}, createIssueNoteHandler(c))
}
