package gitlab

import (
	"context"
	"encoding/json"
	"strconv"
	"unicode/utf8"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yosida95/uritemplate/v3"
	gl "gitlab.com/gitlab-org/api/client-go/v2"
)

// Resource URI templates. They mirror GitLab's own web URLs, whose /-/ segment
// exists for the same reason it is needed here: a project path has an unbounded
// number of group segments, so something must mark where it ends.
const (
	issueURITemplate   = "gitlab:///{+project}/-/issues/{iid}"
	mrURITemplate      = "gitlab:///{+project}/-/merge_requests/{iid}"
	releaseURITemplate = "gitlab:///{+project}/-/releases/{tag}"
	// blobURITemplate takes a ref that cannot itself contain a slash, so
	// branches like feat/x must be addressed by SHA. The tools have no such
	// limit; this is a property of the URI, not of the API.
	blobURITemplate = "gitlab:///{+project}/-/blob/{ref}/{+path}"
)

// resourceVars matches uri against tmpl and returns its variables. The server
// only dispatches a URI to the handler whose template matched, so a failure
// here means the URI is addressed to us but malformed.
func resourceVars(tmpl, uri string) (uritemplate.Values, error) {
	t, err := uritemplate.New(tmpl)
	if err != nil {
		return nil, errors.Wrapf(err, "parse template %s", tmpl)
	}
	vals := t.Match(uri)
	if vals == nil {
		return nil, mcp.ResourceNotFoundError(uri)
	}
	return vals, nil
}

// jsonResource renders v as the single content of a read result. Resources
// carry the same shapes the tools return, so a client can subscribe to an issue
// and get what issue_view would have said.
func jsonResource(uri string, v any) (*mcp.ReadResourceResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, errors.Wrap(err, "marshal resource")
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      uri,
			MIMEType: "application/json",
			Text:     string(data),
		}},
	}, nil
}

func issueResourceHandler(c *Client) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		vals, err := resourceVars(issueURITemplate, uri)
		if err != nil {
			return nil, err
		}
		iid, err := strconv.ParseInt(vals.Get("iid").String(), 10, 64)
		if err != nil {
			return nil, errors.Wrapf(err, "parse issue number of %s", uri)
		}
		pid := vals.Get("project").String()

		issue, _, err := c.gl.Issues.GetIssue(pid, iid, gl.WithContext(ctx))
		if err != nil {
			return nil, errors.Wrapf(err, "get issue %s#%d", pid, iid)
		}
		detail := IssueDetail{
			IssueSummary: issueSummary(issue),
			Confidential: issue.Confidential,
			CreatedAt:    issue.CreatedAt,
			ClosedAt:     issue.ClosedAt,
		}
		detail.Description, detail.DescriptionTruncated = truncate(issue.Description, descriptionLimit)
		return jsonResource(uri, detail)
	}
}

func mergeRequestResourceHandler(c *Client) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		vals, err := resourceVars(mrURITemplate, uri)
		if err != nil {
			return nil, err
		}
		iid, err := strconv.ParseInt(vals.Get("iid").String(), 10, 64)
		if err != nil {
			return nil, errors.Wrapf(err, "parse merge request number of %s", uri)
		}
		pid := vals.Get("project").String()

		mr, _, err := c.gl.MergeRequests.GetMergeRequest(pid, iid, nil, gl.WithContext(ctx))
		if err != nil {
			return nil, errors.Wrapf(err, "get merge request %s!%d", pid, iid)
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
		return jsonResource(uri, detail)
	}
}

func releaseResourceHandler(c *Client) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		vals, err := resourceVars(releaseURITemplate, uri)
		if err != nil {
			return nil, err
		}
		pid := vals.Get("project").String()
		tag := vals.Get("tag").String()

		release, _, err := c.gl.Releases.GetRelease(pid, tag, gl.WithContext(ctx))
		if err != nil {
			return nil, errors.Wrapf(err, "get release %s of %s", tag, pid)
		}
		detail := ReleaseDetail{ReleaseSummary: releaseSummary(release)}
		detail.Description, detail.DescriptionTruncated = truncate(release.Description, descriptionLimit)
		return jsonResource(uri, detail)
	}
}

// blobResourceHandler serves file contents as themselves rather than wrapped in
// JSON: a client embedding a source file wants the source, not an object with
// the source inside it.
func blobResourceHandler(c *Client) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		vals, err := resourceVars(blobURITemplate, uri)
		if err != nil {
			return nil, err
		}
		pid := vals.Get("project").String()
		ref := vals.Get("ref").String()
		path := vals.Get("path").String()

		raw, _, err := c.gl.RepositoryFiles.GetRawFile(pid, path, &gl.GetRawFileOptions{
			Ref: new(ref),
		}, gl.WithContext(ctx))
		if err != nil {
			return nil, errors.Wrapf(err, "read %s at %s of %s", path, ref, pid)
		}

		contents := &mcp.ResourceContents{URI: uri}
		if utf8.Valid(raw) {
			contents.MIMEType = "text/plain"
			contents.Text, _ = truncate(string(raw), fileLimit)
		} else {
			contents.MIMEType = "application/octet-stream"
			contents.Blob = raw
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{contents}}, nil
	}
}

// projectResourceHandler serves the configured default project's metadata. It
// is the one concrete resource this server can list without being told a
// project first, so it doubles as the entry point a client follows.
func projectResourceHandler(c *Client) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		p, _, err := c.gl.Projects.GetProject(c.cfg.DefaultProject, nil, gl.WithContext(ctx))
		if err != nil {
			return nil, errors.Wrapf(err, "get project %s", c.cfg.DefaultProject)
		}
		return jsonResource(req.Params.URI, projectSummary(p))
	}
}

func registerResources(s *mcp.Server, c *Client) {
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		Name:        "issue",
		Title:       "GitLab issue",
		Description: "One issue, addressed the way its web URL is: gitlab:///group/project/-/issues/123.",
		URITemplate: issueURITemplate,
		MIMEType:    "application/json",
	}, issueResourceHandler(c))

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		Name:        "merge_request",
		Title:       "GitLab merge request",
		Description: "One merge request, addressed the way its web URL is: gitlab:///group/project/-/merge_requests/123.",
		URITemplate: mrURITemplate,
		MIMEType:    "application/json",
	}, mergeRequestResourceHandler(c))

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		Name:        "release",
		Title:       "GitLab release",
		Description: "One release by tag: gitlab:///group/project/-/releases/v1.2.3.",
		URITemplate: releaseURITemplate,
		MIMEType:    "application/json",
	}, releaseResourceHandler(c))

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		Name:        "blob",
		Title:       "GitLab repository file",
		Description: "A file at a ref: gitlab:///group/project/-/blob/main/cmd/main.go. The ref cannot contain a slash, so use a commit SHA for branches like feat/x.",
		URITemplate: blobURITemplate,
	}, blobResourceHandler(c))

	if c.cfg.DefaultProject != "" {
		s.AddResource(&mcp.Resource{
			Name:        "project",
			Title:       "Configured GitLab project",
			Description: "Metadata of the project this server defaults to: " + c.cfg.DefaultProject + ".",
			URI:         "gitlab:///" + c.cfg.DefaultProject,
			MIMEType:    "application/json",
		}, projectResourceHandler(c))
	}
}
