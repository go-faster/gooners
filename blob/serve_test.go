package blob_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/blob"
)

func TestMountPath(t *testing.T) {
	for _, tt := range []struct {
		baseURL string
		want    string
	}{
		{"http://h", "/"},
		{"http://h/", "/"},
		{"http://h/blob", "/blob/"},
		{"http://h/blob/", "/blob/"},
		{"https://mcp.example.com/files/v1", "/files/v1/"},
	} {
		s, err := blob.NewHTTP(blob.HTTPOptions{BaseURL: tt.baseURL, FS: blob.Dir(t.TempDir())})
		require.NoError(t, err)
		require.Equal(t, tt.want, s.MountPath(), tt.baseURL)
	}
}

// TestMountPathServesOnASharedMux is the property MountPath exists for: mounted
// where it says, the URLs it hands out resolve.
func TestMountPathServesOnASharedMux(t *testing.T) {
	ctx := t.Context()
	store, err := blob.NewHTTP(blob.HTTPOptions{
		BaseURL: "http://127.0.0.1:0/blob",
		FS:      blob.Dir(t.TempDir()),
	})
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.Handle(store.MountPath(), store)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	b, err := store.Put(ctx, strings.NewReader("payload"), blob.PutOptions{Name: "f.txt"})
	require.NoError(t, err)

	// The advertised URL differs from the test server only in authority, which
	// is the operator's business; the path is what MountPath has to get right.
	resp := get(t, srv.URL+strings.TrimPrefix(b.URL, "http://127.0.0.1:0"))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "payload", string(body))
}

func TestServe(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	store, err := blob.NewHTTP(blob.HTTPOptions{
		BaseURL: "http://127.0.0.1/blob",
		FS:      blob.Dir(t.TempDir()),
	})
	require.NoError(t, err)

	// Port 0 would leave the test with no way to reach the listener, so take a
	// concrete one and hand it straight back.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	done := make(chan error, 1)
	go func() { done <- store.Serve(ctx, blob.ServeOptions{Addr: addr}) }()

	b, err := store.Put(ctx, strings.NewReader("served"), blob.PutOptions{Name: "f.txt"})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return false
		}
		return conn.Close() == nil
	}, 5*time.Second, 10*time.Millisecond, "Serve should listen")

	resp := get(t, "http://"+addr+"/blob/"+b.ID+"/f.txt")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "served", string(body))

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err, "a canceled context is a clean shutdown")
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after its context was canceled")
	}
}

// TestServeReturnsOnABadAddress: a port already in use is a startup error the
// operator must see, not a goroutine that fails later.
func TestServeReturnsOnABadAddress(t *testing.T) {
	store, err := blob.NewHTTP(blob.HTTPOptions{
		BaseURL: "http://127.0.0.1/blob",
		FS:      blob.Dir(t.TempDir()),
	})
	require.NoError(t, err)

	held, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Close() })

	// The context stays live, so returning at all proves Serve does not wait on
	// it once the listener fails.
	require.Error(t, store.Serve(t.Context(), blob.ServeOptions{Addr: held.Addr().String()}))
	require.Error(t, store.Serve(t.Context(), blob.ServeOptions{}), "Addr is required")
}

// get fetches rawURL, closing the body when the test ends.
func get(t *testing.T, rawURL string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}
