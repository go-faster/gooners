package tunnel

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
)

type slogioWriter struct {
	logger *slog.Logger
	level  slog.Level
	attrs  []slog.Attr

	buf        []byte
	skipNextLF bool
	mux        sync.Mutex // protects buf and skipNextLF
}

var _ io.Writer = (*slogioWriter)(nil)

func (w *slogioWriter) Write(p []byte) (n int, err error) {
	return slogioWrite(w, p), nil
}

func (w *slogioWriter) WriteString(s string) (n int, err error) {
	return slogioWrite(w, s), nil
}

const maxSlogioLineBytes = 4096

var truncatedSuffix = []byte("… [truncated]")

func slogioWrite[S ~string | ~[]byte](lw *slogioWriter, p S) int {
	n := len(p)
	if n == 0 {
		return 0
	}

	lw.mux.Lock()
	defer lw.mux.Unlock()

	b := []byte(p)

	for len(b) > 0 {
		if lw.skipNextLF {
			lw.skipNextLF = false
			if b[0] == '\n' {
				b = b[1:]
				if len(b) == 0 {
					break
				}
			}
		}

		idx := bytes.IndexAny(b, "\r\n")
		if idx == -1 {
			lw.appendBounded(b)
			break
		}

		lw.appendBounded(b[:idx])
		lw.log(string(lw.buf))
		lw.resetBuf()

		delim := b[idx]
		b = b[idx+1:]

		if delim == '\r' {
			lw.skipNextLF = true
		}
	}

	return n
}

func (w *slogioWriter) appendBounded(b []byte) {
	if len(b) == 0 {
		return
	}

	if len(w.buf)+len(b) <= maxSlogioLineBytes {
		w.buf = append(w.buf, b...)
		return
	}

	if len(w.buf) >= maxSlogioLineBytes {
		return
	}

	// Reserve room for the truncation marker.
	limit := max(maxSlogioLineBytes-len(truncatedSuffix), 0)
	w.buf = w.buf[:min(len(w.buf), limit)]

	remaining := limit - len(w.buf)
	if remaining > 0 {
		if remaining > len(b) {
			remaining = len(b)
		}
		w.buf = append(w.buf, b[:remaining]...)
	}
	w.buf = append(w.buf, truncatedSuffix...)
}

func (w *slogioWriter) resetBuf() {
	if cap(w.buf) > maxSlogioLineBytes {
		w.buf = make([]byte, 0, maxSlogioLineBytes)
		return
	}
	w.buf = w.buf[:0]
}

func (w *slogioWriter) log(line string) {
	w.logger.LogAttrs(context.Background(), w.level, line, w.attrs...)
}

func (w *slogioWriter) Close() error {
	w.mux.Lock()
	defer w.mux.Unlock()

	if len(w.buf) > 0 {
		w.log(string(w.buf))
	}
	w.resetBuf()
	return nil
}
