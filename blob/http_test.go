package blob_test

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/blob"
)

// clock is a test clock, so expiry can be tested without sleeping.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

// newStore returns a store rooted in a temp directory, its clock, and an HTTP
// server serving it. The base URL path is non-empty so every request also
// exercises prefix stripping.
func newStore(t *testing.T, opts blob.HTTPOptions) (*blob.HTTP, *clock, *httptest.Server) {
	t.Helper()

	c := &clock{now: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)}
	if opts.BaseURL == "" {
		opts.BaseURL = "http://blob.invalid/files"
	}
	if opts.FS == nil {
		opts.FS = blob.Dir(t.TempDir())
	}
	if opts.Now == nil {
		opts.Now = c.Now
	}
	s, err := blob.NewHTTP(opts)
	require.NoError(t, err)

	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, c, srv
}

// fetch requests b.URL against srv, which serves the store under the same path
// the URL advertises.
func fetch(t *testing.T, srv *httptest.Server, rawURL string, header http.Header) *http.Response {
	t.Helper()

	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+u.EscapedPath(), http.NoBody)
	require.NoError(t, err)
	maps.Copy(req.Header, header)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestHTTPRoundTrip(t *testing.T) {
	ctx := t.Context()
	s, _, srv := newStore(t, blob.HTTPOptions{})

	payload := strings.Repeat("gooners", 1000)
	b, err := s.Put(ctx, strings.NewReader(payload), blob.PutOptions{
		Name:     "report.csv",
		MIMEType: "text/csv",
		Size:     blob.SizeUnknown,
	})
	require.NoError(t, err)

	require.NotEmpty(t, b.ID)
	require.Equal(t, "report.csv", b.Name)
	require.Equal(t, "text/csv", b.MIMEType)
	require.Equal(t, int64(len(payload)), b.Size)
	require.Equal(t, "http://blob.invalid/files/"+b.ID+"/report.csv", b.URL)

	resp := fetch(t, srv, b.URL, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, payload, string(body))

	require.Equal(t, "text/csv", resp.Header.Get("Content-Type"))
	require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	require.Contains(t, resp.Header.Get("Content-Disposition"), "attachment")
	require.Contains(t, resp.Header.Get("Content-Disposition"), "report.csv")
}

// TestHTTPOpen: read-back through the interface, which is what a resources/read
// adapter would use, does not depend on the HTTP handler.
func TestHTTPOpen(t *testing.T) {
	ctx := t.Context()
	s, _, _ := newStore(t, blob.HTTPOptions{})

	stored, err := s.Put(ctx, strings.NewReader("payload"), blob.PutOptions{Name: "a.txt"})
	require.NoError(t, err)

	f, b, err := s.Open(ctx, stored.ID)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	require.Equal(t, stored.ID, b.ID)

	data, err := io.ReadAll(f)
	require.NoError(t, err)
	require.Equal(t, "payload", string(data))

	_, _, err = s.Open(ctx, "nosuchid")
	require.ErrorIs(t, err, blob.ErrNotFound)
}

// TestHTTPRange: resuming a large fetch is half the point of a URL, so Range
// must work.
func TestHTTPRange(t *testing.T) {
	ctx := t.Context()
	s, _, srv := newStore(t, blob.HTTPOptions{})

	b, err := s.Put(ctx, strings.NewReader("0123456789"), blob.PutOptions{Name: "d.bin"})
	require.NoError(t, err)

	resp := fetch(t, srv, b.URL, http.Header{"Range": []string{"bytes=2-5"}})
	require.Equal(t, http.StatusPartialContent, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "2345", string(body))
}

// TestHTTPServeTypeDowngrade: the bytes are whatever a tool was handed, served
// from the operator's origin, so a browser must never render them.
func TestHTTPServeTypeDowngrade(t *testing.T) {
	ctx := t.Context()
	s, _, srv := newStore(t, blob.HTTPOptions{})

	for _, declared := range []string{"text/html", "image/svg+xml", "application/xhtml+xml"} {
		t.Run(declared, func(t *testing.T) {
			b, err := s.Put(ctx, strings.NewReader("<script>alert(1)</script>"), blob.PutOptions{
				Name:     "x.html",
				MIMEType: declared,
			})
			require.NoError(t, err)
			// The declared type still reaches the agent, where it is useful.
			require.Equal(t, declared, b.MIMEType)

			resp := fetch(t, srv, b.URL, nil)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))
		})
	}
}

// TestHTTPExpiry: an expired object is gone from the index, from the disk and
// from the handler, and is indistinguishable from one that never existed.
func TestHTTPExpiry(t *testing.T) {
	ctx := t.Context()
	s, c, srv := newStore(t, blob.HTTPOptions{TTL: time.Minute})

	b, err := s.Put(ctx, strings.NewReader("secret"), blob.PutOptions{Name: "s.txt"})
	require.NoError(t, err)
	require.Equal(t, c.now.Add(time.Minute), b.ExpiresAt)

	require.Equal(t, http.StatusOK, fetch(t, srv, b.URL, nil).StatusCode)

	c.advance(time.Minute)
	require.Equal(t, http.StatusNotFound, fetch(t, srv, b.URL, nil).StatusCode)

	_, _, err = s.Open(ctx, b.ID)
	require.ErrorIs(t, err, blob.ErrNotFound)
}

// TestHTTPPerObjectTTL: a tool may shorten the lifetime of one object.
func TestHTTPPerObjectTTL(t *testing.T) {
	ctx := t.Context()
	s, c, srv := newStore(t, blob.HTTPOptions{TTL: time.Hour})

	b, err := s.Put(ctx, strings.NewReader("x"), blob.PutOptions{Name: "s.txt", TTL: time.Minute})
	require.NoError(t, err)

	c.advance(2 * time.Minute)
	require.Equal(t, http.StatusNotFound, fetch(t, srv, b.URL, nil).StatusCode)
}

func TestHTTPUnknownID(t *testing.T) {
	_, _, srv := newStore(t, blob.HTTPOptions{})

	for _, path := range []string{
		"http://blob.invalid/files/deadbeef/x.txt",
		"http://blob.invalid/files/",
		"http://blob.invalid/files",
	} {
		require.Equal(t, http.StatusNotFound, fetch(t, srv, path, nil).StatusCode, path)
	}
}

func TestHTTPMethodNotAllowed(t *testing.T) {
	ctx := t.Context()
	s, _, srv := newStore(t, blob.HTTPOptions{})

	b, err := s.Put(ctx, strings.NewReader("x"), blob.PutOptions{Name: "s.txt"})
	require.NoError(t, err)

	u, err := url.Parse(b.URL)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, srv.URL+u.EscapedPath(), http.NoBody)
	require.NoError(t, err)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	require.Equal(t, "GET, HEAD", resp.Header.Get("Allow"))
}

// TestHTTPMaxSize: both the declared and the undeclared oversize path are
// refused, and neither leaves a partial object behind.
func TestHTTPMaxSize(t *testing.T) {
	ctx := t.Context()

	t.Run("Declared", func(t *testing.T) {
		s, _, _ := newStore(t, blob.HTTPOptions{MaxSize: 8})
		_, err := s.Put(ctx, strings.NewReader("tiny"), blob.PutOptions{Name: "a", Size: 1 << 20})
		require.ErrorIs(t, err, blob.ErrTooLarge)
	})

	t.Run("Undeclared", func(t *testing.T) {
		dir := t.TempDir()
		s, _, srv := newStore(t, blob.HTTPOptions{MaxSize: 8, FS: blob.Dir(dir)})

		_, err := s.Put(ctx, strings.NewReader("123456789"), blob.PutOptions{
			Name: "a",
			Size: blob.SizeUnknown,
		})
		require.ErrorIs(t, err, blob.ErrTooLarge)

		// The failed Put is not reachable and left nothing indexed.
		require.Equal(t, http.StatusNotFound, fetch(t, srv, "http://blob.invalid/files/x/a", nil).StatusCode)
	})

	t.Run("AtLimit", func(t *testing.T) {
		s, _, _ := newStore(t, blob.HTTPOptions{MaxSize: 8})
		b, err := s.Put(ctx, strings.NewReader("12345678"), blob.PutOptions{Name: "a"})
		require.NoError(t, err)
		require.Equal(t, int64(8), b.Size)
	})
}

func TestHTTPDelete(t *testing.T) {
	ctx := t.Context()
	s, _, srv := newStore(t, blob.HTTPOptions{})

	b, err := s.Put(ctx, strings.NewReader("x"), blob.PutOptions{Name: "s.txt"})
	require.NoError(t, err)

	require.NoError(t, s.Delete(ctx, b.ID))
	require.Equal(t, http.StatusNotFound, fetch(t, srv, b.URL, nil).StatusCode)

	// Deleting an unknown id is not an error.
	require.NoError(t, s.Delete(ctx, "nosuchid"))
}

// TestHTTPRunPurges: canceling Run removes everything the store holds, so a
// shutdown does not leave objects on disk.
func TestHTTPRunPurges(t *testing.T) {
	s, _, srv := newStore(t, blob.HTTPOptions{})

	b, err := s.Put(t.Context(), strings.NewReader("x"), blob.PutOptions{Name: "s.txt"})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	cancel()
	<-done

	require.Equal(t, http.StatusNotFound, fetch(t, srv, b.URL, nil).StatusCode)
}

func TestHTTPNameHandling(t *testing.T) {
	ctx := t.Context()
	s, _, srv := newStore(t, blob.HTTPOptions{})

	for _, tt := range []struct {
		name string
		give string
		want string
		mime string
	}{
		{name: "Plain", give: "notes.txt", want: "notes.txt", mime: "text/plain; charset=utf-8"},
		{name: "StripsDirectory", give: "../../etc/passwd", want: "passwd", mime: "application/octet-stream"},
		{name: "StripsQuotes", give: `a"b.txt`, want: "ab.txt", mime: "text/plain; charset=utf-8"},
		{name: "StripsControl", give: "a\nb.txt", want: "ab.txt", mime: "text/plain; charset=utf-8"},
		{name: "Unicode", give: "отчёт.txt", want: "отчёт.txt", mime: "text/plain; charset=utf-8"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, err := s.Put(ctx, strings.NewReader("x"), blob.PutOptions{Name: tt.give})
			require.NoError(t, err)
			require.Equal(t, tt.want, b.Name)
			// The type is guessed from the cleaned name, not the raw one.
			require.Equal(t, tt.mime, b.MIMEType)

			// Whatever the name, the URL still resolves.
			require.Equal(t, http.StatusOK, fetch(t, srv, b.URL, nil).StatusCode)
		})
	}

	t.Run("Empty", func(t *testing.T) {
		b, err := s.Put(ctx, strings.NewReader("x"), blob.PutOptions{})
		require.NoError(t, err)
		require.Equal(t, b.ID+".bin", b.Name)
		require.Equal(t, "application/octet-stream", b.MIMEType)
	})
}

// TestHTTPIDsAreDistinct: ids must not collide, since a repeat would serve one
// caller's bytes to another. Unguessability is a second layer rather than the
// boundary — see "Reaching an object" in the package documentation — but the
// version nibble is asserted because a time-ordered UUID would make an id
// partly predictable and would leak when it was minted.
func TestHTTPIDsAreDistinct(t *testing.T) {
	ctx := t.Context()
	s, _, _ := newStore(t, blob.HTTPOptions{})

	seen := make(map[string]struct{})
	for range 64 {
		b, err := s.Put(ctx, strings.NewReader("x"), blob.PutOptions{Name: "s.txt"})
		require.NoError(t, err)
		require.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
			b.ID, "a random (version 4) UUID")
		require.NotContains(t, seen, b.ID)
		seen[b.ID] = struct{}{}
	}
}

func TestNewHTTPValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts blob.HTTPOptions
	}{
		{name: "NoBaseURL", opts: blob.HTTPOptions{FS: blob.Dir(t.TempDir())}},
		{name: "RelativeBaseURL", opts: blob.HTTPOptions{BaseURL: "/files", FS: blob.Dir(t.TempDir())}},
		{name: "NoHost", opts: blob.HTTPOptions{BaseURL: "http:///files", FS: blob.Dir(t.TempDir())}},
		{name: "WrongScheme", opts: blob.HTTPOptions{BaseURL: "ftp://h/files", FS: blob.Dir(t.TempDir())}},
		{name: "NoFS", opts: blob.HTTPOptions{BaseURL: "http://h/files"}},
		{name: "NegativeTTL", opts: blob.HTTPOptions{BaseURL: "http://h/f", FS: blob.Dir(t.TempDir()), TTL: -time.Second}},
		{name: "NegativeMaxSize", opts: blob.HTTPOptions{BaseURL: "http://h/f", FS: blob.Dir(t.TempDir()), MaxSize: -1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := blob.NewHTTP(tt.opts)
			require.Error(t, err)
		})
	}

	t.Run("TrailingSlash", func(t *testing.T) {
		s, err := blob.NewHTTP(blob.HTTPOptions{BaseURL: "http://h/files/", FS: blob.Dir(t.TempDir())})
		require.NoError(t, err)
		b, err := s.Put(t.Context(), strings.NewReader("x"), blob.PutOptions{Name: "a.txt"})
		require.NoError(t, err)
		require.Equal(t, "http://h/files/"+b.ID+"/a.txt", b.URL)
	})

	// A store mounted at the root of its own listener has no prefix to strip.
	t.Run("RootBaseURL", func(t *testing.T) {
		s, err := blob.NewHTTP(blob.HTTPOptions{BaseURL: "http://h", FS: blob.Dir(t.TempDir())})
		require.NoError(t, err)
		srv := httptest.NewServer(s)
		t.Cleanup(srv.Close)

		b, err := s.Put(t.Context(), strings.NewReader("x"), blob.PutOptions{Name: "a.txt"})
		require.NoError(t, err)
		require.Equal(t, "http://h/"+b.ID+"/a.txt", b.URL)
		require.Equal(t, http.StatusOK, fetch(t, srv, b.URL, nil).StatusCode)
	})
}

// TestNewHTTPClearsLeftovers: objects from a previous process are unreachable
// anyway, since the index is in memory; they must not stay on disk either.
func TestNewHTTPClearsLeftovers(t *testing.T) {
	dir := t.TempDir()
	opts := blob.HTTPOptions{BaseURL: "http://h/files", FS: blob.Dir(dir)}

	first, err := blob.NewHTTP(opts)
	require.NoError(t, err)
	b, err := first.Put(t.Context(), strings.NewReader("x"), blob.PutOptions{Name: "a.txt"})
	require.NoError(t, err)

	second, err := blob.NewHTTP(blob.HTTPOptions{BaseURL: opts.BaseURL, FS: blob.Dir(dir)})
	require.NoError(t, err)

	_, _, err = second.Open(t.Context(), b.ID)
	require.ErrorIs(t, err, blob.ErrNotFound)

	// The bytes are gone from the filesystem, not merely from the index.
	_, err = blob.Dir(dir).Stat("objects/" + b.ID)
	require.Error(t, err)
}

func TestDeny(t *testing.T) {
	ctx := t.Context()
	s := blob.Deny("pass -blob-base-url to enable it")

	_, err := s.Put(ctx, strings.NewReader("x"), blob.PutOptions{Name: "a"})
	require.ErrorIs(t, err, blob.ErrDenied)
	require.Contains(t, err.Error(), "-blob-base-url")

	_, _, err = s.Open(ctx, "id")
	require.ErrorIs(t, err, blob.ErrDenied)
	require.ErrorIs(t, s.Delete(ctx, "id"), blob.ErrDenied)
}

// TestHTTPContextCancelled: a canceled call stores nothing.
func TestHTTPContextCancelled(t *testing.T) {
	s, _, _ := newStore(t, blob.HTTPOptions{})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := s.Put(ctx, strings.NewReader("x"), blob.PutOptions{Name: "a"})
	require.ErrorIs(t, err, context.Canceled)

	_, _, err = s.Open(ctx, "id")
	require.True(t, errors.Is(err, context.Canceled))
}
