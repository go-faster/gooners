package session

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// testServer is a minimal in-process SSH server for tests.
// It handles session (exec) and direct-tcpip (ProxyJump forwarding) channels.
type testServer struct {
	addr    string
	hostKey ssh.Signer
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)

	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	s := &testServer{addr: ln.Addr().String(), hostKey: signer}
	go s.serve(ln, cfg)
	return s
}

func newTestServerPassword(t *testing.T, pass string) *testServer {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if string(password) == pass {
				return nil, nil
			}
			return nil, fmt.Errorf("wrong password")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	s := &testServer{addr: ln.Addr().String(), hostKey: signer}
	go s.serve(ln, cfg)
	return s
}

func (s *testServer) serve(ln net.Listener, cfg *ssh.ServerConfig) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn, cfg)
	}
}

func (s *testServer) handleConn(conn net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sc.Close() }()
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		switch newChan.ChannelType() {
		case "session":
			ch, reqs, err := newChan.Accept()
			if err != nil {
				continue
			}
			go handleTestSession(ch, reqs)
		case "direct-tcpip":
			go handleDirectTCPIP(newChan)
		default:
			_ = newChan.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}
}

// handleTestSession responds to exec requests by echoing the command back as stdout,
// or simulating an SCP transfer if the command is "scp".
func handleTestSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer func() { _ = ch.Close() }()
	for req := range reqs {
		switch req.Type {
		case "exec":
			var payload struct{ Command string }
			if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)

			if strings.HasPrefix(payload.Command, "scp ") {
				handleTestSCP(ch, payload.Command)
				return
			}

			_, _ = fmt.Fprintln(ch, payload.Command)
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{0}))
			return
		default:
			_ = req.Reply(false, nil)
		}
	}
}

func handleTestSCP(ch ssh.Channel, cmd string) {
	if strings.Contains(cmd, "-t") {
		// Sink: Receive file
		ch.Write([]byte{0})
		buf := make([]byte, 1024)
		// Wait for header
		_, _ = ch.Read(buf)
		ch.Write([]byte{0})
		// Wait for content
		_, _ = ch.Read(buf)
		ch.Write([]byte{0})
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{0}))
	} else if strings.Contains(cmd, "-f") {
		// Source: Send file
		buf := make([]byte, 1)
		_, _ = ch.Read(buf)
		content := "hello scp"
		_, _ = fmt.Fprintf(ch, "C0644 %d test.txt\n", len(content))
		_, _ = ch.Read(buf)
		_, _ = ch.Write([]byte(content))
		_, _ = ch.Write([]byte{0})
		_, _ = ch.Read(buf)
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{0}))
	}
}

// directTCPIPMsg mirrors the wire format of the direct-tcpip channel open payload.
type directTCPIPMsg struct {
	DestAddr string
	DestPort uint32
	SrcAddr  string
	SrcPort  uint32
}

// handleDirectTCPIP dials the requested destination and bridges the channel to it.
func handleDirectTCPIP(newChan ssh.NewChannel) {
	var msg directTCPIPMsg
	if err := ssh.Unmarshal(newChan.ExtraData(), &msg); err != nil {
		_ = newChan.Reject(ssh.ConnectionFailed, "bad payload")
		return
	}

	dest := net.JoinHostPort(msg.DestAddr, strconv.Itoa(int(msg.DestPort)))
	targetConn, err := net.Dial("tcp", dest)
	if err != nil {
		_ = newChan.Reject(ssh.ConnectionFailed, err.Error())
		return
	}

	ch, reqs, err := newChan.Accept()
	if err != nil {
		_ = targetConn.Close()
		return
	}
	go ssh.DiscardRequests(reqs)

	go func() {
		defer func() { _ = ch.Close() }()
		defer func() { _ = targetConn.Close() }()
		_, _ = io.Copy(targetConn, ch)
	}()
	go func() {
		defer func() { _ = ch.Close() }()
		defer func() { _ = targetConn.Close() }()
		_, _ = io.Copy(ch, targetConn)
	}()
}
