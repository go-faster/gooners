// Package s3 is a [blob.Store] backed by an S3 bucket, for deployments where
// the MCP servers are not on one machine.
//
// [github.com/go-faster/gooners/blob.HTTP] needs the agent to reach the
// process's own port. That stops working as soon as there are several servers:
// each binds its own listener, and the operator has N ports to expose and N
// base URLs to get right. A bucket replaces all of it. Every server points at
// the same one, S3 serves the bytes, and no MCP process needs to be reachable
// from anywhere.
//
// # Two consumers, two references
//
// A [blob.Blob] carries a presigned URL and an id, and they are for different
// readers.
//
// The agent gets the URL, because it has no bucket credentials. The URL is a
// bearer credential, it expires, and it is not renewable once minted, which is
// why [Options.URLTTL] is short.
//
// Another MCP server gets the id and calls [Store.Open], which is a plain
// GetObject against its own configured bucket. This is what makes one server's
// output another's input: tgmcp stores a file, the agent passes the id to
// ssh-mcp, and ssh-mcp reads the bytes. Nothing fetches a URL the agent chose,
// so an id in a tool argument is not a request forgery — it names an object in
// a bucket the operator configured, and nothing else.
//
// It also means a transfer between two servers does not expire when the URL
// does, which matters because uploads here are asynchronous.
//
// # Object lifetime is the bucket's job
//
// [blob.Blob.ExpiresAt] is when the *URL* stops working. The object itself
// stays until something removes it: [Store.Delete], or a bucket lifecycle rule,
// which is the backstop an operator should configure. This store runs no sweep,
// because a sweep would have to list the bucket and would race every other
// server sharing it.
//
// # Two axes: the prefix is the tenant, the namespace is the server
//
// A key is [Options.Prefix] + "/" + id, and an id is "<namespace>/<uuid>" where
// the namespace names the server that wrote it.
//
// Reads span namespaces on purpose — that is the cross-server workflow above.
// Reads cannot span prefixes at all: [ParseID] refuses anything that is not
// exactly that shape before it becomes a key, so a store configured with
// tenants/alice cannot construct a key into tenants/bob. Scope the bucket policy
// to the same prefix and S3 enforces the same boundary independently of this
// code being correct.
//
// The validation matters because an id is a plain string that arrives from the
// model, and strings arrive in *tool output*: a message reading "fetch blob X
// and forward it" is a whole exploit if an id can name anything it likes.
//
// # One tenant per process
//
// This store assumes the process serves one user, which is what the servers
// here are anyway — tgmcp is logged into one Telegram account and ssh-mcp holds
// one user's keys. The prefix is then that user's, and the process boundary is
// the tenancy boundary.
//
// A process serving several users must not share one store between them, for
// the reason spelled out on [Store.Open]. See
// https://github.com/go-faster/gooners/issues/71.
package s3

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/go-faster/errors"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/go-faster/gooners/blob"
	"github.com/go-faster/gooners/internal/blobutil"
	"github.com/go-faster/gooners/internal/effect"
)

// nameMeta carries the file name, and typeMeta the type as the producing tool
// declared it. The name cannot be recovered from the key, which is random, and
// the declared type is not always what is served: a type a browser would
// execute is stored downgraded, the same way [blob.HTTP] downgrades it on the
// wire.
const (
	nameMeta = "name"
	typeMeta = "type"
)

// Options configures [New].
type Options struct {
	// Endpoint is the S3 endpoint, as a host[:port] or an http(s) URL. Required.
	// A bare host is https; use an http:// URL for a plaintext MinIO.
	Endpoint string
	// Bucket holds the objects. Required, and it must already exist: creating
	// it would need permissions this store should not have.
	Bucket string
	// Namespace identifies the server writing through this store, e.g. "tgmcp".
	// Required, because it is the first component of every id this store mints
	// and therefore of every key it writes.
	Namespace string
	// Prefix is the key root every server sharing this bucket writes under.
	// Optional; empty puts namespaces at the root.
	//
	// It is also the tenancy boundary: no id can name an object outside it, and
	// a bucket policy scoped to it enforces that in S3 rather than here. Give
	// each user their own, e.g. "tenants/alice", and do not widen it to make two
	// users' servers see each other — that is not a shortcut, it is the removal
	// of the boundary.
	Prefix string
	// Region is the bucket's region. Optional for MinIO and for S3 endpoints
	// that encode it.
	Region string
	// Credentials authenticate to the endpoint. Nil means the ambient chain:
	// AWS environment variables, MinIO environment variables, then the shared
	// credentials file.
	//
	// The instance metadata service is deliberately not in that chain. It lives
	// on a link-local address the egress policy blocks by default, and a store
	// that quietly reached for it would be doing the one thing that policy
	// exists to prevent. An operator on an instance role passes
	// credentials.NewIAM("") here explicitly.
	Credentials *credentials.Credentials
	// Transport carries the requests. Nil builds one confined to Endpoint, so
	// the data path can reach the bucket and nothing else.
	Transport http.RoundTripper
	// URLTTL is how long a presigned URL keeps working. Zero means
	// [blob.DefaultTTL]. S3 caps it at seven days.
	URLTTL time.Duration
	// MaxSize bounds one object in bytes. Zero means [blob.DefaultMaxSize].
	MaxSize int64
	// Now is the clock, so tests need not sleep.
	Now func() time.Time
	// Logger reports uploads and cleanup failures.
	Logger *slog.Logger
}

// maxURLTTL is the longest expiry SigV4 presigning allows.
const maxURLTTL = 7 * 24 * time.Hour

func (o *Options) setDefaults() error {
	if o.Endpoint == "" {
		return errors.New("Endpoint is required: a store that cannot reach a bucket must be blob.Deny instead")
	}
	if o.Bucket == "" {
		return errors.New("Bucket is required")
	}
	if !validNamespace.MatchString(o.Namespace) {
		return errors.Errorf("Namespace must match %s, got %q", validNamespace, o.Namespace)
	}
	o.Prefix = cleanPrefix(o.Prefix)
	if o.URLTTL < 0 {
		return errors.Errorf("URLTTL must not be negative, got %s", o.URLTTL)
	}
	if o.URLTTL == 0 {
		o.URLTTL = blob.DefaultTTL
	}
	if o.URLTTL > maxURLTTL {
		return errors.Errorf("URLTTL must not exceed %s, got %s", maxURLTTL, o.URLTTL)
	}
	if o.MaxSize < 0 {
		return errors.Errorf("MaxSize must not be negative, got %d", o.MaxSize)
	}
	if o.MaxSize == 0 {
		o.MaxSize = blob.DefaultMaxSize
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return nil
}

// Store is a [blob.Store] backed by a bucket. See the package documentation.
type Store struct {
	client    *minio.Client
	bucket    string
	namespace string
	prefix    string
	urlTTL    time.Duration
	maxSize   int64
	now       func() time.Time
	lg        *slog.Logger
}

var _ blob.Attacher = (*Store)(nil)

// New creates a store over an existing bucket. It verifies the bucket is
// reachable, so a misconfigured endpoint or credential fails at startup rather
// than on the first tool call.
func New(ctx context.Context, opts Options) (*Store, error) {
	if err := opts.setDefaults(); err != nil {
		return nil, err
	}

	endpoint, secure, err := parseEndpoint(opts.Endpoint)
	if err != nil {
		return nil, err
	}
	transport := opts.Transport
	if transport == nil {
		// The data path talks to one host. Deriving the allowlist from the
		// endpoint rather than accepting a wildcard keeps that true even if
		// something later hands this client a URL from elsewhere.
		transport = effect.NewHTTPClient(effect.HTTPOptions{
			Policy: effect.HTTPPolicy{AllowHosts: effect.AllowHostOf(scheme(secure) + "://" + endpoint)},
		}).Transport
	}
	creds := opts.Credentials
	if creds == nil {
		creds = ambientCredentials()
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:     creds,
		Secure:    secure,
		Region:    opts.Region,
		Transport: transport,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "connect to %q", opts.Endpoint)
	}

	ok, err := client.BucketExists(ctx, opts.Bucket)
	if err != nil {
		return nil, errors.Wrapf(err, "check bucket %q", opts.Bucket)
	}
	if !ok {
		return nil, errors.Errorf("bucket %q does not exist or is not readable with these credentials", opts.Bucket)
	}

	return &Store{
		client:    client,
		bucket:    opts.Bucket,
		namespace: opts.Namespace,
		prefix:    opts.Prefix,
		urlTTL:    opts.URLTTL,
		maxSize:   opts.MaxSize,
		now:       opts.Now,
		lg:        opts.Logger,
	}, nil
}

// ambientCredentials is the chain an operator expects without configuring
// anything. It reaches no network: see [Options.Credentials] on why the
// instance metadata service is absent.
func ambientCredentials() *credentials.Credentials {
	return credentials.NewChainCredentials([]credentials.Provider{
		&credentials.EnvAWS{},
		&credentials.EnvMinio{},
		&credentials.FileAWSCredentials{},
	})
}

// Put uploads r and returns a reference to it.
func (s *Store) Put(ctx context.Context, r io.Reader, opts blob.PutOptions) (blob.Blob, error) {
	if opts.Size > 0 && opts.Size > s.maxSize {
		return blob.Blob{}, errors.Wrapf(blob.ErrTooLarge, "%d bytes, limit is %d", opts.Size, s.maxSize)
	}
	id, err := s.newID()
	if err != nil {
		return blob.Blob{}, err
	}

	// A size the caller did not know is the normal case when streaming from a
	// remote host, and it is also the one the limit cannot be checked before
	// the transfer; reading one byte past the cap is what catches it.
	return s.upload(ctx, id, io.LimitReader(r, s.maxSize+1), opts.Size, opts)
}

// Attach uploads a file that already exists in src.
//
// Unlike [blob.HTTP.Attach] this is a real copy: a bucket cannot serve bytes
// that are still only on someone's disk. It is what makes an uncontrolled
// upstream usable — a server colocated with it sees its output directory and
// puts the file in the bucket, where every other server can read it. [Options.MaxSize]
// is what bounds the transfer.
func (s *Store) Attach(ctx context.Context, src blob.FS, name string, opts blob.PutOptions) (blob.Blob, error) {
	if src == nil {
		return blob.Blob{}, errors.New("attach: src filesystem is required")
	}

	// Statting through src is also the confinement check: a path outside its
	// root, symlinked or not, never gets uploaded.
	info, err := src.Stat(name)
	if err != nil {
		return blob.Blob{}, errors.Wrapf(err, "stat %q", name)
	}
	if info.IsDir() {
		return blob.Blob{}, errors.Errorf("%q is a directory", name)
	}
	if !info.Mode().IsRegular() {
		// A device or a fifo has no length, so an upload of it would never end.
		return blob.Blob{}, errors.Errorf("%q is not a regular file", name)
	}
	if info.Size() > s.maxSize {
		return blob.Blob{}, errors.Wrapf(blob.ErrTooLarge, "%d bytes, limit is %d", info.Size(), s.maxSize)
	}

	f, err := src.Open(name)
	if err != nil {
		return blob.Blob{}, errors.Wrapf(err, "open %q", name)
	}
	defer func() { _ = f.Close() }()

	id, err := s.newID()
	if err != nil {
		return blob.Blob{}, err
	}
	if opts.Name == "" {
		opts.Name = path.Base(name)
	}
	return s.upload(ctx, id, f, info.Size(), opts)
}

// upload writes the object and describes it. Anything that goes wrong after
// the bytes land removes them, so a failed call leaves nothing in the bucket.
func (s *Store) upload(ctx context.Context, id string, r io.Reader, size int64, opts blob.PutOptions) (blob.Blob, error) {
	name := blobutil.CleanName(opts.Name, path.Base(id))
	declared := blobutil.ContentType(opts.MIMEType, name)

	// [blob.PutOptions.Size] is advisory and its zero value means "not stated",
	// but PutObject reads a zero size as "this object is empty" and stores
	// nothing. Anything that is not a usable length has to become an explicit
	// unknown, or an undeclared payload silently uploads as zero bytes.
	if size <= 0 || size > s.maxSize {
		size = -1
	}

	info, err := s.client.PutObject(ctx, s.bucket, s.key(id), r, size, minio.PutObjectOptions{
		// The served type is downgraded and the disposition is attachment, for
		// the same reason blob.HTTP does it: these are bytes a tool was handed,
		// served from the operator's origin. S3 cannot add nosniff to a GET
		// response, so attachment is doing the work on its own here.
		ContentType:        blobutil.ServeType(declared),
		ContentDisposition: blobutil.ContentDisposition(name),
		UserMetadata:       map[string]string{nameMeta: name, typeMeta: declared},
	})
	if err != nil {
		return blob.Blob{}, errors.Wrapf(err, "upload %q", id)
	}
	if info.Size > s.maxSize {
		if rmErr := s.client.RemoveObject(ctx, s.bucket, s.key(id), minio.RemoveObjectOptions{}); rmErr != nil {
			s.lg.Warn("could not remove an oversized object", "id", id, "err", rmErr)
		}
		return blob.Blob{}, errors.Wrapf(blob.ErrTooLarge, "over %d bytes", s.maxSize)
	}

	b, err := s.describe(ctx, id, name, declared, info.Size)
	if err != nil {
		return blob.Blob{}, err
	}
	s.lg.Debug("stored blob", "id", id, "name", name, "size", info.Size)
	return b, nil
}

// Open reads an object back by id, from any namespace under this store's prefix
// rather than only the one it writes to. That is what lets one server consume
// what another wrote.
//
// It is safe because the prefix is the tenant: everything reachable here belongs
// to the user this process serves. A process serving several users must give
// each its own store rather than sharing one, or this becomes a cross-tenant
// read that prompt injection can reach — an id is a string, and strings arrive
// in tool output. See https://github.com/go-faster/gooners/issues/71.
func (s *Store) Open(ctx context.Context, id string) (io.ReadSeekCloser, blob.Blob, error) {
	key, err := s.parse(id)
	if err != nil {
		return nil, blob.Blob{}, err
	}

	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, blob.Blob{}, errors.Wrapf(blob.ErrNotFound, "id %q: %s", id, err)
	}
	// GetObject is lazy, so a missing object surfaces on the first read rather
	// than here; statting now turns that into an error the caller can act on
	// and yields the metadata either way.
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, blob.Blob{}, errors.Wrapf(blob.ErrNotFound, "id %q: %s", id, err)
	}

	b, err := s.describe(ctx, id, metaName(info, id), metaType(info), info.Size)
	if err != nil {
		_ = obj.Close()
		return nil, blob.Blob{}, err
	}
	return obj, b, nil
}

// Delete removes an object. Deleting an unknown id is not an error, and an id
// this store could not have minted is refused rather than silently ignored.
func (s *Store) Delete(ctx context.Context, id string) error {
	key, err := s.parse(id)
	if err != nil {
		return err
	}
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return errors.Wrapf(err, "remove object %q", id)
	}
	return nil
}

// describe builds the reference handed to the agent, minting the URL it will
// fetch with.
func (s *Store) describe(ctx context.Context, id, name, mimeType string, size int64) (blob.Blob, error) {
	key, err := s.parse(id)
	if err != nil {
		return blob.Blob{}, err
	}
	u, err := s.client.PresignedGetObject(ctx, s.bucket, key, s.urlTTL, url.Values{})
	if err != nil {
		return blob.Blob{}, errors.Wrapf(err, "presign %q", id)
	}
	return blob.Blob{
		ID:        id,
		URL:       u.String(),
		Name:      name,
		MIMEType:  mimeType,
		Size:      size,
		ExpiresAt: s.now().Add(s.urlTTL),
	}, nil
}

// newID mints an id in this store's namespace.
func (s *Store) newID() (string, error) {
	raw, err := blobutil.NewID()
	if err != nil {
		return "", err
	}
	return s.namespace + "/" + raw, nil
}

// WithTenant returns a store over the same bucket under a different key prefix,
// sharing this one's client and its connection pool.
//
// It exists so the tenancy boundary can move from the process to the session
// without reworking the store: a caller that learns the tenant per session
// derives one of these, where calling [New] again would re-check the bucket over
// the network every time. See https://github.com/go-faster/gooners/issues/71.
//
// The prefix must come from the process's own notion of who the caller is, never
// from a tool argument — a caller that picks its own prefix has picked its own
// tenant.
func (s *Store) WithTenant(prefix string) *Store {
	scoped := *s
	scoped.prefix = cleanPrefix(prefix)
	return &scoped
}

// cleanPrefix reduces a configured prefix to a bare key root, so a leading or
// trailing slash and a "." are all the same prefix rather than three.
func cleanPrefix(prefix string) string {
	prefix = strings.Trim(path.Clean("/"+prefix), "/")
	if prefix == "." {
		return ""
	}
	return prefix
}

// key is the bucket key for an id that is already known to be well formed.
func (s *Store) key(id string) string { return path.Join(s.prefix, id) }

// parse validates an id and returns its key. It is the gate between a string
// the agent supplied and a key this store will act on.
func (s *Store) parse(id string) (string, error) {
	if _, _, err := ParseID(id); err != nil {
		return "", err
	}
	return s.key(id), nil
}

const (
	namespacePattern = `[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}`
	uuidPattern      = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`
)

var (
	validNamespace = regexp.MustCompile(`^` + namespacePattern + `$`)
	validID        = regexp.MustCompile(`^(` + namespacePattern + `)/(` + uuidPattern + `)$`)
)

// ParseID splits an id into the namespace that minted it and its UUID.
//
// It is deliberately strict, and it is the gate between a string the model
// supplied and a key this store will act on. Anything not of exactly this shape
// never becomes a key, which is what keeps a crafted id — "../", an absolute
// key, another tenant's prefix — from naming an object outside the configured
// prefix. Loosening it is a tenancy change, not a parsing change.
func ParseID(id string) (namespace, objectUUID string, err error) {
	m := validID.FindStringSubmatch(id)
	if m == nil {
		return "", "", errors.Wrapf(blob.ErrNotFound, "id %q is not a blob id", id)
	}
	return m[1], m[2], nil
}

// metaName recovers the file name an object was stored under. The key is
// random, so without the metadata there is nothing to fall back on but the id.
func metaName(info minio.ObjectInfo, id string) string {
	return blobutil.CleanName(info.UserMetadata[userMetaKey(nameMeta)], path.Base(id))
}

// metaType recovers the type the producing tool declared, which is not
// necessarily the one the object is served as.
func metaType(info minio.ObjectInfo) string {
	if t := info.UserMetadata[userMetaKey(typeMeta)]; t != "" {
		return t
	}
	return blobutil.ContentType(info.ContentType, "")
}

// userMetaKey is how minio-go presents a user metadata key it read back: the
// x-amz-meta- prefix is stripped and the rest is canonicalised as a header.
func userMetaKey(k string) string { return http.CanonicalHeaderKey(k) }

// parseEndpoint splits an endpoint into the host:port minio-go wants and
// whether to use TLS. A bare host is https, because an endpoint that silently
// fell back to plaintext would send the credentials in the clear.
func parseEndpoint(endpoint string) (host string, secure bool, err error) {
	if !strings.Contains(endpoint, "://") {
		return endpoint, true, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", false, errors.Wrapf(err, "parse Endpoint %q", endpoint)
	}
	switch u.Scheme {
	case "https":
		secure = true
	case "http":
		secure = false
	default:
		return "", false, errors.Errorf("Endpoint scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", false, errors.Errorf("Endpoint must name a host, got %q", endpoint)
	}
	return u.Host, secure, nil
}

func scheme(secure bool) string {
	if secure {
		return "https"
	}
	return "http"
}
