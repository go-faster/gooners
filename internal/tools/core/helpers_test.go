package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/effect"
)

// spoolFile writes content to a temp file and opens it the way the pool hands
// a spool to a tool: as an [effect.File], never as a path.
func spoolFile(t *testing.T, content string) effect.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spool.out")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	f, err := effect.OS().Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestReadHead(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		maxLines int
		maxBytes int64
		want     string
	}{
		{"empty file", "", 10, 100, ""},
		{"no trailing newline", "hello\nworld", 10, 100, "hello\nworld"},
		{"exactly threshold lines", "l1\nl2\nl3\n", 3, 100, "l1\nl2\nl3\n"},
		{
			name: "maxLines hit before EOF", content: "l1\nl2\nl3\nl4\n", maxLines: 2, maxBytes: 100,
			want: "l1\nl2\n\n... [Output truncated due to size/line limit] ...",
		},
		{
			name: "maxBytes hit before EOF", content: "l1\nl2\nl3\n", maxLines: 10, maxBytes: 4,
			want: "l1\nl\n... [Output truncated due to size/line limit] ...",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := readHead(spoolFile(t, tt.content), tt.maxLines, tt.maxBytes)
			require.NoError(t, err)
			require.Equal(t, tt.want, res)
		})
	}
}

func TestReadTail(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		maxLines int
		maxBytes int64
		want     string
	}{
		{"empty file", "", 10, 100, ""},
		{"no trailing newline", "hello\nworld", 10, 100, "hello\nworld"},
		{
			name: "maxLines hit before EOF", content: "l1\nl2\nl3\nl4\n", maxLines: 2, maxBytes: 100,
			want: "... [Output truncated due to size/line limit] ...\nl3\nl4",
		},
		{
			name: "maxBytes hit before EOF", content: "l1\nl2\nl3\n", maxLines: 10, maxBytes: 4,
			want: "... [Output truncated due to size/line limit] ...\nl3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := readTail(spoolFile(t, tt.content), tt.maxLines, tt.maxBytes)
			require.NoError(t, err)
			require.Equal(t, tt.want, res)
		})
	}
}
