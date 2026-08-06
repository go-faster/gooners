package cmdutil

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-faster/errors"
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/gooners/blob"
)

// BlobFlags configure the blob store a server hands large tool results to.
//
// The store serves on its own listener rather than the MCP transport's,
// because stdio has no mux and stdio-in-a-container is exactly the deployment
// that needs a URL in the first place.
type BlobFlags struct {
	BaseURL string
	Addr    string
	Dir     string
	TTL     time.Duration
}

// Register registers blob store flags on fs.
func (flags *BlobFlags) Register(fs *flag.FlagSet) {
	fs.StringVar(&flags.BaseURL, "blob-base-url", "", "externally reachable URL prefix for served files, e.g. https://mcp.example.com/blob; the blob store is disabled when unset")
	fs.StringVar(&flags.Addr, "blob-addr", "", "listen address for the blob store; enables it with a base URL derived from the address when -blob-base-url is unset and the address is local")
	fs.StringVar(&flags.Dir, "blob-dir", "", "directory holding served files; defaults to a per-binary directory under the system temp directory")
	fs.DurationVar(&flags.TTL, "blob-ttl", blob.DefaultTTL, "how long a served file stays fetchable; its URL is a credential, so keep it short")
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

// Setup builds the blob store and the function that serves it until ctx is
// done.
//
// With neither -blob-base-url nor -blob-addr it returns a [blob.Deny] store
// and a Run that returns immediately: a tool then fails with an error naming
// the flag, rather than returning a URL that resolves nowhere.
func (flags BlobFlags) Setup(opts BlobOptions) (blob.Attacher, func(context.Context) error, error) {
	if err := opts.setDefaults(); err != nil {
		return nil, nil, err
	}
	noop := func(context.Context) error { return nil }

	if flags.BaseURL == "" && flags.Addr == "" {
		return blob.Deny(fmt.Sprintf("%s was started without a blob store; pass -blob-addr, and -blob-base-url when the agent does not reach this server at that address", opts.Name)), noop, nil
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
		g, ctx := errgroup.WithContext(ctx)
		g.Go(func() error {
			store.Run(ctx)
			return nil
		})
		g.Go(func() error {
			srv := &http.Server{
				Addr:              flags.Addr,
				Handler:           store,
				ReadHeaderTimeout: 10 * time.Second,
			}
			return serveUntilDone(ctx, srv, opts.Logger)
		})
		return g.Wait()
	}
	return store, run, nil
}

// serveUntilDone runs srv until ctx is done, then shuts it down.
func serveUntilDone(ctx context.Context, srv *http.Server, lg *slog.Logger) error {
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return errors.Wrapf(err, "listen %s", srv.Addr)
	}

	parentCtx := ctx
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	})
	g.Go(func() error {
		if err := srv.Serve(ln); err != nil {
			if errors.Is(err, http.ErrServerClosed) && parentCtx.Err() != nil {
				lg.Info("blob server closed gracefully")
				return nil
			}
			return errors.Wrap(err, "blob server")
		}
		return nil
	})
	return g.Wait()
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
