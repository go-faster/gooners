package s3_test

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/go-faster/gooners/blob"
	s3store "github.com/go-faster/gooners/blob/s3"
)

const (
	minioImage  = "minio/minio:RELEASE.2025-04-22T22-12-26Z"
	minioUser   = "minioadmin"
	minioSecret = "minioadmin"
	testBucket  = "blobs"
)

// minioEndpoint is the shared MinIO, started once for the package. One
// container is enough because every test gets its own tenant prefix, which is
// the same isolation the deployment relies on.
var minioEndpoint string

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}

	endpoint, stop, err := startMinIO(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "start minio: %v\n", err)
		os.Exit(1)
	}
	minioEndpoint = endpoint

	code := m.Run()
	stop()
	os.Exit(code)
}

// startMinIO brings up a MinIO and creates the bucket.
func startMinIO(ctx context.Context) (endpoint string, stop func(), err error) {
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: minioImage,
			Env: map[string]string{
				"MINIO_ROOT_USER":     minioUser,
				"MINIO_ROOT_PASSWORD": minioSecret,
			},
			Cmd:          []string{"server", "/data"},
			ExposedPorts: []string{"9000/tcp"},
			WaitingFor: wait.ForHTTP("/minio/health/live").
				WithPort("9000/tcp").
				WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		return "", nil, err
	}
	stop = func() { _ = testcontainers.TerminateContainer(container) }

	host, err := container.Host(ctx)
	if err != nil {
		stop()
		return "", nil, err
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		stop()
		return "", nil, err
	}
	addr := host + ":" + port.Port()

	admin, err := minio.New(addr, &minio.Options{
		Creds: credentials.NewStaticV4(minioUser, minioSecret, ""),
	})
	if err != nil {
		stop()
		return "", nil, err
	}
	if err := admin.MakeBucket(ctx, testBucket, minio.MakeBucketOptions{}); err != nil {
		stop()
		return "", nil, err
	}
	return "http://" + addr, stop, nil
}

// tenant is the key prefix for one test, so tests cannot see each other's
// objects for the same reason two users cannot.
func tenant(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("e2e test needs docker; skipped with -short")
	}
	return "tenants/" + t.Name()
}

// newStore builds a store as one server in a deployment: its own namespace,
// sharing a tenant prefix with the others.
func newStore(t *testing.T, namespace, prefix string, opts s3store.Options) *s3store.Store {
	t.Helper()

	opts.Endpoint = minioEndpoint
	opts.Bucket = testBucket
	opts.Namespace = namespace
	opts.Prefix = prefix
	opts.Credentials = credentials.NewStaticV4(minioUser, minioSecret, "")
	// The default transport is confined to the endpoint, which is what a real
	// deployment wants; here the endpoint is a mapped container port, so the
	// derived allowlist is that port and nothing else. Passing nil exercises it.

	store, err := s3store.New(t.Context(), opts)
	require.NoError(t, err)
	return store
}

func fetch(t *testing.T, url string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestPutAndFetch(t *testing.T) {
	prefix := tenant(t)
	s := newStore(t, "tgmcp", prefix, s3store.Options{})

	// .png rather than a media extension: Go's table has it built in, so the
	// guess does not depend on the runner's /etc/mime.types.
	b, err := s.Put(t.Context(), strings.NewReader("frame-bytes"), blob.PutOptions{Name: "frame.png"})
	require.NoError(t, err)
	require.Equal(t, "frame.png", b.Name)
	require.Equal(t, int64(len("frame-bytes")), b.Size)
	require.Equal(t, "image/png", b.MIMEType, "guessed from the name")
	require.True(t, strings.HasPrefix(b.ID, "tgmcp/"), "the id names the server that minted it")
	require.False(t, b.ExpiresAt.IsZero())

	// The agent's half: fetch the presigned URL with no credentials of its own.
	resp := fetch(t, b.URL)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "frame-bytes", string(body))
	require.Contains(t, resp.Header.Get("Content-Disposition"), "attachment")
}

// TestServedTypeIsDowngraded: these are bytes a tool was handed, served from the
// operator's origin. S3 cannot add nosniff, so the stored type and the
// disposition are what keep a browser from executing them.
func TestServedTypeIsDowngraded(t *testing.T) {
	prefix := tenant(t)
	s := newStore(t, "tgmcp", prefix, s3store.Options{})

	b, err := s.Put(t.Context(), strings.NewReader("<script>alert(1)</script>"),
		blob.PutOptions{Name: "page.html"})
	require.NoError(t, err)
	require.Equal(t, "text/html; charset=utf-8", b.MIMEType, "the agent still learns what it is")

	resp := fetch(t, b.URL)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"),
		"the wire type is downgraded")
	require.Contains(t, resp.Header.Get("Content-Disposition"), "attachment")
}

// TestOneServerReadsAnothersObject is the workflow the S3 store exists for:
// tgmcp stores a file, the agent passes the id to ssh-mcp, and ssh-mcp reads the
// bytes without fetching any URL.
func TestOneServerReadsAnothersObject(t *testing.T) {
	prefix := tenant(t)
	tg := newStore(t, "tgmcp", prefix, s3store.Options{})
	ssh := newStore(t, "ssh-mcp", prefix, s3store.Options{})

	b, err := tg.Put(t.Context(), strings.NewReader("kubeconfig-bytes"),
		blob.PutOptions{Name: "kubeconfig.yaml"})
	require.NoError(t, err)

	r, got, err := ssh.Open(t.Context(), b.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	data, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, "kubeconfig-bytes", string(data))
	require.Equal(t, "kubeconfig.yaml", got.Name, "the name survives the round trip")
	require.Equal(t, b.ID, got.ID)
	require.Equal(t, b.Size, got.Size)
}

// TestATenantCannotReadAnother is the boundary the deployment rests on: the
// prefix is the tenant, and no id crosses it.
func TestATenantCannotReadAnother(t *testing.T) {
	alicePrefix := tenant(t) + "/alice"
	alice := newStore(t, "tgmcp", alicePrefix, s3store.Options{})
	bob := newStore(t, "tgmcp", tenant(t)+"/bob", s3store.Options{})

	b, err := alice.Put(t.Context(), strings.NewReader("alice-secret"), blob.PutOptions{Name: "s.txt"})
	require.NoError(t, err)

	_, _, err = bob.Open(t.Context(), b.ID)
	require.Error(t, err, "a well-formed id from another tenant names nothing here")

	// The same store derived onto Alice's prefix does reach it, which is what
	// makes the boundary the prefix rather than the process.
	r, _, err := bob.WithTenant(alicePrefix).Open(t.Context(), b.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, "alice-secret", string(data))
}

// TestAttachUploads covers the uncontrolled-upstream case: a server colocated
// with an upstream sees its output directory and puts the file in the bucket,
// where a server on another machine can read it.
func TestAttachUploads(t *testing.T) {
	prefix := tenant(t)
	s := newStore(t, "gateway", prefix, s3store.Options{})

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "out.webm"), []byte("recorded"), 0o600))

	b, err := s.Attach(t.Context(), blob.Dir(dir), "out.webm", blob.PutOptions{})
	require.NoError(t, err)
	require.Equal(t, "out.webm", b.Name)
	require.Equal(t, int64(len("recorded")), b.Size)

	resp := fetch(t, b.URL)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "recorded", string(body))

	// The original is untouched: Attach copies, it does not move.
	require.FileExists(t, filepath.Join(dir, "out.webm"))
}

// TestAttachStaysInsideTheMount: the confinement is the filesystem provider's,
// not a check in this package.
func TestAttachStaysInsideTheMount(t *testing.T) {
	prefix := tenant(t)
	s := newStore(t, "gateway", prefix, s3store.Options{})

	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("nope"), 0o600))

	_, err := s.Attach(t.Context(), blob.Dir(t.TempDir()), "../"+filepath.Base(outside)+"/secret",
		blob.PutOptions{})
	require.Error(t, err)
}

func TestDelete(t *testing.T) {
	prefix := tenant(t)
	s := newStore(t, "tgmcp", prefix, s3store.Options{})

	b, err := s.Put(t.Context(), strings.NewReader("x"), blob.PutOptions{Name: "s.txt"})
	require.NoError(t, err)
	require.NoError(t, s.Delete(t.Context(), b.ID))

	_, _, err = s.Open(t.Context(), b.ID)
	require.Error(t, err)

	// Deleting what is not there is not an error; deleting a malformed id is.
	require.NoError(t, s.Delete(t.Context(), b.ID))
	require.Error(t, s.Delete(t.Context(), "../elsewhere"))
}

// TestMaxSize covers both halves: a declared oversize is refused before the
// transfer, and an undeclared one is caught during it and leaves nothing behind.
func TestMaxSize(t *testing.T) {
	prefix := tenant(t)
	s := newStore(t, "tgmcp", prefix, s3store.Options{MaxSize: 8})

	_, err := s.Put(t.Context(), strings.NewReader(strings.Repeat("x", 64)),
		blob.PutOptions{Name: "big.bin", Size: 64})
	require.ErrorIs(t, err, blob.ErrTooLarge, "a declared size is refused up front")

	_, err = s.Put(t.Context(), strings.NewReader(strings.Repeat("x", 64)),
		blob.PutOptions{Name: "big.bin", Size: blob.SizeUnknown})
	require.ErrorIs(t, err, blob.ErrTooLarge, "an undeclared one is caught in flight")

	b, err := s.Put(t.Context(), strings.NewReader("fits"), blob.PutOptions{Name: "small.bin"})
	require.NoError(t, err)
	require.Equal(t, int64(4), b.Size)
}

func TestOpenRejectsUnknownAndMalformedIDs(t *testing.T) {
	prefix := tenant(t)
	s := newStore(t, "tgmcp", prefix, s3store.Options{})

	for _, id := range []string{
		"tgmcp/0192f4a0-1b2c-4d3e-8f90-a1b2c3d4e5f6", // well formed, never minted
		"../../etc/passwd",
		"tgmcp/../../../other",
		"",
	} {
		_, _, err := s.Open(t.Context(), id)
		require.ErrorIs(t, err, blob.ErrNotFound, "id %q", id)
	}
}

func TestNewRefusesAMissingBucket(t *testing.T) {
	tenant(t)

	_, err := s3store.New(t.Context(), s3store.Options{
		Endpoint:    minioEndpoint,
		Bucket:      "no-such-bucket",
		Namespace:   "tgmcp",
		Credentials: credentials.NewStaticV4(minioUser, minioSecret, ""),
	})
	require.Error(t, err, "a misconfigured bucket fails at startup, not on the first tool call")
}
