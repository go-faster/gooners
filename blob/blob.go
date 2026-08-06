// Package blob turns "here are some bytes" into a reference an MCP client can
// fetch out of band.
//
// A tool that produces a file has three bad options: return the bytes inline
// and pay for them in the model's context, return a host path that means
// nothing unless the agent shares a filesystem with the server, or refuse. The
// third is honest and the second fails by handing back a plausible wrong
// answer. This package is the fourth option: store the bytes, return a URL,
// and let the agent fetch them with curl, ffmpeg or a browser.
//
// A tool stores bytes through a [Store] and returns a
// [github.com/modelcontextprotocol/go-sdk/mcp.ResourceLink], which is valid
// tool-result content and costs about fifty tokens regardless of the payload.
// [Content] does both, keeping small payloads inline so the common case does
// not get slower.
//
// # Reachability is the operator's problem
//
// A [Store] cannot tell whether the URL it mints is reachable from wherever the
// agent runs. [HTTPOptions.BaseURL] is how the operator answers that, and it is
// required: a store with no base URL is a [Deny] store, because an unreachable
// URL is exactly the failure mode this package exists to remove. Behind Docker
// or a tunnel the advertised URL is not the bind address, and behind mcpgateway
// it is the upstream's own address, which the gateway does not rewrite.
//
// # A URL is a credential
//
// The object id is unguessable and it is the only thing standing between the
// world and the bytes. It also ends up in the tool result, the session
// transcript and, for anyone running the OTLP exporters, the logs. Short
// expiries are therefore mandatory rather than a nicety; see [DefaultTTL].
package blob

import (
	"context"
	"io"
	"io/fs"
	"time"

	"github.com/go-faster/errors"
)

// ErrNotFound is returned for an id that never existed or has expired. The two
// are deliberately indistinguishable: telling them apart is an oracle for
// probing ids.
var ErrNotFound = errors.New("blob not found")

// ErrDenied is returned by a [Store] that refuses every operation, either
// because the process was started without one (see [Deny]) or because policy
// forbids it.
var ErrDenied = errors.New("blob store denied by policy")

// ErrTooLarge is returned when a payload exceeds the store's size limit.
var ErrTooLarge = errors.New("blob is over the store's size limit")

// SizeUnknown is the [PutOptions.Size] value for a payload whose length is not
// known up front, which is the normal case when streaming from a remote host.
const SizeUnknown = -1

// Blob is a stored object a client can fetch out of band.
type Blob struct {
	// ID identifies the object within its store.
	ID string
	// URL is where a client fetches it. It embeds the id, so it is a
	// credential; see the package documentation.
	URL string
	// Name is the file name a client should save it as.
	Name string
	// MIMEType is what the bytes are. A reference without it is as opaque as a
	// bare id, and an agent will act on it blind.
	MIMEType string
	// Size is the stored length in bytes, always known once stored.
	Size int64
	// ExpiresAt is when the store stops serving it.
	ExpiresAt time.Time
}

// PutOptions describes a payload being stored.
type PutOptions struct {
	// Name is the file name a client should save the object as. Only the base
	// name is used. Empty gets a generated one.
	Name string
	// MIMEType is what the bytes are. Empty is guessed from Name's extension,
	// falling back to application/octet-stream.
	MIMEType string
	// Size is the payload length if known, or [SizeUnknown]. It is advisory:
	// the store records what it actually wrote. Providing it lets a store
	// refuse an oversized payload before transferring it.
	Size int64
	// TTL overrides the store's default lifetime for this object. Zero means
	// the store's default.
	TTL time.Duration
}

// Store keeps bytes somewhere a client can reach.
type Store interface {
	// Put stores r and returns a reference to it. It reads r to EOF and does
	// not close it.
	Put(ctx context.Context, r io.Reader, opts PutOptions) (Blob, error)
	// Open reads a stored object back. The caller closes it. It returns
	// [ErrNotFound] for an unknown or expired id.
	//
	// Read-back is part of the contract rather than an implementation detail:
	// the HTTP handler needs it, and so does any adapter serving the same
	// object through resources/read.
	Open(ctx context.Context, id string) (io.ReadSeekCloser, Blob, error)
	// Delete removes an object. Deleting an unknown id is not an error.
	Delete(ctx context.Context, id string) error
}

// FS is the host filesystem a store writes through.
//
// It is deliberately the method set of the repository's internal filesystem
// provider, so a confined provider satisfies it with no adapter and a store
// inherits that confinement rather than opening a second, unpoliced path to
// disk. Importers outside this module use [Dir].
type FS interface {
	Open(name string) (File, error)
	Create(name string) (File, error)
	Stat(name string) (fs.FileInfo, error)
	MkdirAll(name string, perm fs.FileMode) error
	Remove(name string) error
	RemoveAll(name string) error
}

// File is an open file handle from an [FS].
type File interface {
	io.ReadWriteSeeker
	io.Closer
	Stat() (fs.FileInfo, error)
}

// Deny returns a Store that refuses every operation with [ErrDenied], wrapped
// with reason.
//
// It is the right value for a process that was started without a reachable
// base URL: a tool then fails with an error naming the missing flag instead of
// returning a URL that resolves nowhere.
func Deny(reason string) Store { return denyStore{reason: reason} }

type denyStore struct{ reason string }

func (d denyStore) err() error { return errors.Wrap(ErrDenied, d.reason) }

func (d denyStore) Put(context.Context, io.Reader, PutOptions) (Blob, error) {
	return Blob{}, d.err()
}

func (d denyStore) Open(context.Context, string) (io.ReadSeekCloser, Blob, error) {
	return nil, Blob{}, d.err()
}

func (d denyStore) Delete(context.Context, string) error { return d.err() }
