package gitlab

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	gl "gitlab.com/gitlab-org/api/client-go/v2"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

// fileLimit caps repo_file. A vendored bundle or a lockfile is easily larger
// than anything an agent can use in one read.
const fileLimit = 200000

// ProjectSummary is the compact view of a project.
type ProjectSummary struct {
	ID            int64      `json:"id"`
	PathWithNS    string     `json:"path_with_namespace" jsonschema:"the group/project path other tools take as their project argument"`
	Name          string     `json:"name"`
	Description   string     `json:"description,omitempty"`
	DefaultBranch string     `json:"default_branch,omitempty"`
	Visibility    string     `json:"visibility,omitempty" jsonschema:"private, internal, or public"`
	Archived      bool       `json:"archived,omitempty"`
	Topics        []string   `json:"topics,omitempty"`
	StarCount     int64      `json:"star_count,omitempty"`
	ForksCount    int64      `json:"forks_count,omitempty"`
	LastActivity  *time.Time `json:"last_activity_at,omitempty"`
	WebURL        string     `json:"web_url,omitempty"`
}

// ProjectDetail adds the counts and README that are only worth a call when
// looking at one project.
type ProjectDetail struct {
	ProjectSummary
	OpenIssuesCount int64  `json:"open_issues_count,omitempty"`
	Readme          string `json:"readme,omitempty"`
	ReadmeTruncated bool   `json:"readme_truncated,omitempty"`
}

func projectSummary(p *gl.Project) ProjectSummary {
	return ProjectSummary{
		ID:            p.ID,
		PathWithNS:    p.PathWithNamespace,
		Name:          p.Name,
		Description:   p.Description,
		DefaultBranch: p.DefaultBranch,
		Visibility:    string(p.Visibility),
		Archived:      p.Archived,
		Topics:        p.Topics,
		StarCount:     p.StarCount,
		ForksCount:    p.ForksCount,
		LastActivity:  p.LastActivityAt,
		WebURL:        p.WebURL,
	}
}

type ViewProjectArgs struct {
	Project    string `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	WithReadme bool   `json:"with_readme,omitempty" jsonschema:"include the README's contents; costs one extra API call"`
}

type ViewProjectRes struct {
	Project ProjectDetail `json:"project"`
}

func viewProjectHandler(c *Client) mcp.ToolHandlerFor[ViewProjectArgs, ViewProjectRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ViewProjectArgs) (*mcp.CallToolResult, ViewProjectRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, ViewProjectRes{}, err
		}

		p, _, err := c.gl.Projects.GetProject(pid, nil, gl.WithContext(ctx))
		if err != nil {
			return nil, ViewProjectRes{}, errors.Wrapf(err, "get project %s", pid)
		}

		detail := ProjectDetail{
			ProjectSummary:  projectSummary(p),
			OpenIssuesCount: p.OpenIssuesCount,
		}

		if args.WithReadme && p.ReadmeURL != "" {
			// ReadmeURL is a web URL; its last path segment is the file name in
			// the default branch, which is what the raw-file endpoint wants.
			name := readmeName(p.ReadmeURL)
			raw, _, err := c.gl.RepositoryFiles.GetRawFile(pid, name, &gl.GetRawFileOptions{
				Ref: new(p.DefaultBranch),
			}, gl.WithContext(ctx))
			if err != nil {
				return nil, ViewProjectRes{}, errors.Wrapf(err, "read %s of %s", name, pid)
			}
			detail.Readme, detail.ReadmeTruncated = truncate(string(raw), descriptionLimit)
		}

		return nil, ViewProjectRes{Project: detail}, nil
	}
}

// readmeName extracts the file name from a project's readme_url, which looks
// like https://host/group/project/-/blob/main/README.md.
func readmeName(readmeURL string) string {
	for i := len(readmeURL) - 1; i >= 0; i-- {
		if readmeURL[i] == '/' {
			return readmeURL[i+1:]
		}
	}
	return readmeURL
}

type SearchProjectsArgs struct {
	ListArgs
	Search     string `json:"search" jsonschema:"text to match against project names and paths"`
	Membership bool   `json:"membership,omitempty" jsonschema:"only projects the authenticated user is a member of"`
	OrderBy    string `json:"order_by,omitempty" jsonschema:"id, name, path, created_at, updated_at, or last_activity_at; defaults to created_at"`
	Sort       string `json:"sort,omitempty" jsonschema:"asc or desc; defaults to desc"`
}

type SearchProjectsRes struct {
	Projects []ProjectSummary `json:"projects"`
}

func searchProjectsHandler(c *Client) mcp.ToolHandlerFor[SearchProjectsArgs, SearchProjectsRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args SearchProjectsArgs) (*mcp.CallToolResult, SearchProjectsRes, error) {
		if args.Search == "" {
			return nil, SearchProjectsRes{}, errors.New("search is required")
		}

		opt := &gl.ListProjectsOptions{
			ListOptions: args.ListArgs.listOptions(),
			Search:      new(args.Search),
		}
		if args.Membership {
			opt.Membership = new(true)
		}
		if args.OrderBy != "" {
			opt.OrderBy = new(args.OrderBy)
		}
		if args.Sort != "" {
			opt.Sort = new(args.Sort)
		}

		projects, _, err := c.gl.Projects.ListProjects(opt, gl.WithContext(ctx))
		if err != nil {
			return nil, SearchProjectsRes{}, errors.Wrapf(err, "search projects for %q", args.Search)
		}

		res := SearchProjectsRes{Projects: make([]ProjectSummary, 0, len(projects))}
		for _, p := range projects {
			res.Projects = append(res.Projects, projectSummary(p))
		}
		return nil, res, nil
	}
}

// TreeEntry is one node of a repository tree.
type TreeEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type" jsonschema:"tree for a directory, blob for a file"`
}

type TreeArgs struct {
	ListArgs
	Project   string `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	Path      string `json:"path,omitempty" jsonschema:"directory to list; empty means the repository root"`
	Ref       string `json:"ref,omitempty" jsonschema:"branch, tag or commit SHA; defaults to the project's default branch"`
	Recursive bool   `json:"recursive,omitempty" jsonschema:"list the whole subtree rather than one level"`
}

type TreeRes struct {
	Entries []TreeEntry `json:"entries"`
}

func treeHandler(c *Client) mcp.ToolHandlerFor[TreeArgs, TreeRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args TreeArgs) (*mcp.CallToolResult, TreeRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, TreeRes{}, err
		}

		opt := &gl.ListTreeOptions{ListOptions: args.ListArgs.listOptions()}
		if args.Path != "" {
			opt.Path = new(args.Path)
		}
		if args.Ref != "" {
			opt.Ref = new(args.Ref)
		}
		if args.Recursive {
			opt.Recursive = new(true)
		}

		nodes, _, err := c.gl.Repositories.ListTree(pid, opt, gl.WithContext(ctx))
		if err != nil {
			return nil, TreeRes{}, errors.Wrapf(err, "list tree of %s", pid)
		}

		res := TreeRes{Entries: make([]TreeEntry, 0, len(nodes))}
		for _, n := range nodes {
			res.Entries = append(res.Entries, TreeEntry{Name: n.Name, Path: n.Path, Type: n.Type})
		}
		return nil, res, nil
	}
}

type FileArgs struct {
	Project string `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	Path    string `json:"path" jsonschema:"file path within the repository, e.g. cmd/main.go"`
	Ref     string `json:"ref,omitempty" jsonschema:"branch, tag or commit SHA; defaults to the project's default branch"`
}

type FileRes struct {
	Path      string `json:"path"`
	Ref       string `json:"ref,omitempty"`
	Content   string `json:"content,omitempty"`
	Size      int64  `json:"size" jsonschema:"size of the file in bytes, before any truncation"`
	Truncated bool   `json:"truncated,omitempty"`
	// Binary reports that the file is not valid UTF-8, so Content is empty
	// rather than a wall of escaped bytes.
	Binary bool `json:"binary,omitempty"`
}

func fileHandler(c *Client) mcp.ToolHandlerFor[FileArgs, FileRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args FileArgs) (*mcp.CallToolResult, FileRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, FileRes{}, err
		}
		if args.Path == "" {
			return nil, FileRes{}, errors.New("path is required")
		}

		ref := args.Ref
		if ref == "" {
			p, _, err := c.gl.Projects.GetProject(pid, nil, gl.WithContext(ctx))
			if err != nil {
				return nil, FileRes{}, errors.Wrapf(err, "get default branch of %s", pid)
			}
			ref = p.DefaultBranch
		}

		raw, _, err := c.gl.RepositoryFiles.GetRawFile(pid, args.Path, &gl.GetRawFileOptions{
			Ref: new(ref),
		}, gl.WithContext(ctx))
		if err != nil {
			return nil, FileRes{}, errors.Wrapf(err, "read %s at %s of %s", args.Path, ref, pid)
		}

		res := FileRes{Path: args.Path, Ref: ref, Size: int64(len(raw))}
		if !utf8.Valid(raw) {
			res.Binary = true
			return nil, res, nil
		}
		res.Content, res.Truncated = truncate(string(raw), fileLimit)
		return nil, res, nil
	}
}

func registerRepoTools(s *mcp.Server, c *Client) {
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "repo_view",
		Description: "Reads a GitLab project's metadata: description, default branch, visibility, topics, and optionally its README.",
		Flags:       mcputil.ReadOnly,
	}, viewProjectHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "repo_search",
		Description: "Searches GitLab projects by name or path. Use it to find the project path the other tools take.",
		Flags:       mcputil.ReadOnly,
	}, searchProjectsHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "repo_tree",
		Description: "Lists files and directories in a GitLab repository at a given path and ref.",
		Flags:       mcputil.ReadOnly,
	}, treeHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "repo_file",
		Description: "Reads a file from a GitLab repository at a given ref.",
		Flags:       mcputil.ReadOnly,
	}, fileHandler(c))
}
