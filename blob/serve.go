package blob

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-faster/errors"
)

// DefaultShutdownTimeout bounds how long [HTTP.Serve] waits for in-flight
// fetches after its context is done. A fetch can legitimately be a large file,
// so it is generous.
const DefaultShutdownTimeout = 30 * time.Second

// MountPath returns the path [HTTP] must be mounted at on a shared mux for the
// URLs it hands out to resolve.
//
// It is derived from [HTTPOptions.BaseURL], which is the only thing that knows
// where the store is externally reachable. A server with its own mux mounts the
// handler here; one calling [HTTP.Serve] does not need it, since the store then
// owns the whole listener.
//
// The returned path always ends in a slash, because [net/http.ServeMux] matches
// a subtree only when the pattern does.
func (h *HTTP) MountPath() string {
	if h.prefix == "" {
		return "/"
	}
	return h.prefix + "/"
}

// ServeOptions configures [HTTP.Serve].
type ServeOptions struct {
	// Addr is the listen address, e.g. ":8090". Required.
	//
	// It is not [HTTPOptions.BaseURL]'s host: behind Docker, a tunnel or a
	// reverse proxy the address bound here is not the one an agent reaches, and
	// conflating them is what hands back a URL resolving nowhere.
	Addr string
	// ShutdownTimeout bounds the wait for in-flight fetches once ctx is done.
	// Zero means [DefaultShutdownTimeout].
	ShutdownTimeout time.Duration
	// Logger reports the lifecycle. Nil means [log/slog.Default].
	Logger *slog.Logger
}

func (o *ServeOptions) setDefaults() error {
	if o.Addr == "" {
		return errors.New("Addr is required")
	}
	if o.ShutdownTimeout == 0 {
		o.ShutdownTimeout = DefaultShutdownTimeout
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return nil
}

// Serve runs the store on its own listener until ctx is done, sweeping expired
// objects alongside, then drops everything it holds.
//
// The store owns a listener rather than borrowing the MCP transport's because
// stdio has no mux, and stdio-in-a-container is the deployment that needs a URL
// in the first place. A server that does have a mux mounts the handler at
// [HTTP.MountPath] instead and calls [HTTP.Run] for the sweep.
//
// It returns nil on a clean shutdown, so it can be the last thing a server
// waits on.
func (h *HTTP) Serve(ctx context.Context, opts ServeOptions) error {
	if err := opts.setDefaults(); err != nil {
		return err
	}

	// Listen before anything else: a port already in use is a startup error the
	// operator must see, not a goroutine failing later.
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return errors.Wrapf(err, "listen %s", opts.Addr)
	}
	opts.Logger.Info("blob store listening", "addr", ln.Addr().String(), "path", h.MountPath())

	srv := &http.Server{
		Handler: h,
		// The handler streams whole files, so only the header read is bounded.
		ReadHeaderTimeout: 10 * time.Second,
	}

	// The sweep and the shutdown trigger hang off a context this function can
	// also cancel, so that Serve failing on its own — rather than because ctx
	// ended — still unblocks both waits below.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	sweptDone := make(chan struct{})
	go func() {
		defer close(sweptDone)
		h.Run(runCtx)
	}()

	shutdownDone := make(chan error, 1)
	go func() {
		<-runCtx.Done()
		// Shutdown gets its own context: the one that just expired cannot also
		// bound the grace period it triggered.
		shutCtx, cancel := context.WithTimeout(context.Background(), opts.ShutdownTimeout)
		defer cancel()
		shutdownDone <- srv.Shutdown(shutCtx)
	}()

	serveErr := srv.Serve(ln)
	if errors.Is(serveErr, http.ErrServerClosed) && ctx.Err() != nil {
		serveErr = nil
	}
	stop()

	// Wait for the sweep to purge and for Shutdown to report, so returning
	// means the store is actually finished with its objects.
	<-sweptDone
	if err := <-shutdownDone; err != nil && serveErr == nil {
		serveErr = errors.Wrap(err, "shut down blob server")
	}
	if serveErr != nil {
		return errors.Wrap(serveErr, "blob server")
	}
	opts.Logger.Info("blob store closed")
	return nil
}
