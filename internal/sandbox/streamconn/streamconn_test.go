package streamconn

import (
	"bytes"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"empty", ""},
		{"short", "hello"},
		{"multiline", "line one\nline two\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ar, aw := io.Pipe()
			br, bw := io.Pipe()
			t.Cleanup(func() {
				_ = ar.Close()
				_ = aw.Close()
				_ = br.Close()
				_ = bw.Close()
			})

			// a reads what b writes; b reads what a writes.
			a := New(ar, bw, Options{})
			b := New(br, aw, Options{})
			t.Cleanup(func() {
				_ = a.Close()
				_ = b.Close()
			})

			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = a.Write([]byte(tc.payload))
			}()

			buf := make([]byte, len(tc.payload)+1)
			if tc.payload == "" {
				// Nothing to read; just make sure the write completed.
				<-done
				return
			}
			n, err := readFull(b, buf, len(tc.payload))
			require.NoError(t, err)
			require.Equal(t, tc.payload, string(buf[:n]))
			<-done
		})
	}
}

// readFull reads exactly want bytes (or fewer on error) using repeated Read calls.
func readFull(r io.Reader, buf []byte, want int) (int, error) {
	total := 0
	for total < want {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func TestClose_UnblocksBlockedRead(t *testing.T) {
	pr, pw := io.Pipe()
	c := New(pr, pw, Options{
		Close: func() error {
			return pr.Close()
		},
	})
	t.Cleanup(func() { _ = pw.Close() })

	// Nothing ever writes to pr, so the pump (and thus Read, which has
	// nothing buffered) is guaranteed to block until Close fires - whether
	// Close happens before or after the goroutine below reaches Read.
	errCh := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 16))
		errCh <- err
	}()

	require.NoError(t, c.Close())

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, net.ErrClosed)
	case <-time.After(time.Second):
		t.Fatal("Read did not unblock after Close")
	}
}

func TestReadDeadline_Exceeded(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})
	c := New(pr, pw, Options{})
	t.Cleanup(func() { _ = c.Close() })

	require.NoError(t, c.SetReadDeadline(time.Now().Add(-time.Second)))

	_, err := c.Read(make([]byte, 16))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
}

func TestWriteDeadline_Exceeded(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})
	c := New(pr, pw, Options{})
	t.Cleanup(func() { _ = c.Close() })

	require.NoError(t, c.SetWriteDeadline(time.Now().Add(-time.Second)))

	_, err := c.Write([]byte("hi"))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
}

func TestClose_Idempotent(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})

	var calls int
	var mu sync.Mutex
	c := New(pr, pw, Options{
		Close: func() error {
			mu.Lock()
			calls++
			mu.Unlock()
			return nil
		},
	})

	require.NoError(t, c.Close())
	require.NoError(t, c.Close())
	require.NoError(t, c.Close())

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, calls)
}

func TestRead_ReaderEOFSurfaces(t *testing.T) {
	r := strings.NewReader("")
	_, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	c := New(r, pw, Options{})
	t.Cleanup(func() { _ = c.Close() })

	_, err := c.Read(make([]byte, 16))
	require.ErrorIs(t, err, io.EOF)
}

type fakeCloseWriter struct {
	io.Writer
	closed bool
	err    error
}

func (f *fakeCloseWriter) CloseWrite() error {
	f.closed = true
	return f.err
}

func TestCloseWrite_Propagates(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})

	fw := &fakeCloseWriter{Writer: pw}
	c := New(pr, fw, Options{})
	t.Cleanup(func() { _ = c.Close() })

	cw, ok := c.(interface{ CloseWrite() error })
	require.True(t, ok, "Conn should implement CloseWrite when the writer does")
	require.NoError(t, cw.CloseWrite())
	require.True(t, fw.closed)
}

func TestNew_NoCloseWriteWhenWriterLacksIt(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})

	c := New(pr, pw, Options{})
	t.Cleanup(func() { _ = c.Close() })

	_, ok := c.(interface{ CloseWrite() error })
	require.False(t, ok, "Conn should not implement CloseWrite when the writer doesn't")
}

func TestWrite_AfterClose(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})
	c := New(pr, pw, Options{})
	require.NoError(t, c.Close())

	_, err := c.Write([]byte("x"))
	require.ErrorIs(t, err, net.ErrClosed)
}

// TestRead_OneByteReader proves the read pump correctly reassembles data
// that its source delivers one byte at a time, as a real exec/network
// stream can fragment arbitrarily.
func TestRead_OneByteReader(t *testing.T) {
	const payload = "the quick brown fox jumps over the lazy dog"

	_, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	src := iotest.OneByteReader(strings.NewReader(payload))
	c := New(src, pw, Options{})
	t.Cleanup(func() { _ = c.Close() })

	buf := make([]byte, len(payload))
	n, err := readFull(c, buf, len(payload))
	require.NoError(t, err)
	require.Equal(t, payload, string(buf[:n]))
}

// TestRead_DataErrReader proves that when the source returns the final
// chunk of data together with io.EOF (rather than EOF on a separate,
// subsequent call), the adapter still delivers that final chunk instead of
// dropping it.
func TestRead_DataErrReader(t *testing.T) {
	const payload = "final chunk arrives with EOF attached"

	_, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	src := iotest.DataErrReader(strings.NewReader(payload))
	c := New(src, pw, Options{})
	t.Cleanup(func() { _ = c.Close() })

	buf := make([]byte, len(payload))
	n, err := readFull(c, buf, len(payload))
	require.NoError(t, err, "the final chunk must not be dropped")
	require.Equal(t, payload, string(buf[:n]))

	// The next Read must surface EOF, not hang or drop it.
	_, err = c.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF)
}

// TestRead_TimeoutReaderSurfacesError proves a source that returns a
// mid-stream error (iotest.ErrTimeout, on TimeoutReader's second Read call)
// is surfaced as a real error out of the adapter's Read, without panicking
// or being swallowed - after any data already buffered ahead of it is
// delivered.
func TestRead_TimeoutReaderSurfacesError(t *testing.T) {
	const payload = "some data before the timeout"

	_, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	src := iotest.TimeoutReader(strings.NewReader(payload))
	c := New(src, pw, Options{})
	t.Cleanup(func() { _ = c.Close() })

	buf := make([]byte, len(payload))
	n, err := readFull(c, buf, len(payload))
	require.NoError(t, err)
	require.Equal(t, payload, string(buf[:n]))

	_, err = c.Read(make([]byte, 1))
	require.ErrorIs(t, err, iotest.ErrTimeout)
}

// TestRead_ConformsToIotestTestReader runs the standard library's own
// io.Reader conformance checks (correct byte counts, correct EOF behavior,
// no over-reading, reads of varying sizes) against the net.Conn returned by
// New, wrapping a plain io.Pipe half whose write end is closed after the
// payload is written.
func TestRead_ConformsToIotestTestReader(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 256) // 4KB, enough for varied read sizes

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = pw.Write(payload)
		_ = pw.Close()
	}()
	t.Cleanup(func() { <-done })

	_, otherPw := io.Pipe()
	t.Cleanup(func() { _ = otherPw.Close() })

	c := New(pr, otherPw, Options{})
	t.Cleanup(func() { _ = c.Close() })

	require.NoError(t, iotest.TestReader(c, payload))
}

func TestAddrDefaults(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})
	c := New(pr, pw, Options{})
	t.Cleanup(func() { _ = c.Close() })

	require.NotEmpty(t, c.LocalAddr().String())
	require.NotEmpty(t, c.RemoteAddr().String())
}
