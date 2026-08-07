package cmdutil

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/gooners/blob"
	"github.com/go-faster/gooners/blob/s3"
)

// BlobFlags configure the blob store a server hands large tool results to.
//
// There are two backends and they answer different deployments. The HTTP store
// serves on its own listener, because stdio has no mux and stdio-in-a-container
// is exactly the deployment that needs a URL in the first place; it is right
// whenever the agent can reach this process's port. The S3 store is right when
// it cannot — several servers on several machines, where per-process listeners
// mean N ports to expose and N base URLs to get right.
//
// S3 also does something the HTTP store cannot: one server reads what another
// wrote, by id, over the shared bucket. That is what lets a file move between
// servers without passing through the agent.
type BlobFlags struct {
	BaseURL string
	Addr    string
	Dir     string
	TTL     time.Duration

	S3Endpoint string
	S3Bucket   string
	S3Prefix   string
	S3Region   string
}

// Register registers blob store flags on fs.
func (flags *BlobFlags) Register(fs *flag.FlagSet) {
	fs.StringVar(&flags.BaseURL, "blob-base-url", "", "externally reachable URL prefix for served files, e.g. https://mcp.example.com/blob; the blob store is disabled when unset")
	fs.StringVar(&flags.Addr, "blob-addr", "", "listen address for the blob store; enables it with a base URL derived from the address when -blob-base-url is unset and the address is local")
	fs.StringVar(&flags.Dir, "blob-dir", "", "directory holding served files; defaults to a per-binary directory under the system temp directory")
	fs.DurationVar(&flags.TTL, "blob-ttl", blob.DefaultTTL, "how long a served file stays fetchable; a URL outlives the tool call in the transcript and the logs, so keep it short")

	fs.StringVar(&flags.S3Endpoint, "blob-s3-endpoint", "", "S3 endpoint, as a host[:port] or an http(s) URL; selects the S3 backend instead of the built-in HTTP one, which is what lets servers on different machines exchange files")
	fs.StringVar(&flags.S3Bucket, "blob-s3-bucket", "", "bucket holding the objects; required with -blob-s3-endpoint, and it must already exist")
	fs.StringVar(&flags.S3Prefix, "blob-s3-prefix", "", "key prefix every server sharing the bucket writes under; it is the tenancy boundary, so give each user their own, e.g. tenants/alice")
	fs.StringVar(&flags.S3Region, "blob-s3-region", "", "bucket region; optional for MinIO and for endpoints that encode it")
}

// BlobOptions configures [BlobFlags.Setup].
type BlobOptions struct {
	Name   string
	Logger *slog.Logger
}

func (o *BlobOptions) setDefaults() error {
	if o.Name == "" {
		return errors.New("server name is required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return nil
}

// Setup builds the blob store and the function that runs it until ctx is done.
//
// With none of the enabling flags it returns a [blob.Deny] store and a Run that
// returns immediately: a tool then fails with an error naming the flag, rather
// than returning a URL that resolves nowhere.
func (flags BlobFlags) Setup(ctx context.Context, opts BlobOptions) (blob.Attacher, func(context.Context) error, error) {
	if err := opts.setDefaults(); err != nil {
		return nil, nil, err
	}
	noop := func(context.Context) error { return nil }

	httpSet := flags.BaseURL != "" || flags.Addr != ""
	if flags.S3Endpoint != "" {
		if httpSet {
			return nil, nil, errors.New("-blob-s3-endpoint and -blob-addr/-blob-base-url are two different backends: pass one or the other")
		}
		store, err := flags.setupS3(ctx, opts)
		if err != nil {
			return nil, nil, err
		}
		return store, noop, nil
	}

	if !httpSet {
		return blob.Deny(fmt.Sprintf("%s was started without a blob store; pass -blob-addr, or -blob-s3-endpoint when the servers are not on one machine", opts.Name)), noop, nil
	}
	if flags.Addr == "" {
		return nil, nil, errors.New("-blob-base-url needs -blob-addr: the URL has to be served by something")
	}

	baseURL := flags.BaseURL
	if baseURL == "" {
		var err error
		if baseURL, err = localBaseURL(flags.Addr); err != nil {
			return nil, nil, err
		}
		opts.Logger.Info("derived blob base URL from the listen address", "url", baseURL)
	}

	dir := flags.Dir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), opts.Name+"-blobs")
	}
	if flags.TTL == 0 {
		flags.TTL = blob.DefaultTTL
	}

	store, err := blob.NewHTTP(blob.HTTPOptions{
		BaseURL: baseURL,
		FS:      blob.Dir(dir),
		TTL:     flags.TTL,
		Logger:  opts.Logger,
	})
	if err != nil {
		return nil, nil, errors.Wrap(err, "create blob store")
	}
	opts.Logger.Info("blob store enabled", "url", baseURL, "addr", flags.Addr, "dir", dir, "ttl", flags.TTL)

	run := func(ctx context.Context) error {
		return store.Serve(ctx, blob.ServeOptions{Addr: flags.Addr, Logger: opts.Logger})
	}
	return store, run, nil
}

// setupS3 builds the bucket-backed store.
//
// The namespace is the binary's own name rather than a flag. It is the first
// component of every id this server mints, and it says which server wrote an
// object; letting an operator set it per deployment would only make ids lie
// about their origin.
func (flags BlobFlags) setupS3(ctx context.Context, opts BlobOptions) (blob.Attacher, error) {
	if flags.S3Bucket == "" {
		return nil, errors.New("-blob-s3-endpoint needs -blob-s3-bucket")
	}
	store := &lazyStore{build: func(ctx context.Context) (blob.Attacher, error) {
		return s3.New(ctx, s3.Options{
			Endpoint:  flags.S3Endpoint,
			Bucket:    flags.S3Bucket,
			Namespace: opts.Name,
			Prefix:    flags.S3Prefix,
			Region:    flags.S3Region,
			URLTTL:    flags.TTL,
			Logger:    opts.Logger,
		})
	}}

	// Build it now so a wrong bucket or a bad credential is in the logs at
	// startup, but do not let that stop the process: everything else this
	// binary serves is unrelated to blobs, and the store rebuilds itself on the
	// next call once the endpoint is back.
	if _, err := store.get(ctx); err != nil {
		opts.Logger.Error("blob store is not usable yet; blob tools will fail until it is",
			"backend", "s3",
			"endpoint", flags.S3Endpoint,
			"bucket", flags.S3Bucket,
			"err", err,
		)
		return store, nil
	}

	opts.Logger.Info("blob store enabled",
		"backend", "s3",
		"endpoint", flags.S3Endpoint,
		"bucket", flags.S3Bucket,
		"prefix", flags.S3Prefix,
		"namespace", opts.Name,
		"url_ttl", flags.TTL,
	)
	return store, nil
}

// localBaseURL derives a base URL from a listen address, but only for an
// address that is unambiguously local. Deriving one from a wildcard bind would
// hand out a URL naming a host the server merely listens on, which is the
// plausible-wrong-answer failure this package exists to remove.
func localBaseURL(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", errors.Wrapf(err, "parse -blob-addr %q", addr)
	}
	switch strings.ToLower(host) {
	case "", "localhost", "127.0.0.1", "::1":
		return "http://localhost:" + port, nil
	}
	return "", errors.Errorf("-blob-base-url is required with -blob-addr %q: only a local address implies its own URL", addr)
}
