package blob_test

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/blob"
)

func TestBlobResult(t *testing.T) {
	expires := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	got := blob.Blob{
		ID:        "tgmcp/0192f4a0-1b2c-4d3e-8f90-a1b2c3d4e5f6",
		URL:       "https://example.com/blob/abc/f.txt",
		Name:      "f.txt",
		MIMEType:  "text/plain",
		Size:      12,
		ExpiresAt: expires,
	}.Result()

	require.Equal(t, "tgmcp/0192f4a0-1b2c-4d3e-8f90-a1b2c3d4e5f6", got.Blob,
		"the id is what another tool consumes")
	require.Equal(t, "https://example.com/blob/abc/f.txt", got.URL, "the URL is what the agent fetches")
	require.Equal(t, "f.txt", got.Name)
	require.Equal(t, "text/plain", got.MIMEType)
	require.Equal(t, int64(12), got.Size)
	require.Equal(t, "2026-08-07T12:00:00Z", got.ExpiresAt)
}

func TestSourceOpen(t *testing.T) {
	ctx := t.Context()
	store, _, _ := newStore(t, blob.HTTPOptions{})

	b, err := store.Put(ctx, strings.NewReader("payload"), blob.PutOptions{Name: "f.txt"})
	require.NoError(t, err)

	t.Run("Found", func(t *testing.T) {
		r, got, err := blob.Source{Blob: b.ID}.Open(ctx, store)
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		data, err := io.ReadAll(r)
		require.NoError(t, err)
		require.Equal(t, "payload", string(data))
		require.Equal(t, b.ID, got.ID)
		require.Equal(t, "f.txt", got.Name)
	})

	t.Run("Unknown", func(t *testing.T) {
		_, _, err := blob.Source{Blob: "nope"}.Open(ctx, store)
		require.ErrorIs(t, err, blob.ErrNotFound)
	})

	// An empty argument is the agent not passing one, which has to be a legible
	// error rather than a lookup of "".
	t.Run("Empty", func(t *testing.T) {
		_, _, err := blob.Source{}.Open(ctx, store)
		require.Error(t, err)
		require.NotErrorIs(t, err, blob.ErrNotFound)
	})

	// A server started without a store refuses by naming that, so the operator
	// learns the flag rather than the agent learning the id was wrong.
	t.Run("NoStore", func(t *testing.T) {
		_, _, err := blob.Source{Blob: b.ID}.Open(ctx, nil)
		require.ErrorIs(t, err, blob.ErrDenied)
	})

	t.Run("DenyStore", func(t *testing.T) {
		_, _, err := blob.Source{Blob: b.ID}.Open(ctx, blob.Deny("no -blob-addr"))
		require.ErrorIs(t, err, blob.ErrDenied)
		require.ErrorContains(t, err, "no -blob-addr")
	})
}
