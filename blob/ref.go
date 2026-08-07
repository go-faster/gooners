package blob

import (
	"context"
	"io"
	"time"

	"github.com/go-faster/errors"
)

// Source is the input half of the convention: a tool that consumes a file
// embeds it to accept an object another tool produced.
//
// Embed it rather than declaring a field, so every server names the argument
// the same thing and describes it the same way. An agent that learned the shape
// from one tool then already knows it for the rest.
//
//	type sendFileParams struct {
//	    blob.Source
//	    Chat string `json:"chat"`
//	    Path string `json:"path,omitempty" jsonschema:"..."`
//	}
//
// A tool takes an **id**, never a URL. The id names an object in the store this
// process was configured with, so there is no destination for a caller to
// choose and nothing to validate beyond the id's own shape. A URL argument, by
// contrast, is a request forgery unless it is checked against the configured
// origin — and it stops working when the URL expires mid-transfer, which
// asynchronous transfers make likely.
type Source struct {
	Blob string `json:"blob,omitempty" jsonschema:"id of a file another tool stored, e.g. as returned in the blob field of its result; use it instead of a path to consume a file this server has no filesystem access to"`
}

// Result is the output half: a tool that produces a file returns it so the
// agent gets a URL it can fetch and another tool gets an id it can consume.
//
// The two are for different readers. The agent has no storage credentials and
// needs the URL; another server has them and reads the object directly, which
// is what survives the URL expiring. Returning only one of them breaks one of
// the two.
type Result struct {
	// Blob is the id, for passing to another tool.
	Blob string `json:"blob" jsonschema:"id of the stored file; pass it to another tool's blob argument to hand the file over without downloading it"`
	// URL is where the agent fetches it.
	URL string `json:"url" jsonschema:"temporary URL to fetch the file from, e.g. with curl"`
	// Name is the file name to save it as.
	Name string `json:"name"`
	// MIMEType is what the bytes are.
	MIMEType string `json:"mime_type"`
	// Size is the stored length in bytes.
	Size int64 `json:"size" jsonschema:"file size in bytes"`
	// ExpiresAt is when url stops working. The id may outlive it.
	ExpiresAt string `json:"expires_at" jsonschema:"RFC 3339 timestamp after which url stops working; the blob id may still be usable"`
}

// Result renders a stored object as tool output.
func (b Blob) Result() Result {
	return Result{
		Blob:      b.ID,
		URL:       b.URL,
		Name:      b.Name,
		MIMEType:  b.MIMEType,
		Size:      b.Size,
		ExpiresAt: b.ExpiresAt.Format(time.RFC3339),
	}
}

// Open resolves a [Source] against a store.
//
// It exists so a handler does not have to decide what an empty argument means,
// and so the refusal is the same everywhere. The caller closes the reader.
func (s Source) Open(ctx context.Context, store Store) (io.ReadSeekCloser, Blob, error) {
	if store == nil {
		return nil, Blob{}, errors.Wrap(ErrDenied, "this server was started without a blob store")
	}
	if s.Blob == "" {
		return nil, Blob{}, errors.New("blob is required")
	}
	return store.Open(ctx, s.Blob)
}
