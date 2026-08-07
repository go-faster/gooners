package cmdutil

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/blob"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestBlobFlagsRegister(t *testing.T) {
	var flags BlobFlags
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.Register(fs)

	require.NoError(t, fs.Parse([]string{"-blob-addr", ":9000", "-blob-ttl", "1m"}))
	require.Equal(t, ":9000", flags.Addr)
	require.Equal(t, time.Minute, flags.TTL)
	require.Equal(t, blob.DefaultTTL, mustDefault(t, fs, "blob-ttl"))
}

func mustDefault(t *testing.T, fs *flag.FlagSet, name string) time.Duration {
	t.Helper()

	f := fs.Lookup(name)
	require.NotNil(t, f)
	d, err := time.ParseDuration(f.DefValue)
	require.NoError(t, err)
	return d
}

// TestBlobFlagsSetupDisabled: unconfigured means a store that refuses by
// naming the flag, not one that mints URLs nothing serves.
func TestBlobFlagsSetupDisabled(t *testing.T) {
	store, run, err := BlobFlags{}.Setup(BlobOptions{Name: "test-mcp", Logger: testLogger()})
	require.NoError(t, err)
	require.NoError(t, run(t.Context()))

	_, err = store.Put(t.Context(), strings.NewReader("x"), blob.PutOptions{Name: "a"})
	require.ErrorIs(t, err, blob.ErrDenied)
	require.ErrorContains(t, err, "-blob-addr")
	require.ErrorContains(t, err, "test-mcp")
}

func TestBlobFlagsSetupValidation(t *testing.T) {
	for _, tt := range []struct {
		name  string
		flags BlobFlags
		opts  BlobOptions
	}{
		{
			name:  "BaseURLWithoutAddr",
			flags: BlobFlags{BaseURL: "https://example.com/blob"},
			opts:  BlobOptions{Name: "test-mcp"},
		},
		{
			// Deriving a URL from a wildcard bind names a host the server
			// merely listens on, which is the wrong answer the package exists
			// to prevent.
			name:  "WildcardAddrWithoutBaseURL",
			flags: BlobFlags{Addr: "0.0.0.0:9000"},
			opts:  BlobOptions{Name: "test-mcp"},
		},
		{
			name:  "MalformedAddr",
			flags: BlobFlags{Addr: "not-an-address"},
			opts:  BlobOptions{Name: "test-mcp"},
		},
		{
			name:  "NoName",
			flags: BlobFlags{Addr: ":9000"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := tt.flags.Setup(tt.opts)
			require.Error(t, err)
		})
	}
}

func TestBlobFlagsDerivedBaseURL(t *testing.T) {
	for _, addr := range []string{":9000", "localhost:9000", "127.0.0.1:9000"} {
		t.Run(addr, func(t *testing.T) {
			store, _, err := BlobFlags{Addr: addr, Dir: t.TempDir()}.Setup(BlobOptions{Name: "test-mcp", Logger: testLogger()})
			require.NoError(t, err)

			b, err := store.Put(t.Context(), strings.NewReader("x"), blob.PutOptions{Name: "a.txt"})
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(b.URL, "http://localhost:9000/"), "got %s", b.URL)
		})
	}
}

// TestBlobFlagsServes: the store is reachable at the address it advertises,
// and shutting it down purges what it held.
func TestBlobFlagsServes(t *testing.T) {
	// Port zero would make the advertised URL wrong, so take a free port and
	// hand the same one to the flags.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	store, run, err := BlobFlags{Addr: addr, Dir: t.TempDir()}.Setup(BlobOptions{Name: "test-mcp", Logger: testLogger()})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- run(ctx) }()

	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)

	b, err := store.Put(ctx, strings.NewReader("served"), blob.PutOptions{Name: "a.txt"})
	require.NoError(t, err)
	require.Equal(t, "http://localhost:"+port+"/"+b.ID+"/a.txt", b.URL)

	require.Eventually(t, func() bool {
		resp, err := fetchURL(ctx, b.URL)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		return err == nil && resp.StatusCode == http.StatusOK && string(body) == "served"
	}, 5*time.Second, 10*time.Millisecond)

	cancel()
	require.NoError(t, <-errc)

	// Run's shutdown purged the object, so nothing is left on disk.
	_, _, err = store.Open(t.Context(), b.ID)
	require.ErrorIs(t, err, blob.ErrNotFound)
}

func fetchURL(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}
