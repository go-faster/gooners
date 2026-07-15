// Package streamconn adapts a half-duplex stream pair (a reader from a
// remote process's stdout and a writer to its stdin) into a net.Conn.
//
// It takes a separate reader and writer rather than one io.ReadWriter because
// backends differ in how they hand back a process's stdio: Docker's exec
// attach gives one hijacked connection, but Kubernetes' SPDY exec streams
// arrive already demultiplexed as two independent halves. Keeping reader and
// writer separate lets both map onto the same adapter.
package streamconn

import (
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a [New] conn.
type Options struct {
	Local, Remote net.Addr
	// Close is called once, by Conn.Close, after the streams are shut down.
	Close func() error
	// ReadBuffer is the size of the internal read pump channel. Default 8.
	ReadBuffer int
}

func (o *Options) setDefaults() {
	if o.Local == nil {
		o.Local = addr("streamconn-local")
	}
	if o.Remote == nil {
		o.Remote = addr("streamconn-remote")
	}
	if o.Close == nil {
		o.Close = func() error { return nil }
	}
	if o.ReadBuffer <= 0 {
		o.ReadBuffer = 8
	}
}

type addr string

func (a addr) Network() string { return "streamconn" }
func (a addr) String() string  { return string(a) }

// closeWriter is satisfied by writers that support half-closing (e.g. a
// Docker hijacked connection's CloseWrite, or an *os.File-backed pipe).
type closeWriter interface {
	CloseWrite() error
}

// New returns a net.Conn that reads from r and writes to w. If w implements
// CloseWrite, so does the returned Conn.
func New(r io.Reader, w io.Writer, opts Options) net.Conn {
	opts.setDefaults()

	c := &conn{
		w:      w,
		opts:   opts,
		closed: make(chan struct{}),
		readCh: make(chan []byte, opts.ReadBuffer),
		rd:     newDeadline(),
		wd:     newDeadline(),
	}
	go c.pump(r)

	if cw, ok := w.(closeWriter); ok {
		return &closeWriteConn{conn: c, w: cw}
	}
	return c
}

type conn struct {
	w    io.Writer
	opts Options

	closeOnce sync.Once
	closed    chan struct{} // closed by Close
	closeErr  error

	readCh  chan []byte // chunks from the pump goroutine
	pumpErr error       // set once, before readCh is closed; safe via channel-close happens-before

	readMu   sync.Mutex
	leftover []byte

	writeMu     sync.Mutex
	closedWrite atomic.Bool

	rd, wd *deadline
}

// pump copies from r into readCh until r returns an error (including EOF) or
// the conn is closed.
func (c *conn) pump(r io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case c.readCh <- chunk:
			case <-c.closed:
				return
			}
		}
		if err != nil {
			c.pumpErr = err
			close(c.readCh)
			return
		}
	}
}

func (c *conn) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	if len(c.leftover) > 0 {
		n := copy(b, c.leftover)
		c.leftover = c.leftover[n:]
		return n, nil
	}

	select {
	case <-c.closed:
		return 0, net.ErrClosed
	case <-c.rd.wait():
		return 0, os.ErrDeadlineExceeded
	default:
	}

	select {
	case chunk, ok := <-c.readCh:
		if !ok {
			if c.pumpErr != nil {
				return 0, c.pumpErr
			}
			return 0, io.EOF
		}
		n := copy(b, chunk)
		if n < len(chunk) {
			c.leftover = chunk[n:]
		}
		return n, nil
	case <-c.closed:
		return 0, net.ErrClosed
	case <-c.rd.wait():
		return 0, os.ErrDeadlineExceeded
	}
}

func (c *conn) Write(b []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	select {
	case <-c.wd.wait():
		return 0, os.ErrDeadlineExceeded
	default:
	}
	if c.closedWrite.Load() {
		return 0, net.ErrClosed
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.w.Write(b)
}

func (c *conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.closeErr = c.opts.Close()
	})
	return c.closeErr
}

func (c *conn) LocalAddr() net.Addr  { return c.opts.Local }
func (c *conn) RemoteAddr() net.Addr { return c.opts.Remote }

func (c *conn) SetDeadline(t time.Time) error {
	c.rd.set(t)
	c.wd.set(t)
	return nil
}

func (c *conn) SetReadDeadline(t time.Time) error {
	c.rd.set(t)
	return nil
}

func (c *conn) SetWriteDeadline(t time.Time) error {
	c.wd.set(t)
	return nil
}

// closeWriteConn adds CloseWrite to conn, only ever constructed when the
// underlying writer supports it (see New).
type closeWriteConn struct {
	*conn
	w closeWriter
}

func (c *closeWriteConn) CloseWrite() error {
	c.closedWrite.Store(true)
	return c.w.CloseWrite()
}
