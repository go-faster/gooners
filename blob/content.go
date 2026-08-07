package blob

import (
	"bytes"
	"context"
	"io"
	"mime"
	"strings"
	"unicode/utf8"

	"github.com/go-faster/errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/blobutil"
)

// DefaultInlineLimit is the size below which [Content] keeps a payload in the
// tool result instead of storing it. It exists so the common case does not get
// slower: for a small file the extra round trip costs more than the tokens.
const DefaultInlineLimit = 256 << 10

// ContentOptions configures [Content].
type ContentOptions struct {
	PutOptions

	// InlineLimit is the largest payload returned inline. Zero means
	// [DefaultInlineLimit]; a negative value always stores.
	InlineLimit int64
}

// Content stores r and returns tool-result content referring to it: the bytes
// inline when they are small enough to be worth it, a
// [github.com/modelcontextprotocol/go-sdk/mcp.ResourceLink] otherwise. The
// returned [Blob] is zero when the payload was inlined.
//
// Only text and images are ever inlined. Other binary content has nowhere to
// go inline — an embedded resource needs a URI, and inventing one that resolves
// nowhere is the failure this package exists to remove — so it is always
// stored, however small.
//
// It reads r to EOF and does not close it. A payload above the limit streams
// into the store rather than being buffered.
func Content(ctx context.Context, s Store, r io.Reader, opts ContentOptions) ([]mcp.Content, Blob, error) {
	limit := opts.InlineLimit
	switch {
	case limit == 0:
		limit = DefaultInlineLimit
	case limit < 0:
		limit = 0
	}

	mimeType := blobutil.ContentType(opts.MIMEType, blobutil.CleanName(opts.Name, "blob"))

	// Read one byte past the limit: that is what distinguishes "fits" from
	// "does not", and it is the head of the stream either way.
	head, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, Blob{}, errors.Wrap(err, "read payload")
	}

	if int64(len(head)) <= limit {
		if c, ok := inline(head, mimeType); ok {
			return []mcp.Content{c}, Blob{}, nil
		}
	}

	opts.PutOptions.MIMEType = mimeType
	b, err := s.Put(ctx, io.MultiReader(bytes.NewReader(head), r), opts.PutOptions)
	if err != nil {
		return nil, Blob{}, err
	}
	return []mcp.Content{&mcp.ResourceLink{
		URI:      b.URL,
		Name:     b.Name,
		MIMEType: b.MIMEType,
		Size:     &b.Size,
	}}, b, nil
}

// inline renders data as tool-result content, reporting whether the type can
// be represented inline at all.
func inline(data []byte, mimeType string) (mcp.Content, bool) {
	base, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		return nil, false
	}
	switch {
	case strings.HasPrefix(base, "image/") && base != "image/svg+xml":
		return &mcp.ImageContent{Data: data, MIMEType: mimeType}, true
	case isTextual(base) && utf8.Valid(data):
		return &mcp.TextContent{Text: string(data)}, true
	default:
		return nil, false
	}
}

// isTextual reports whether a media type is text an agent can read directly.
func isTextual(base string) bool {
	if strings.HasPrefix(base, "text/") {
		return true
	}
	if strings.HasSuffix(base, "+json") || strings.HasSuffix(base, "+xml") {
		return true
	}
	switch base {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml", "application/toml":
		return true
	}
	return false
}
