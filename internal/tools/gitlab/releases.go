package gitlab

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path"
	"time"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	gl "gitlab.com/gitlab-org/api/client-go/v2"

	"github.com/go-faster/gooners/blob"
	"github.com/go-faster/gooners/internal/tools/mcputil"
)

// maxAssetSize bounds both directions of an asset transfer. Release assets are
// binaries; without a cap, one call could exhaust memory or fill the disk.
const maxAssetSize = 100 << 20 // 100 MiB

// ReleaseSummary is the compact list view of a release.
type ReleaseSummary struct {
	TagName     string       `json:"tag_name"`
	Name        string       `json:"name,omitempty"`
	Author      *UserSummary `json:"author,omitempty"`
	CreatedAt   *time.Time   `json:"created_at,omitempty"`
	ReleasedAt  *time.Time   `json:"released_at,omitempty"`
	Upcoming    bool         `json:"upcoming_release,omitempty" jsonschema:"released_at is in the future, so the release is not yet published"`
	CommitSHA   string       `json:"commit_sha,omitempty"`
	AssetsCount int64        `json:"assets_count"`
}

// ReleaseDetail adds the notes and asset list.
type ReleaseDetail struct {
	ReleaseSummary
	Description          string             `json:"description,omitempty" jsonschema:"the release notes"`
	DescriptionTruncated bool               `json:"description_truncated,omitempty"`
	Milestones           []string           `json:"milestones,omitempty"`
	Assets               []ReleaseAsset     `json:"assets,omitempty"`
	Sources              []ReleaseAssetLink `json:"sources,omitempty" jsonschema:"auto-generated source archives"`
}

// ReleaseAsset is one attached link or file.
type ReleaseAsset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
	// DirectAssetURL is the permalink form; release_asset_download prefers it.
	DirectAssetURL string `json:"direct_asset_url,omitempty"`
	// External marks an asset hosted outside this GitLab instance. Downloading
	// one is refused: the HTTP client's allowlist is the instance itself.
	External bool   `json:"external,omitempty"`
	LinkType string `json:"link_type,omitempty" jsonschema:"other, runbook, image, or package"`
}

// ReleaseAssetLink is a source archive entry.
type ReleaseAssetLink struct {
	Format string `json:"format"`
	URL    string `json:"url"`
}

func releaseSummary(r *gl.Release) ReleaseSummary {
	return ReleaseSummary{
		TagName:     r.TagName,
		Name:        r.Name,
		Author:      userSummary(&r.Author),
		CreatedAt:   r.CreatedAt,
		ReleasedAt:  r.ReleasedAt,
		Upcoming:    r.UpcomingRelease,
		CommitSHA:   r.Commit.ID,
		AssetsCount: r.Assets.Count,
	}
}

type ListReleasesArgs struct {
	ListArgs
	Project string `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
}

type ListReleasesRes struct {
	Releases []ReleaseSummary `json:"releases"`
}

func listReleasesHandler(c *Client) mcp.ToolHandlerFor[ListReleasesArgs, ListReleasesRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ListReleasesArgs) (*mcp.CallToolResult, ListReleasesRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, ListReleasesRes{}, err
		}

		releases, _, err := c.gl.Releases.ListReleases(pid, &gl.ListReleasesOptions{
			ListOptions: args.ListArgs.listOptions(),
		}, gl.WithContext(ctx))
		if err != nil {
			return nil, ListReleasesRes{}, errors.Wrapf(err, "list releases of %s", pid)
		}

		res := ListReleasesRes{Releases: make([]ReleaseSummary, 0, len(releases))}
		for _, r := range releases {
			res.Releases = append(res.Releases, releaseSummary(r))
		}
		return nil, res, nil
	}
}

type ViewReleaseArgs struct {
	Project string `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	TagName string `json:"tag_name,omitempty" jsonschema:"tag of the release; empty means the latest release"`
}

type ViewReleaseRes struct {
	Release ReleaseDetail `json:"release"`
}

func viewReleaseHandler(c *Client) mcp.ToolHandlerFor[ViewReleaseArgs, ViewReleaseRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ViewReleaseArgs) (*mcp.CallToolResult, ViewReleaseRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, ViewReleaseRes{}, err
		}

		var release *gl.Release
		if args.TagName == "" {
			release, _, err = c.gl.Releases.GetLatestRelease(pid, gl.WithContext(ctx))
			if err != nil {
				return nil, ViewReleaseRes{}, errors.Wrapf(err, "get latest release of %s", pid)
			}
		} else {
			release, _, err = c.gl.Releases.GetRelease(pid, args.TagName, gl.WithContext(ctx))
			if err != nil {
				return nil, ViewReleaseRes{}, errors.Wrapf(err, "get release %s of %s", args.TagName, pid)
			}
		}

		detail := ReleaseDetail{ReleaseSummary: releaseSummary(release)}
		detail.Description, detail.DescriptionTruncated = truncate(release.Description, descriptionLimit)
		for _, m := range release.Milestones {
			if m != nil {
				detail.Milestones = append(detail.Milestones, m.Title)
			}
		}
		for _, l := range release.Assets.Links {
			if l == nil {
				continue
			}
			detail.Assets = append(detail.Assets, ReleaseAsset{
				ID:             l.ID,
				Name:           l.Name,
				URL:            l.URL,
				DirectAssetURL: l.DirectAssetURL,
				External:       l.External,
				LinkType:       string(l.LinkType),
			})
		}
		for _, s := range release.Assets.Sources {
			detail.Sources = append(detail.Sources, ReleaseAssetLink{Format: s.Format, URL: s.URL})
		}

		return nil, ViewReleaseRes{Release: detail}, nil
	}
}

type CreateReleaseArgs struct {
	Project     string   `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	TagName     string   `json:"tag_name" jsonschema:"tag to release; it is created from ref when it does not exist"`
	Ref         string   `json:"ref,omitempty" jsonschema:"commit SHA, branch or tag to create tag_name from; required only when the tag does not exist yet"`
	Name        string   `json:"name,omitempty" jsonschema:"release title; defaults to the tag name"`
	Description string   `json:"description,omitempty" jsonschema:"release notes, in GitLab-flavored Markdown"`
	Milestones  []string `json:"milestones,omitempty" jsonschema:"milestone titles to associate"`
	ReleasedAt  string   `json:"released_at,omitempty" jsonschema:"RFC 3339 timestamp; a future value creates an upcoming release"`
}

type ReleaseRes struct {
	Release ReleaseSummary `json:"release"`
}

func createReleaseHandler(c *Client) mcp.ToolHandlerFor[CreateReleaseArgs, ReleaseRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args CreateReleaseArgs) (*mcp.CallToolResult, ReleaseRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, ReleaseRes{}, err
		}
		if args.TagName == "" {
			return nil, ReleaseRes{}, errors.New("tag_name is required")
		}

		opt := &gl.CreateReleaseOptions{TagName: new(args.TagName)}
		if args.Ref != "" {
			opt.Ref = new(args.Ref)
		}
		if args.Name != "" {
			opt.Name = new(args.Name)
		}
		if args.Description != "" {
			opt.Description = new(args.Description)
		}
		if len(args.Milestones) > 0 {
			opt.Milestones = new(args.Milestones)
		}
		if args.ReleasedAt != "" {
			t, err := time.Parse(time.RFC3339, args.ReleasedAt)
			if err != nil {
				return nil, ReleaseRes{}, errors.Wrapf(err, "parse released_at %q", args.ReleasedAt)
			}
			opt.ReleasedAt = new(t)
		}

		release, _, err := c.gl.Releases.CreateRelease(pid, opt, gl.WithContext(ctx))
		if err != nil {
			return nil, ReleaseRes{}, errors.Wrapf(err, "create release %s in %s", args.TagName, pid)
		}
		return nil, ReleaseRes{Release: releaseSummary(release)}, nil
	}
}

type UploadReleaseAssetArgs struct {
	Project string `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	TagName string `json:"tag_name" jsonschema:"tag of an existing release to attach the asset to"`
	// Path is resolved by the server's filesystem provider, not by this
	// handler: what the agent may read is decided by how the binary was
	// started, not by the argument.
	Path     string `json:"path" jsonschema:"local file to upload, relative to the server's assets directory"`
	Name     string `json:"name,omitempty" jsonschema:"asset name shown on the release page; defaults to the file's base name"`
	LinkType string `json:"link_type,omitempty" jsonschema:"other, runbook, image, or package; defaults to other"`
}

type UploadReleaseAssetRes struct {
	Asset ReleaseAsset `json:"asset"`
	Size  int64        `json:"size" jsonschema:"bytes uploaded"`
}

func uploadReleaseAssetHandler(c *Client) mcp.ToolHandlerFor[UploadReleaseAssetArgs, UploadReleaseAssetRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args UploadReleaseAssetArgs) (*mcp.CallToolResult, UploadReleaseAssetRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, UploadReleaseAssetRes{}, err
		}
		if args.TagName == "" {
			return nil, UploadReleaseAssetRes{}, errors.New("tag_name is required")
		}
		if args.Path == "" {
			return nil, UploadReleaseAssetRes{}, errors.New("path is required")
		}

		info, err := c.cfg.FS.Stat(args.Path)
		if err != nil {
			return nil, UploadReleaseAssetRes{}, errors.Wrapf(err, "stat %s", args.Path)
		}
		if info.Size() > maxAssetSize {
			return nil, UploadReleaseAssetRes{}, errors.Errorf("%s is %d bytes, over the %d byte asset limit", args.Path, info.Size(), maxAssetSize)
		}

		data, err := c.cfg.FS.ReadFile(args.Path)
		if err != nil {
			return nil, UploadReleaseAssetRes{}, errors.Wrapf(err, "read %s", args.Path)
		}

		name := args.Name
		if name == "" {
			name = path.Base(args.Path)
		}

		// An asset is a project upload first; the release link then points at
		// it. There is no single endpoint that does both.
		uploaded, _, err := c.gl.ProjectMarkdownUploads.UploadProjectMarkdown(pid, bytes.NewReader(data), name, gl.WithContext(ctx))
		if err != nil {
			return nil, UploadReleaseAssetRes{}, errors.Wrapf(err, "upload %s to %s", name, pid)
		}

		opt := &gl.CreateReleaseLinkOptions{
			Name: new(name),
			URL:  new(c.webURL(uploaded.FullPath)),
		}
		if args.LinkType != "" {
			opt.LinkType = new(gl.LinkTypeValue(args.LinkType))
		}

		link, _, err := c.gl.ReleaseLinks.CreateReleaseLink(pid, args.TagName, opt, gl.WithContext(ctx))
		if err != nil {
			return nil, UploadReleaseAssetRes{}, errors.Wrapf(err, "attach %s to release %s", name, args.TagName)
		}

		return nil, UploadReleaseAssetRes{
			Asset: ReleaseAsset{
				ID:             link.ID,
				Name:           link.Name,
				URL:            link.URL,
				DirectAssetURL: link.DirectAssetURL,
				External:       link.External,
				LinkType:       string(link.LinkType),
			},
			Size: info.Size(),
		}, nil
	}
}

type DownloadReleaseAssetArgs struct {
	Project string `json:"project,omitempty" jsonschema:"project path (group/project) or numeric ID; defaults to the server's configured project"`
	TagName string `json:"tag_name" jsonschema:"tag of the release holding the asset"`
	Name    string `json:"name" jsonschema:"asset name as listed by release_view"`
	Path    string `json:"path,omitempty" jsonschema:"local destination, relative to the server's assets directory; omit to get a temporary URL instead, which is the only useful answer when you do not share this server's filesystem"`
}

type DownloadReleaseAssetRes struct {
	Path      string     `json:"path,omitempty" jsonschema:"host path the asset was written to"`
	URL       string     `json:"url,omitempty" jsonschema:"temporary URL to fetch the asset from, e.g. with curl"`
	ExpiresAt *time.Time `json:"expires_at,omitempty" jsonschema:"when url stops working"`
	Size      int64      `json:"size" jsonschema:"bytes transferred"`
}

func downloadReleaseAssetHandler(c *Client) mcp.ToolHandlerFor[DownloadReleaseAssetArgs, DownloadReleaseAssetRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args DownloadReleaseAssetArgs) (*mcp.CallToolResult, DownloadReleaseAssetRes, error) {
		pid, err := c.project(args.Project)
		if err != nil {
			return nil, DownloadReleaseAssetRes{}, err
		}
		if args.TagName == "" {
			return nil, DownloadReleaseAssetRes{}, errors.New("tag_name is required")
		}
		if args.Name == "" {
			return nil, DownloadReleaseAssetRes{}, errors.New("name is required")
		}
		// Fail before the transfer rather than after it, while the error can
		// still name the path.
		if args.Path != "" {
			if _, err := c.cfg.FS.Resolve(args.Path); err != nil {
				return nil, DownloadReleaseAssetRes{}, errors.Wrapf(err, "destination %s", args.Path)
			}
		}

		release, _, err := c.gl.Releases.GetRelease(pid, args.TagName, gl.WithContext(ctx))
		if err != nil {
			return nil, DownloadReleaseAssetRes{}, errors.Wrapf(err, "get release %s of %s", args.TagName, pid)
		}

		var link *gl.ReleaseLink
		for _, l := range release.Assets.Links {
			if l != nil && l.Name == args.Name {
				link = l
				break
			}
		}
		if link == nil {
			return nil, DownloadReleaseAssetRes{}, errors.Errorf("release %s of %s has no asset named %q", args.TagName, pid, args.Name)
		}

		url := link.DirectAssetURL
		if url == "" {
			url = link.URL
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
		if err != nil {
			return nil, DownloadReleaseAssetRes{}, errors.Wrapf(err, "build request for %s", url)
		}
		if c.cfg.Token != "" {
			req.Header.Set("PRIVATE-TOKEN", c.cfg.Token)
		}

		// A release link can point at any host, so this GET is the one place a
		// tool follows a URL the project's contents chose. The client's
		// allowlist is the configured instance, which is what stops an asset
		// link from turning this server into a fetch-anything proxy.
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, DownloadReleaseAssetRes{}, errors.Wrapf(err, "download %s", url)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			return nil, DownloadReleaseAssetRes{}, errors.Errorf("download %s: unexpected status %s", url, resp.Status)
		}
		// The cap applies however the asset leaves here, and it holds even when
		// Content-Length lies or is absent.
		body := newCappedReader(resp.Body, args.Name)

		if args.Path == "" {
			return downloadToBlob(ctx, c, args, body, resp.ContentLength)
		}

		data, err := io.ReadAll(body)
		if err != nil {
			return nil, DownloadReleaseAssetRes{}, errors.Wrapf(err, "read %s", url)
		}
		if err := c.cfg.FS.WriteFile(args.Path, data, 0o600); err != nil {
			return nil, DownloadReleaseAssetRes{}, errors.Wrapf(err, "write %s", args.Path)
		}

		resolved, err := c.cfg.FS.Resolve(args.Path)
		if err != nil {
			return nil, DownloadReleaseAssetRes{}, errors.Wrapf(err, "resolve %s", args.Path)
		}
		return nil, DownloadReleaseAssetRes{Path: resolved, Size: int64(len(data))}, nil
	}
}

// downloadToBlob stores the asset and answers with a link to it, for the agent
// that cannot read this server's filesystem.
func downloadToBlob(ctx context.Context, c *Client, args DownloadReleaseAssetArgs, body io.Reader, size int64) (*mcp.CallToolResult, DownloadReleaseAssetRes, error) {
	if size <= 0 {
		size = blob.SizeUnknown
	}
	// The type is guessed from the asset name rather than taken from the
	// response: the name is what the agent asked for, and a type chosen by
	// project content decides how a client renders these bytes.
	content, b, err := blob.Content(ctx, c.cfg.Blob, body, blob.ContentOptions{
		PutOptions: blob.PutOptions{Name: args.Name, Size: size},
	})
	if err != nil {
		return nil, DownloadReleaseAssetRes{}, errors.Wrapf(err, "store asset %q", args.Name)
	}

	res := DownloadReleaseAssetRes{URL: b.URL, Size: b.Size}
	if !b.ExpiresAt.IsZero() {
		res.ExpiresAt = new(b.ExpiresAt)
	}
	return &mcp.CallToolResult{Content: content}, res, nil
}

// cappedReader fails at the limit instead of truncating there, so an asset
// over the cap is an error rather than a silently short file.
type cappedReader struct {
	r    io.Reader
	n    int64 // bytes still allowed, one more than the limit
	name string
}

// newCappedReader allows one byte past [maxAssetSize], which is what
// distinguishes an asset exactly at the limit from one over it.
func newCappedReader(r io.Reader, name string) *cappedReader {
	return &cappedReader{r: r, n: maxAssetSize + 1, name: name}
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, errors.Errorf("asset %q is over the %d byte limit", c.name, maxAssetSize)
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	return n, err
}

func registerReleaseTools(s *mcp.Server, c *Client) {
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "release_list",
		Description: "Lists releases of a GitLab project, newest first.",
		Flags:       mcputil.ReadOnly,
	}, listReleasesHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "release_view",
		Description: "Reads one GitLab release by tag, with its notes, milestones and assets. Omit tag_name for the latest release.",
		Flags:       mcputil.ReadOnly,
	}, viewReleaseHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "release_create",
		Description: "Creates a GitLab release from a tag, creating the tag from ref when it does not exist yet.",
	}, createReleaseHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "release_asset_upload",
		Description: "Uploads a local file and attaches it as an asset of an existing GitLab release. Only files under the server's assets directory are readable.",
	}, uploadReleaseAssetHandler(c))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "release_asset_download",
		Description: "Downloads a GitLab release asset, either to a local file when path is given, or to a temporary URL you can fetch with curl when it is omitted. Only paths under the server's assets directory are writable, and only assets hosted on the configured instance can be fetched.",
	}, downloadReleaseAssetHandler(c))
}
