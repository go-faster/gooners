package tunnel

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// testLogHandler is a simple slog.Handler that captures records.
type testLogHandler struct {
	records []slog.Record
}

func (h *testLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *testLogHandler) Handle(ctx context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}
func (h *testLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *testLogHandler) WithGroup(name string) slog.Handler       { return h }

func TestSlogioWriter(t *testing.T) {
	t.Run("Lines", func(t *testing.T) {
		h := &testLogHandler{}
		logger := slog.New(h)
		w := &slogioWriter{
			logger: logger,
			level:  slog.LevelInfo,
			attrs:  []slog.Attr{slog.String("test", "test")},
		}

		n, err := w.WriteString("hello\nworld\r\nfoo")
		require.NoError(t, err)
		require.Equal(t, 16, n)

		// Should have logged "hello" and "world", "foo" is in buf
		require.Len(t, h.records, 2)
		require.Equal(t, "hello", h.records[0].Message)
		require.Equal(t, "world", h.records[1].Message)

		// Close flushes the remainder
		err = w.Close()
		require.NoError(t, err)

		require.Len(t, h.records, 3)
		require.Equal(t, "foo", h.records[2].Message)
	})

	t.Run("Truncate", func(t *testing.T) {
		h := &testLogHandler{}
		logger := slog.New(h)
		w := &slogioWriter{
			logger: logger,
			level:  slog.LevelInfo,
		}

		longStr := strings.Repeat("a", maxSlogioLineBytes+100)
		n, err := w.WriteString(longStr + "\n")
		require.NoError(t, err)
		require.Equal(t, len(longStr)+1, n)

		require.Len(t, h.records, 1)
		msg := h.records[0].Message
		require.Equal(t, maxSlogioLineBytes, len(msg))
		require.True(t, strings.HasSuffix(msg, string(truncatedSuffix)))
	})

	t.Run("SkipNextLF", func(t *testing.T) {
		h := &testLogHandler{}
		logger := slog.New(h)
		w := &slogioWriter{
			logger: logger,
			level:  slog.LevelInfo,
		}

		// Write \r
		n, err := w.WriteString("bar\r")
		require.NoError(t, err)
		require.Equal(t, 4, n)

		require.Len(t, h.records, 1)
		require.Equal(t, "bar", h.records[0].Message)

		// Write \nfoo
		n, err = w.WriteString("\nfoo\n")
		require.NoError(t, err)
		require.Equal(t, 5, n)

		require.Len(t, h.records, 2)
		require.Equal(t, "foo", h.records[1].Message)

		// Write single \n
		w.skipNextLF = true
		n, err = w.WriteString("\n")
		require.NoError(t, err)
		require.Equal(t, 1, n)
		require.Len(t, h.records, 2) // unchanged

		// Write ANOTHER single \n
		// This should log an empty line, but if the bug exists, it will swallow it!
		n, err = w.WriteString("\n")
		require.NoError(t, err)
		require.Equal(t, 1, n)
		require.Len(t, h.records, 3) // Should be 3 now!
	})

	t.Run("WriteString", func(t *testing.T) {
		h := &testLogHandler{}
		logger := slog.New(h)
		w := &slogioWriter{
			logger: logger,
			level:  slog.LevelInfo,
		}

		n, err := w.WriteString("hello\n")
		require.NoError(t, err)
		require.Equal(t, 6, n)

		require.Len(t, h.records, 1)
		require.Equal(t, "hello", h.records[0].Message)
	})

	t.Run("EmptyWrite", func(t *testing.T) {
		h := &testLogHandler{}
		logger := slog.New(h)
		w := &slogioWriter{
			logger: logger,
			level:  slog.LevelInfo,
		}

		n, err := w.Write([]byte{})
		require.NoError(t, err)
		require.Equal(t, 0, n)
		require.Len(t, h.records, 0)

		n, err = w.WriteString("")
		require.NoError(t, err)
		require.Equal(t, 0, n)
		require.Len(t, h.records, 0)
	})

	t.Run("TruncationExactLimit", func(t *testing.T) {
		h := &testLogHandler{}
		logger := slog.New(h)
		w := &slogioWriter{
			logger: logger,
			level:  slog.LevelInfo,
		}

		longStr := strings.Repeat("b", maxSlogioLineBytes)
		n, err := w.WriteString(longStr + "\n")
		require.NoError(t, err)
		require.Equal(t, len(longStr)+1, n)

		require.Len(t, h.records, 1)
		msg := h.records[0].Message
		require.Equal(t, maxSlogioLineBytes, len(msg))
		require.False(t, strings.HasSuffix(msg, string(truncatedSuffix)))
	})

	t.Run("TruncationPartialWrites", func(t *testing.T) {
		h := &testLogHandler{}
		logger := slog.New(h)
		w := &slogioWriter{
			logger: logger,
			level:  slog.LevelInfo,
		}

		// Write half
		halfStr := strings.Repeat("c", maxSlogioLineBytes/2)
		w.WriteString(halfStr)

		// Write another chunk that exceeds the limit
		exceedingStr := strings.Repeat("d", maxSlogioLineBytes)
		w.WriteString(exceedingStr + "\n")

		require.Len(t, h.records, 1)
		msg := h.records[0].Message
		require.Equal(t, maxSlogioLineBytes, len(msg))
		require.True(t, strings.HasPrefix(msg, halfStr))
		require.True(t, strings.HasSuffix(msg, string(truncatedSuffix)))
	})
}

func BenchmarkSlogioWriter(b *testing.B) {
	logger := slog.New(&testLogHandler{})
	w := &slogioWriter{
		logger: logger,
		level:  slog.LevelInfo,
	}

	line := []byte("hello world this is a typical log line that might come from a subprocess\n")

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = w.Write(line)
	}
}
