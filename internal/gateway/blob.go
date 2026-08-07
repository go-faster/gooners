package gateway

import (
	"context"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/blob"
)

// blobShareName is the gateway's own tool for turning a host path into a URL.
const blobShareName = "blob_share"

// blobMount is one configured directory the gateway may serve files from.
type blobMount struct {
	name string
	// prefix is the directory as upstreams report it, slash-separated. It is
	// what a path is matched against.
	prefix string
	// fs is the directory as the gateway sees it, confined to itself.
	fs blob.FS
}

// newBlobMounts builds the mount table from config. The confinement lives in
// the provider, so a path that escapes a mount fails in the filesystem rather
// than in a check the tool has to remember to make.
func newBlobMounts(cfgs []BlobMountConfig) []blobMount {
	mounts := make([]blobMount, 0, len(cfgs))
	for _, c := range cfgs {
		mounts = append(mounts, blobMount{
			name:   c.Name,
			prefix: filepath.ToSlash(strings.TrimSuffix(c.prefix(), "/")),
			fs:     blob.Dir(c.Dir),
		})
	}
	// Longest prefix first, so a mount nested inside another one wins.
	sort.SliceStable(mounts, func(i, j int) bool {
		return len(mounts[i].prefix) > len(mounts[j].prefix)
	})
	return mounts
}

type blobShareInput struct {
	Path string `json:"path" jsonschema:"absolute host path another tool reported, e.g. /var/lib/example-mcp/out.png; it must be inside one of the gateway's shared directories"`
	Name string `json:"name,omitempty" jsonschema:"file name to serve it as; defaults to the path's base name"`
}

type blobShareOutput struct {
	// Blob is the stored object's id, which another server configured against
	// the same store reads directly. It is what carries the file onward — to an
	// upload tool, say — without the agent fetching and re-uploading it.
	Blob string `json:"blob" jsonschema:"id of the stored file; pass it to another tool's blob argument to hand the file over without downloading it"`
	URL  string `json:"url" jsonschema:"temporary URL to fetch the file from, e.g. with curl"`
	// Mount names which shared directory answered, so an agent that guessed
	// wrong can tell why it got the file it did.
	Mount     string `json:"mount"`
	Name      string `json:"name"`
	Size      int64  `json:"size" jsonschema:"file size in bytes"`
	ExpiresAt string `json:"expires_at" jsonschema:"RFC 3339 timestamp after which url stops working; the blob id may still be usable"`
}

// blobInstructions tells the model that a host path is not a dead end. A tool
// description is only read once a client has decided to look at that tool, and
// the agent is holding the path before it has any reason to; instructions are
// in front of the model from the first turn, which is when it needs to know.
//
// It names no mount. Instructions are handed to the transport at startup while
// the mount list reloads in place, so a list embedded here would go stale; the
// tool's own refusal names the mounts that would have worked.
const blobInstructions = "When a tool returns a path on the server's filesystem, that path is not " +
	"reachable from where you run: call blob_share with it to get a temporary URL you can fetch."

// withBlobInstructions appends [blobInstructions] to the operator's own.
func withBlobInstructions(instructions string) string {
	if instructions == "" {
		return blobInstructions
	}
	return instructions + "\n\n" + blobInstructions
}

// blobShareDescription names the directories that would work, so an agent
// holding a path can tell whether the tool applies to it without spending a
// call to find out.
//
// The list lives here rather than in the gateway's instructions because
// instructions are handed to the transport once at startup while the mount
// list reloads in place: re-registering the tool refreshes the description and
// emits listChanged, so what a client reads stays true after a reload.
func blobShareDescription(mounts []blobMount) string {
	desc := "Turn a host path another tool reported into a temporary URL you can fetch with curl. " +
		"Use it when a tool writes a file and returns its path: the path is on the server's filesystem, " +
		"which you may not share, while the URL works from anywhere you can reach this gateway. "
	if len(mounts) == 0 {
		return desc + "No shared directories are configured, so no path can be served right now."
	}
	return desc + "Only paths inside these shared directories can be served: " + describeMounts(mounts) + "."
}

// registerBlobTool adds blob_share to the aggregate server, or replaces it when
// a reload changed which directories are served. Route servers expose one
// upstream directly and do not get it, matching the discovery tools.
func (g *Gateway) registerBlobTool(mounts []blobMount) {
	mcp.AddTool(g.server, &mcp.Tool{
		Name:        blobShareName,
		Description: blobShareDescription(mounts),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, g.handleBlobShare)
}

func (g *Gateway) handleBlobShare(ctx context.Context, _ *mcp.CallToolRequest, in blobShareInput) (*mcp.CallToolResult, blobShareOutput, error) {
	if in.Path == "" {
		return nil, blobShareOutput{}, errors.New("path is required")
	}
	store, mounts := g.blobState()
	if store == nil {
		return nil, blobShareOutput{}, errors.New("the gateway was started without a blob store")
	}

	mount, rel, ok := matchBlobMount(mounts, in.Path)
	if !ok {
		return nil, blobShareOutput{}, errors.Errorf("%s is not inside any shared directory (%s)", in.Path, describeMounts(mounts))
	}

	// The prefix match is textual; the mount's filesystem is what decides
	// whether the path resolves to a file inside it, symlinks included.
	b, err := store.Attach(ctx, mount.fs, rel, blob.PutOptions{Name: in.Name})
	if err != nil {
		return nil, blobShareOutput{}, errors.Wrapf(err, "share %s from the %s directory", in.Path, mount.name)
	}

	out := blobShareOutput{
		Blob:      b.ID,
		URL:       b.URL,
		Mount:     mount.name,
		Name:      b.Name,
		Size:      b.Size,
		ExpiresAt: b.ExpiresAt.Format(time.RFC3339),
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ResourceLink{
		URI:      b.URL,
		Name:     b.Name,
		MIMEType: b.MIMEType,
		Size:     &b.Size,
	}}}, out, nil
}

// matchBlobMount finds the mount holding p and returns the path relative to it.
func matchBlobMount(mounts []blobMount, p string) (blobMount, string, bool) {
	// Upstreams report paths in their own namespace, which may not be this
	// host's; matching is on the configured prefix, never on what exists here.
	p = path.Clean(filepath.ToSlash(p))
	for _, m := range mounts {
		if p == m.prefix {
			continue // the directory itself, not a file in it
		}
		if rel, ok := strings.CutPrefix(p, m.prefix+"/"); ok {
			return m, rel, true
		}
	}
	return blobMount{}, "", false
}

// describeMounts names the configured prefixes, so a refusal says what would
// have worked.
func describeMounts(mounts []blobMount) string {
	if len(mounts) == 0 {
		return "none are configured"
	}
	names := make([]string, 0, len(mounts))
	for _, m := range mounts {
		names = append(names, m.name+" at "+m.prefix)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
