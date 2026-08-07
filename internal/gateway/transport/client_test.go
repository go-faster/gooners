package gatewaytransport

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A request that never gets a response must be released when cleanup runs.
// This is the http/sse equivalent of the stdio SIGKILL: without it a wedged
// upstream holds the request, its goroutine and its connection forever, since
// these clients deliberately carry no timeout.
func TestAbortableClient_ReleasesInflight(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(block)
		srv.Close()
	})

	cl, cleanup := newUpstreamClient(srv.Client(), nil, nil, nil)

	errCh := make(chan error, 1)
	go func() {
		resp, err := cl.Get(srv.URL)
		if err == nil {
			_ = resp.Body.Close()
		}
		errCh <- err
	}()

	// The request is parked in the handler; nothing else can free it.
	select {
	case err := <-errCh:
		t.Fatalf("request returned before cleanup: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, cleanup())
	select {
	case err := <-errCh:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("cleanup did not release the in-flight request")
	}
}

// Cancellation must be tied to the body being closed, not to RoundTrip
// returning: SSE and streamable HTTP keep reading long after the headers land,
// and canceling early would break them outright.
func TestAbortableClient_KeepsStreamingBodyAlive(t *testing.T) {
	second := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("first\n"))
		w.(http.Flusher).Flush()
		select {
		case <-second:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("second\n"))
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)

	cl, cleanup := newUpstreamClient(srv.Client(), nil, nil, nil)
	t.Cleanup(func() { _ = cleanup() })

	resp, err := cl.Get(srv.URL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	buf := make([]byte, len("first\n"))
	_, err = io.ReadFull(resp.Body, buf)
	require.NoError(t, err)
	require.Equal(t, "first\n", string(buf))

	// RoundTrip returned a while ago; the stream must still be usable.
	close(second)
	buf = make([]byte, len("second\n"))
	_, err = io.ReadFull(resp.Body, buf)
	require.NoError(t, err)
	require.Equal(t, "second\n", string(buf))
}

// Ordinary requests must be unaffected, and closing the body must not leave the
// per-request cancellation registered.
func TestAbortableClient_NormalRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	cl, cleanup := newUpstreamClient(srv.Client(), nil, nil, nil)
	t.Cleanup(func() { _ = cleanup() })

	resp, err := cl.Get(srv.URL)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, "ok", string(body))
	// Double close must not panic on the once-guarded release.
	_ = resp.Body.Close()
}
