package agent_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/go-faster/gooners/internal/sandbox/agent"
	"github.com/go-faster/gooners/internal/sandbox/streamconn"
)

// TestServe exercises the whole preamble + streamconn + agent stack with zero
// Docker: a real agent.Serve runs on one end of an in-memory pipe pair (both
// ends wrapped in streamconn, exactly like production - the agent side over
// its stdin/stdout, the manager side over what would be the container's
// hijacked exec stream), and a real ssh.NewClientConn drives the other end.
func TestServe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("agent.Serve shells out to /bin/bash or /bin/sh, unavailable on windows")
	}

	// --- key material, exactly as sandbox-mcp's Manager would generate it ---
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	hostPubKey, err := ssh.NewPublicKey(hostPriv.Public())
	require.NoError(t, err)
	hostPEMBlock, err := ssh.MarshalPrivateKey(hostPriv, "")
	require.NoError(t, err)
	hostKeyPEM := string(pem.EncodeToMemory(hostPEMBlock))

	_, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	clientSigner, err := ssh.NewSignerFromKey(clientPriv)
	require.NoError(t, err)
	authorizedKey := string(ssh.MarshalAuthorizedKey(clientSigner.PublicKey()))

	preamble := agent.Preamble{
		Version:       1,
		HostKeyPEM:    hostKeyPEM,
		AuthorizedKey: authorizedKey,
	}

	// --- wire up the half-duplex stdio stream pair ---
	// manager -> agent (agent's "stdin")
	agentIn, mgrOut := io.Pipe()
	// agent -> manager (agent's "stdout")
	mgrIn, agentOut := io.Pipe()

	mgrConn := streamconn.New(mgrIn, mgrOut, streamconn.Options{
		Close: func() error {
			_ = mgrIn.Close()
			return mgrOut.Close()
		},
	})

	agentServeErr := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		r := bufio.NewReader(agentIn)
		p, err := agent.ReadPreamble(r)
		if err != nil {
			agentServeErr <- err
			return
		}

		hostKey, err := ssh.ParsePrivateKey([]byte(p.HostKeyPEM))
		if err != nil {
			agentServeErr <- err
			return
		}
		authKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(p.AuthorizedKey))
		if err != nil {
			agentServeErr <- err
			return
		}

		// The SAME bufio.Reader is handed to streamconn: it may already hold
		// buffered handshake bytes read past the preamble's newline.
		agentConn := streamconn.New(r, agentOut, streamconn.Options{
			Close: func() error {
				_ = agentIn.Close()
				return agentOut.Close()
			},
		})

		agentServeErr <- agent.Serve(ctx, agentConn, agent.Config{
			HostKey:       hostKey,
			AuthorizedKey: authKey,
			Shell:         p.Shell,
			Workdir:       p.Workdir,
			Version:       "test",
			Logger:        slog.New(slog.DiscardHandler),
		})
	}()

	// Manager writes the preamble across its conn, then drives the SSH
	// handshake over that same conn - exactly as production does.
	require.NoError(t, agent.WritePreamble(mgrConn, preamble))

	clientConn, chans, reqs, err := ssh.NewClientConn(mgrConn, "sandbox", &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientSigner)},
		HostKeyCallback: ssh.FixedHostKey(hostPubKey),
		Timeout:         10 * time.Second,
	})
	require.NoError(t, err)
	sshClient := ssh.NewClient(clientConn, chans, reqs)

	t.Cleanup(func() {
		_ = sshClient.Close()
		select {
		case <-agentServeErr:
		case <-time.After(5 * time.Second):
			t.Log("agent.Serve did not return after the connection closed")
		}
	})

	t.Run("exec exits zero with stdout", func(t *testing.T) {
		sess, err := sshClient.NewSession()
		require.NoError(t, err)
		defer func() { _ = sess.Close() }()

		var stdout bytes.Buffer
		sess.Stdout = &stdout
		require.NoError(t, sess.Run("echo hi"))
		require.Equal(t, "hi\n", stdout.String())
	})

	t.Run("exec exits nonzero, proving exit-status is wired", func(t *testing.T) {
		sess, err := sshClient.NewSession()
		require.NoError(t, err)
		defer func() { _ = sess.Close() }()

		err = sess.Run("false")
		var exitErr *ssh.ExitError
		require.ErrorAs(t, err, &exitErr)
		require.Equal(t, 1, exitErr.ExitStatus())
	})

	t.Run("a second channel works while the first is still open", func(t *testing.T) {
		// sessA runs `cat`, which blocks reading stdin until it's closed. If
		// the agent were serving only one channel and then returning, sessB
		// would never get accepted while sessA is in flight.
		sessA, err := sshClient.NewSession()
		require.NoError(t, err)
		defer func() { _ = sessA.Close() }()

		stdinA, err := sessA.StdinPipe()
		require.NoError(t, err)
		require.NoError(t, sessA.Start("cat"))

		sessB, err := sshClient.NewSession()
		require.NoError(t, err)
		defer func() { _ = sessB.Close() }()

		var stdoutB bytes.Buffer
		sessB.Stdout = &stdoutB
		require.NoError(t, sessB.Run("echo concurrent-ok"))
		require.Equal(t, "concurrent-ok\n", stdoutB.String())

		require.NoError(t, stdinA.Close())
		require.NoError(t, sessA.Wait())
	})

	t.Run("sftp round trip", func(t *testing.T) {
		sftpClient, err := sftp.NewClient(sshClient)
		require.NoError(t, err)
		defer func() { _ = sftpClient.Close() }()

		path := filepath.Join(t.TempDir(), "roundtrip.txt")
		const content = "sftp round trip over the exec stream"

		wf, err := sftpClient.Create(path)
		require.NoError(t, err)
		_, err = wf.Write([]byte(content))
		require.NoError(t, err)
		require.NoError(t, wf.Close())

		rf, err := sftpClient.Open(path)
		require.NoError(t, err)
		data, err := io.ReadAll(rf)
		require.NoError(t, err)
		require.NoError(t, rf.Close())
		require.Equal(t, content, string(data))
	})
}
