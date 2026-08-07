package blob

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/gooners/internal/blobutil"
)

// DefaultTTL is how long an object stays fetchable. It is short because a URL
// outlives the tool call in the transcript, and because in a bucket-backed
// store it is a bearer token; see "Reaching an object" in the package
// documentation.
const DefaultTTL = 15 * time.Minute

// DefaultMaxSize bounds one object. Without a cap, one tool call fills the
// disk.
const DefaultMaxSize = 1 << 30 // 1 GiB

// objectsDir holds the stored bytes inside the store's filesystem. It is a
// fixed name so leftovers from a previous process can be cleared at startup
// without enumerating the filesystem.
const objectsDir = "objects"

// HTTPOptions configures [NewHTTP].
type HTTPOptions struct {
	// BaseURL is the externally reachable prefix the handler is served under,
	// e.g. https://mcp.example.com/blob. It is required, and it is the
	// operator's assertion that a client can reach this server there; see the
	// package documentation.
	//
	// Its path is stripped from incoming requests, so mounting the handler at
	// that path needs no [net/http.StripPrefix].
	BaseURL string
	// FS stores the bytes. It is required, and it is what confines the store:
	// pass a provider rooted at a directory this process may fill.
	FS FS
	// TTL is the default object lifetime. Zero means [DefaultTTL]; it may not
	// be negative.
	TTL time.Duration
	// MaxSize bounds one object in bytes. Zero means [DefaultMaxSize].
	MaxSize int64
	// Now is the clock, so tests need not sleep.
	Now func() time.Time
	// Logger reports sweeps and serve failures.
	Logger *slog.Logger
}

func (o *HTTPOptions) setDefaults() error {
	if o.BaseURL == "" {
		return errors.New("BaseURL is required: a store that cannot advertise a reachable URL must be blob.Deny instead")
	}
	u, err := url.Parse(o.BaseURL)
	if err != nil {
		return errors.Wrapf(err, "parse BaseURL %q", o.BaseURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.Errorf("BaseURL must be an absolute http(s) URL, got %q", o.BaseURL)
	}
	if u.Host == "" {
		return errors.Errorf("BaseURL must name a host, got %q", o.BaseURL)
	}
	o.BaseURL = strings.TrimSuffix(o.BaseURL, "/")
	if o.FS == nil {
		return errors.New("FS is required")
	}
	if o.TTL < 0 {
		return errors.Errorf("TTL must not be negative, got %s", o.TTL)
	}
	if o.TTL == 0 {
		o.TTL = DefaultTTL
	}
	if o.MaxSize < 0 {
		return errors.Errorf("MaxSize must not be negative, got %d", o.MaxSize)
	}
	if o.MaxSize == 0 {
		o.MaxSize = DefaultMaxSize
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return nil
}

// HTTP is a [Store] that serves its objects over HTTP from a directory.
//
// It needs no infrastructure, which makes it the right backend whenever the
// agent can reach this process's port. It owns its own listener rather than
// borrowing the MCP transport's: every binary here defaults to stdio, which
// has no mux, and stdio-in-a-container is precisely the case that motivates
// the package.
//
// Objects do not survive the process. The index is in memory and the object
// directory is cleared at startup, so nothing accumulates across restarts.
type HTTP struct {
	baseURL string
	prefix  string
	fs      FS
	ttl     time.Duration
	maxSize int64
	now     func() time.Time
	lg      *slog.Logger

	mu      sync.Mutex
	objects map[string]object
}

// object is one served reference: the metadata handed to the agent, plus where
// the bytes actually are.
type object struct {
	blob Blob
	// fs and path locate the bytes. For a stored object they are the store's
	// own filesystem and objects/<id>; for an attached one they are the
	// caller's mount and the file's name within it.
	fs   FS
	path string
	// attached marks bytes the store does not own. Expiry drops the reference
	// and leaves the file where it was.
	attached bool
}

var (
	_ Attacher     = (*HTTP)(nil)
	_ http.Handler = (*HTTP)(nil)
)

// NewHTTP creates a store serving from opts.FS. It clears any objects left by
// a previous process.
func NewHTTP(opts HTTPOptions) (*HTTP, error) {
	if err := opts.setDefaults(); err != nil {
		return nil, err
	}
	u, err := url.Parse(opts.BaseURL)
	if err != nil {
		return nil, errors.Wrapf(err, "parse BaseURL %q", opts.BaseURL)
	}

	// Leftovers from a previous run are unreachable anyway, since the index is
	// in memory; clearing them is about disk, not correctness, so a failure is
	// worth a warning rather than a refusal to start.
	if err := opts.FS.RemoveAll(objectsDir); err != nil {
		opts.Logger.Warn("could not clear the object directory", "err", err)
	}
	if err := opts.FS.MkdirAll(objectsDir, 0o700); err != nil {
		return nil, errors.Wrap(err, "create object directory")
	}

	return &HTTP{
		baseURL: opts.BaseURL,
		prefix:  strings.TrimSuffix(u.Path, "/"),
		fs:      opts.FS,
		ttl:     opts.TTL,
		maxSize: opts.MaxSize,
		now:     opts.Now,
		lg:      opts.Logger,
		objects: make(map[string]object),
	}, nil
}

// Put stores r and returns a fetchable reference to it.
func (h *HTTP) Put(ctx context.Context, r io.Reader, opts PutOptions) (Blob, error) {
	if err := ctx.Err(); err != nil {
		return Blob{}, err
	}
	// A declared size lets an oversized payload be refused before it is
	// transferred; an undeclared one is caught by the LimitReader below.
	if opts.Size > 0 && opts.Size > h.maxSize {
		return Blob{}, errors.Wrapf(ErrTooLarge, "%d bytes, limit is %d", opts.Size, h.maxSize)
	}

	id, err := blobutil.NewID()
	if err != nil {
		return Blob{}, err
	}
	name := blobutil.CleanName(opts.Name, id)

	n, err := h.write(id, r)
	if err != nil {
		return Blob{}, err
	}

	b := h.describe(id, name, opts, n)
	h.mu.Lock()
	h.objects[id] = object{blob: b, fs: h.fs, path: objectPath(id)}
	h.mu.Unlock()
	return b, nil
}

// Attach serves a file that already exists in src, without copying it. The
// bytes stay where they are, and expiry drops only the reference.
func (h *HTTP) Attach(ctx context.Context, src FS, name string, opts PutOptions) (Blob, error) {
	if err := ctx.Err(); err != nil {
		return Blob{}, err
	}
	if src == nil {
		return Blob{}, errors.New("attach: src filesystem is required")
	}

	// Statting through src is also the confinement check: a path outside its
	// root, symlinked or not, never gets an id.
	info, err := src.Stat(name)
	if err != nil {
		return Blob{}, errors.Wrapf(err, "stat %q", name)
	}
	if info.IsDir() {
		return Blob{}, errors.Errorf("%q is a directory", name)
	}
	if !info.Mode().IsRegular() {
		// A device or a fifo would make the handler block on a read that never
		// ends, holding a connection for as long as the client waits.
		return Blob{}, errors.Errorf("%q is not a regular file", name)
	}
	if info.Size() > h.maxSize {
		return Blob{}, errors.Wrapf(ErrTooLarge, "%d bytes, limit is %d", info.Size(), h.maxSize)
	}

	id, err := blobutil.NewID()
	if err != nil {
		return Blob{}, err
	}
	if opts.Name == "" {
		opts.Name = path.Base(name)
	}

	b := h.describe(id, blobutil.CleanName(opts.Name, id), opts, info.Size())
	h.mu.Lock()
	h.objects[id] = object{blob: b, fs: src, path: name, attached: true}
	h.mu.Unlock()
	return b, nil
}

// describe builds the reference handed to the agent.
func (h *HTTP) describe(id, name string, opts PutOptions, size int64) Blob {
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = h.ttl
	}
	return Blob{
		ID:        id,
		URL:       h.baseURL + "/" + id + "/" + url.PathEscape(name),
		Name:      name,
		MIMEType:  blobutil.ContentType(opts.MIMEType, name),
		Size:      size,
		ExpiresAt: h.now().Add(ttl),
	}
}

// write copies r into the object file, removing it if anything goes wrong so a
// failed Put leaves nothing behind.
func (h *HTTP) write(id string, r io.Reader) (int64, error) {
	f, err := h.fs.Create(objectPath(id))
	if err != nil {
		return 0, errors.Wrap(err, "create object")
	}

	n, copyErr := io.Copy(f, io.LimitReader(r, h.maxSize+1))
	closeErr := f.Close()
	switch {
	case copyErr != nil:
		err = errors.Wrap(copyErr, "store object")
	case closeErr != nil:
		err = errors.Wrap(closeErr, "store object")
	case n > h.maxSize:
		err = errors.Wrapf(ErrTooLarge, "over %d bytes", h.maxSize)
	}
	if err != nil {
		if rmErr := h.fs.Remove(objectPath(id)); rmErr != nil {
			h.lg.Warn("could not remove a partially stored object", "id", id, "err", rmErr)
		}
		return 0, err
	}
	return n, nil
}

// Open reads a stored object back.
func (h *HTTP) Open(ctx context.Context, id string) (io.ReadSeekCloser, Blob, error) {
	if err := ctx.Err(); err != nil {
		return nil, Blob{}, err
	}
	o, ok := h.lookup(id)
	if !ok {
		return nil, Blob{}, errors.Wrapf(ErrNotFound, "id %q", id)
	}
	f, err := o.fs.Open(o.path)
	if err != nil {
		return nil, Blob{}, errors.Wrapf(ErrNotFound, "id %q: %s", id, err)
	}
	return f, o.blob, nil
}

// Delete drops an object. Bytes the store does not own stay where they are.
func (h *HTTP) Delete(_ context.Context, id string) error {
	h.mu.Lock()
	o, known := h.objects[id]
	delete(h.objects, id)
	h.mu.Unlock()
	if !known {
		return nil
	}
	if err := h.discard(o); err != nil {
		return errors.Wrapf(err, "remove object %q", id)
	}
	return nil
}

// discard removes the bytes behind an object, if they were the store's to
// remove in the first place.
func (h *HTTP) discard(o object) error {
	if o.attached {
		return nil
	}
	return o.fs.Remove(o.path)
}

// lookup returns the object if it exists and has not expired, dropping it if
// it has.
func (h *HTTP) lookup(id string) (object, bool) {
	h.mu.Lock()
	o, ok := h.objects[id]
	expired := ok && !h.now().Before(o.blob.ExpiresAt)
	if expired {
		delete(h.objects, id)
	}
	h.mu.Unlock()

	if !ok {
		return object{}, false
	}
	if expired {
		if err := h.discard(o); err != nil {
			h.lg.Warn("could not remove an expired object", "id", id, "err", err)
		}
		return object{}, false
	}
	return o, true
}

// Run sweeps expired objects until ctx is done, then removes everything the
// store holds. It is meant to be called as `go store.Run(ctx)`.
func (h *HTTP) Run(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval(h.ttl))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			h.purge()
			return
		case <-ticker.C:
			h.sweep()
		}
	}
}

func (h *HTTP) sweep() {
	now := h.now()
	h.mu.Lock()
	var expired []object
	for id, o := range h.objects {
		if !now.Before(o.blob.ExpiresAt) {
			expired = append(expired, o)
			delete(h.objects, id)
		}
	}
	h.mu.Unlock()
	h.remove(expired)
}

func (h *HTTP) purge() {
	h.mu.Lock()
	held := make([]object, 0, len(h.objects))
	for _, o := range h.objects {
		held = append(held, o)
	}
	clear(h.objects)
	h.mu.Unlock()
	h.remove(held)
}

func (h *HTTP) remove(objects []object) {
	for _, o := range objects {
		if err := h.discard(o); err != nil {
			h.lg.Warn("could not remove an object", "id", o.blob.ID, "err", err)
		}
	}
	if len(objects) > 0 {
		h.lg.Debug("dropped blobs", "count", len(objects))
	}
}

// ServeHTTP serves one object per request, keyed by the id in the path.
func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := h.requestID(r.URL.Path)
	o, ok := h.lookup(id)
	if !ok {
		// Unknown, expired and malformed ids are one response: anything else
		// tells a prober which ids exist.
		http.NotFound(w, r)
		return
	}

	f, err := o.fs.Open(o.path)
	if err != nil {
		// An attached file can be deleted or replaced by whoever owns it while
		// a reference is live, so this is an ordinary outcome, not a fault.
		h.lg.Warn("could not open a served object", "id", id, "err", err)
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()

	// These bytes are whatever a tool was handed, served from the operator's
	// origin: never let a browser render them, and never let it sniff a type
	// out of the content.
	w.Header().Set("Content-Type", blobutil.ServeType(o.blob.MIMEType))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", blobutil.ContentDisposition(o.blob.Name))
	w.Header().Set("Cache-Control", "no-store")

	// A zero modtime omits Last-Modified, which nothing here can use. Range
	// still works, and it matters: resuming a large fetch is half the point.
	http.ServeContent(w, r, "", time.Time{}, f)
}

// requestID extracts the object id from a request path, tolerating the handler
// being mounted either at BaseURL's path or at the root of its own listener.
func (h *HTTP) requestID(p string) string {
	if h.prefix != "" {
		p = strings.TrimPrefix(p, h.prefix)
	}
	p = strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		p = p[:i]
	}
	return p
}

func objectPath(id string) string { return path.Join(objectsDir, id) }

// sweepInterval keeps the sweep frequent enough that an expired object does
// not sit on disk for much longer than its TTL, without spinning on a very
// short one.
func sweepInterval(ttl time.Duration) time.Duration {
	return min(max(ttl/4, 10*time.Second), 5*time.Minute)
}
