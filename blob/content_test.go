package blob_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/blob"
)

// pngHeader is enough to be a plausible image; nothing here decodes it.
var pngHeader = []byte("\x89PNG\r\n\x1a\n")

func TestContentInline(t *testing.T) {
	ctx := t.Context()
	s, _, _ := newStore(t, blob.HTTPOptions{})

	t.Run("Text", func(t *testing.T) {
		content, b, err := blob.Content(ctx, s, strings.NewReader("hello"), blob.ContentOptions{
			PutOptions: blob.PutOptions{Name: "notes.txt"},
		})
		require.NoError(t, err)
		require.Zero(t, b, "an inlined payload is not stored")

		require.Len(t, content, 1)
		text, ok := content[0].(*mcp.TextContent)
		require.True(t, ok, "got %T", content[0])
		require.Equal(t, "hello", text.Text)
	})

	t.Run("JSON", func(t *testing.T) {
		content, _, err := blob.Content(ctx, s, strings.NewReader(`{"a":1}`), blob.ContentOptions{
			PutOptions: blob.PutOptions{Name: "x", MIMEType: "application/json"},
		})
		require.NoError(t, err)
		_, ok := content[0].(*mcp.TextContent)
		require.True(t, ok, "got %T", content[0])
	})

	t.Run("Image", func(t *testing.T) {
		content, b, err := blob.Content(ctx, s, bytes.NewReader(pngHeader), blob.ContentOptions{
			PutOptions: blob.PutOptions{Name: "panel.png"},
		})
		require.NoError(t, err)
		require.Zero(t, b)

		img, ok := content[0].(*mcp.ImageContent)
		require.True(t, ok, "got %T", content[0])
		require.Equal(t, pngHeader, img.Data)
		require.Equal(t, "image/png", img.MIMEType)
	})

	// Text that is not valid UTF-8 cannot be a TextContent, so it is stored
	// rather than corrupted on the way into the transcript.
	t.Run("InvalidUTF8", func(t *testing.T) {
		content, b, err := blob.Content(ctx, s, bytes.NewReader([]byte{0xff, 0xfe, 0xfd}), blob.ContentOptions{
			PutOptions: blob.PutOptions{Name: "broken.txt"},
		})
		require.NoError(t, err)
		require.NotZero(t, b)
		_, ok := content[0].(*mcp.ResourceLink)
		require.True(t, ok, "got %T", content[0])
	})

	// SVG is markup, not a raster image: it goes inline as text, so a client
	// reads it rather than rendering it.
	t.Run("SVG", func(t *testing.T) {
		content, _, err := blob.Content(ctx, s, strings.NewReader("<svg/>"), blob.ContentOptions{
			PutOptions: blob.PutOptions{Name: "x.svg"},
		})
		require.NoError(t, err)
		_, ok := content[0].(*mcp.TextContent)
		require.True(t, ok, "got %T", content[0])
	})
}

func TestContentLink(t *testing.T) {
	ctx := t.Context()
	s, _, srv := newStore(t, blob.HTTPOptions{})

	payload := strings.Repeat("x", 1000)
	content, b, err := blob.Content(ctx, s, strings.NewReader(payload), blob.ContentOptions{
		PutOptions:  blob.PutOptions{Name: "big.txt"},
		InlineLimit: 10,
	})
	require.NoError(t, err)

	require.Len(t, content, 1)
	link, ok := content[0].(*mcp.ResourceLink)
	require.True(t, ok, "got %T", content[0])
	require.Equal(t, b.URL, link.URI)
	require.Equal(t, "big.txt", link.Name)
	require.Equal(t, "text/plain; charset=utf-8", link.MIMEType)
	require.NotNil(t, link.Size)
	require.Equal(t, int64(len(payload)), *link.Size)

	// The head read for the size decision is not lost: the stored object is
	// the whole payload.
	resp := fetch(t, srv, b.URL, nil)
	body := make([]byte, len(payload)+1)
	n, _ := resp.Body.Read(body)
	require.Equal(t, payload[:n], string(body[:n]))
	require.Equal(t, int64(len(payload)), b.Size)
}

// TestContentBinaryAlwaysStored: a binary payload has nowhere to go inline, so
// size does not enter into it.
func TestContentBinaryAlwaysStored(t *testing.T) {
	s, _, _ := newStore(t, blob.HTTPOptions{})

	content, b, err := blob.Content(t.Context(), s, bytes.NewReader([]byte{0, 1, 2}), blob.ContentOptions{
		PutOptions: blob.PutOptions{Name: "a.bin"},
	})
	require.NoError(t, err)
	require.NotZero(t, b)
	require.Equal(t, int64(3), b.Size)
	_, ok := content[0].(*mcp.ResourceLink)
	require.True(t, ok, "got %T", content[0])
}

func TestContentInlineLimit(t *testing.T) {
	ctx := t.Context()
	s, _, _ := newStore(t, blob.HTTPOptions{})

	for _, tt := range []struct {
		name       string
		limit      int64
		size       int
		wantInline bool
	}{
		{name: "AtLimit", limit: 4, size: 4, wantInline: true},
		{name: "OverLimit", limit: 4, size: 5},
		{name: "Negative", limit: -1, size: 1},
		{name: "DefaultInlines", limit: 0, size: blob.DefaultInlineLimit, wantInline: true},
		{name: "DefaultStores", limit: 0, size: blob.DefaultInlineLimit + 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			content, b, err := blob.Content(ctx, s, strings.NewReader(strings.Repeat("x", tt.size)), blob.ContentOptions{
				PutOptions:  blob.PutOptions{Name: "a.txt"},
				InlineLimit: tt.limit,
			})
			require.NoError(t, err)

			if tt.wantInline {
				require.Zero(t, b)
				_, ok := content[0].(*mcp.TextContent)
				require.True(t, ok, "got %T", content[0])
				return
			}
			require.NotZero(t, b)
			_, ok := content[0].(*mcp.ResourceLink)
			require.True(t, ok, "got %T", content[0])
		})
	}
}

// TestContentDeniedStore: a payload too large to inline, with no store to put
// it in, is an error naming the missing configuration rather than a truncated
// or invented result.
func TestContentDeniedStore(t *testing.T) {
	ctx := t.Context()
	s := blob.Deny("pass -blob-base-url to enable it")

	_, _, err := blob.Content(ctx, s, bytes.NewReader([]byte{0, 1, 2}), blob.ContentOptions{
		PutOptions: blob.PutOptions{Name: "a.bin"},
	})
	require.ErrorIs(t, err, blob.ErrDenied)

	// Small text still works: it never needed the store.
	content, _, err := blob.Content(ctx, s, strings.NewReader("hi"), blob.ContentOptions{
		PutOptions: blob.PutOptions{Name: "a.txt"},
	})
	require.NoError(t, err)
	_, ok := content[0].(*mcp.TextContent)
	require.True(t, ok, "got %T", content[0])
}
