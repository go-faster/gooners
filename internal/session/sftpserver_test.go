package session

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/pkg/sftp"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// errConnKilled stands in for a peer that resets the connection.
var errConnKilled = errors.New("connection killed")

// controlConn wraps a live TCP connection so a test can stall or kill it.
//
// Stalling is the interesting one: it models a link that goes quiet without a FIN or an
// RST — the slow-network failure this package has to survive. Reads and writes block
// forever and nothing ever errors, so nothing downstream notices on its own.
type controlConn struct {
	net.Conn

	mu     sync.Mutex
	stall  chan struct{} // non-nil while stalled; closed on kill
	killed bool
	read   int64
	base   int64 // read count at the last mark
	onRead func(c *controlConn, sinceMark int64)

	// hold, while non-nil, withholds everything the server sends.
	// See [sftpTestServer.holdResponsesBetween].
	hold     chan struct{}
	holdFrom int64
	holdTo   int64
	holdDone bool
}

// mark rebases the read counter, so a test can trigger on payload bytes rather than on
// whatever the SSH and SFTP handshakes happen to cost.
func (c *controlConn) mark() {
	c.mu.Lock()
	c.base = c.read
	c.mu.Unlock()
}

// gate blocks while the connection is stalled and fails once it is killed.
func (c *controlConn) gate() error {
	for {
		c.mu.Lock()
		killed, stall := c.killed, c.stall
		c.mu.Unlock()

		switch {
		case killed:
			return errConnKilled
		case stall == nil:
			return nil
		default:
			<-stall
		}
	}
}

func (c *controlConn) Read(b []byte) (int, error) {
	if err := c.gate(); err != nil {
		return 0, err
	}
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.mu.Lock()
		c.read += int64(n)
		since, hook := c.read-c.base, c.onRead
		if c.holdTo > 0 && !c.holdDone {
			switch {
			case since >= c.holdTo:
				if c.hold != nil {
					close(c.hold)
					c.hold = nil
				}
				c.holdDone = true
			case since >= c.holdFrom && c.hold == nil:
				c.hold = make(chan struct{})
			}
		}
		c.mu.Unlock()
		if hook != nil {
			hook(c, since)
		}
	}
	return n, err
}

func (c *controlConn) Write(b []byte) (int, error) {
	if err := c.gate(); err != nil {
		return 0, err
	}
	c.mu.Lock()
	hold := c.hold
	c.mu.Unlock()
	if hold != nil {
		<-hold
	}
	return c.Conn.Write(b)
}

// Stall makes every read and write from here on block indefinitely.
func (c *controlConn) Stall() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stall == nil && !c.killed {
		c.stall = make(chan struct{})
	}
}

// Kill drops the connection, as a reset would.
func (c *controlConn) Kill() {
	c.mu.Lock()
	if c.killed {
		c.mu.Unlock()
		return
	}
	c.killed = true
	if c.stall != nil {
		close(c.stall)
		c.stall = nil
	}
	c.mu.Unlock()
	_ = c.Conn.Close()
}

// sftpTestServer is an in-process SSH server that serves the SFTP subsystem from a real
// directory and hands the test control over the accepted connection.
type sftpTestServer struct {
	addr string
	// root is the directory transfers land in. Remote paths are absolute.
	root string

	mu       sync.Mutex
	conns    []*controlConn
	onRead   func(c *controlConn, sinceMark int64)
	holdFrom int64
	holdTo   int64
}

// holdResponsesBetween makes the server withhold everything it sends — every write
// acknowledgement included — from the moment it has read `from` payload bytes until it has
// read `to`.
//
// A client that waits for each write to be acknowledged before issuing the next one never
// gets past its first request, so it wedges and the session dies on the keepalive. A
// client that pipelines its writes sails straight through. That makes this a check on
// pipelining that cannot be faked by merely being fast, and needs no timing threshold.
//
// `from` must land inside the first write request: the SFTP handshake and file open have
// to be answered or the transfer never starts at all. `to` must land beyond one write
// request but within what the SSH channel window lets the client send unacknowledged.
func (s *sftpTestServer) holdResponsesBetween(from, to int64) {
	s.mu.Lock()
	s.holdFrom, s.holdTo = from, to
	s.mu.Unlock()
}

func newSFTPTestServer(t *testing.T) *sftpTestServer {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)

	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	// The pool only keepalives servers that identify as OpenSSH.
	cfg.ServerVersion = "SSH-2.0-OpenSSH_9.6"

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	s := &sftpTestServer{addr: ln.Addr().String(), root: t.TempDir()}
	// A stalled connection parks its server goroutines forever; killing them releases it.
	t.Cleanup(s.killAll)
	go s.serve(ln, cfg)
	return s
}

// afterBytes runs fn once the server has read n payload bytes — counted from the moment
// the SFTP subsystem starts, so n is transfer data rather than handshake. For a transfer
// in progress that means "partway through". It must be set before connecting.
func (s *sftpTestServer) afterBytes(n int64, fn func(c *controlConn)) {
	var once sync.Once
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onRead = func(c *controlConn, sinceMark int64) {
		if sinceMark >= n {
			once.Do(func() { fn(c) })
		}
	}
}

// killAll drops every connection the server has accepted.
func (s *sftpTestServer) killAll() {
	s.mu.Lock()
	conns := make([]*controlConn, len(s.conns))
	copy(conns, s.conns)
	s.mu.Unlock()
	for _, c := range conns {
		c.Kill()
	}
}

func (s *sftpTestServer) serve(ln net.Listener, cfg *ssh.ServerConfig) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		c := &controlConn{Conn: conn}
		s.mu.Lock()
		c.onRead, c.holdFrom, c.holdTo = s.onRead, s.holdFrom, s.holdTo
		s.conns = append(s.conns, c)
		s.mu.Unlock()
		go handleSFTPConn(c, cfg)
	}
}

func handleSFTPConn(conn *controlConn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sc.Close() }()
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		ch, chReqs, err := newChan.Accept()
		if err != nil {
			continue
		}
		go serveSFTPSubsystem(conn, ch, chReqs)
	}
}

func serveSFTPSubsystem(conn *controlConn, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer func() { _ = ch.Close() }()

	for req := range reqs {
		var payload struct{ Subsystem string }
		if req.Type != "subsystem" || ssh.Unmarshal(req.Payload, &payload) != nil || payload.Subsystem != "sftp" {
			_ = req.Reply(false, nil)
			continue
		}
		_ = req.Reply(true, nil)
		conn.mark() // everything from here on is SFTP traffic

		srv, err := sftp.NewServer(ch)
		if err != nil {
			return
		}
		_ = srv.Serve()
		_ = srv.Close()
		return
	}
}
