package blob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/go-faster/errors"
)

// DefaultTTL is how long an object stays fetchable. It is short because the
// URL is a credential that outlives the tool call in the transcript.
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
	objects map[string]Blob
}

var (
	_ Store        = (*HTTP)(nil)
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
		objects: make(map[string]Blob),
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

	id, err := newID()
	if err != nil {
		return Blob{}, err
	}
	name := cleanName(opts.Name, id)

	n, err := h.write(id, r)
	if err != nil {
		return Blob{}, err
	}

	ttl := opts.TTL
	if ttl <= 0 {
		ttl = h.ttl
	}
	b := Blob{
		ID:        id,
		URL:       h.baseURL + "/" + id + "/" + url.PathEscape(name),
		Name:      name,
		MIMEType:  contentType(opts.MIMEType, name),
		Size:      n,
		ExpiresAt: h.now().Add(ttl),
	}

	h.mu.Lock()
	h.objects[id] = b
	h.mu.Unlock()
	return b, nil
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
	b, ok := h.lookup(id)
	if !ok {
		return nil, Blob{}, errors.Wrapf(ErrNotFound, "id %q", id)
	}
	f, err := h.fs.Open(objectPath(id))
	if err != nil {
		return nil, Blob{}, errors.Wrapf(ErrNotFound, "id %q: %s", id, err)
	}
	return f, b, nil
}

// Delete removes an object.
func (h *HTTP) Delete(_ context.Context, id string) error {
	h.mu.Lock()
	_, known := h.objects[id]
	delete(h.objects, id)
	h.mu.Unlock()
	if !known {
		return nil
	}
	if err := h.fs.Remove(objectPath(id)); err != nil {
		return errors.Wrapf(err, "remove object %q", id)
	}
	return nil
}

// lookup returns the object if it exists and has not expired, dropping it if
// it has.
func (h *HTTP) lookup(id string) (Blob, bool) {
	h.mu.Lock()
	b, ok := h.objects[id]
	expired := ok && !h.now().Before(b.ExpiresAt)
	if expired {
		delete(h.objects, id)
	}
	h.mu.Unlock()

	if !ok {
		return Blob{}, false
	}
	if expired {
		if err := h.fs.Remove(objectPath(id)); err != nil {
			h.lg.Warn("could not remove an expired object", "id", id, "err", err)
		}
		return Blob{}, false
	}
	return b, true
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
	var expired []string
	for id, b := range h.objects {
		if !now.Before(b.ExpiresAt) {
			expired = append(expired, id)
			delete(h.objects, id)
		}
	}
	h.mu.Unlock()
	h.remove(expired)
}

func (h *HTTP) purge() {
	h.mu.Lock()
	ids := make([]string, 0, len(h.objects))
	for id := range h.objects {
		ids = append(ids, id)
	}
	clear(h.objects)
	h.mu.Unlock()
	h.remove(ids)
}

func (h *HTTP) remove(ids []string) {
	for _, id := range ids {
		if err := h.fs.Remove(objectPath(id)); err != nil {
			h.lg.Warn("could not remove an object", "id", id, "err", err)
		}
	}
	if len(ids) > 0 {
		h.lg.Debug("removed expired blobs", "count", len(ids))
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
	b, ok := h.lookup(id)
	if !ok {
		// Unknown, expired and malformed ids are one response: anything else
		// tells a prober which ids exist.
		http.NotFound(w, r)
		return
	}

	f, err := h.fs.Open(objectPath(id))
	if err != nil {
		h.lg.Warn("could not open a stored object", "id", id, "err", err)
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()

	// These bytes are whatever a tool was handed, served from the operator's
	// origin: never let a browser render them, and never let it sniff a type
	// out of the content.
	w.Header().Set("Content-Type", serveType(b.MIMEType))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", contentDisposition(b.Name))
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

// newID returns an unguessable object id. It is the only credential guarding
// the object, so it is 128 bits from a cryptographic source.
func newID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", errors.Wrap(err, "generate blob id")
	}
	return hex.EncodeToString(buf[:]), nil
}

// cleanName reduces a caller-supplied name to a base name safe to put in a URL
// path and a Content-Disposition header.
func cleanName(name, id string) string {
	// Reduce to a base name first, so a path only loses its directories rather
	// than being flattened into one long name.
	name = path.Base(strings.TrimSpace(strings.ReplaceAll(name, `\`, "/")))
	// What is left cannot break out of a quoted Content-Disposition or inject
	// a header.
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return id + ".bin"
	}
	return name
}

// contentType picks the declared type, guessing from the extension when the
// caller had none.
func contentType(declared, name string) string {
	if declared != "" {
		return declared
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// serveType downgrades the types a browser would execute on this origin. The
// declared type still reaches the agent on the ResourceLink, which is where it
// is useful; what goes on the wire is only what a fetcher needs.
func serveType(t string) string {
	base, _, err := mime.ParseMediaType(t)
	if err != nil {
		return "application/octet-stream"
	}
	switch base {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "application/xml", "text/xml":
		return "application/octet-stream"
	}
	return t
}

func contentDisposition(name string) string {
	// name is already stripped of quotes, backslashes and control characters,
	// so the quoted form cannot be broken out of. The RFC 5987 form carries
	// non-ASCII names that the quoted one cannot.
	return mime.FormatMediaType("attachment", map[string]string{"filename": name})
}

// sweepInterval keeps the sweep frequent enough that an expired object does
// not sit on disk for much longer than its TTL, without spinning on a very
// short one.
func sweepInterval(ttl time.Duration) time.Duration {
	return min(max(ttl/4, 10*time.Second), 5*time.Minute)
}
