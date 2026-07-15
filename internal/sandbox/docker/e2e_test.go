package docker_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/go-faster/gooners/internal/sandbox"
	"github.com/go-faster/gooners/internal/sandbox/agent"
	"github.com/go-faster/gooners/internal/sandbox/docker"
)

// buildSandboxAgent compiles cmd/sandbox-agent for GOOS=linux, GOARCH=arch
// into <dir>/<arch>/sandbox-agent, exactly the layout docker.Options.AgentDir
// expects.
func buildSandboxAgent(t *testing.T, dir, arch string) string {
	t.Helper()
	dest := filepath.Join(dir, arch, "sandbox-agent")
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))

	cmd := exec.Command("go", "build", "-o", dest, "github.com/go-faster/gooners/cmd/sandbox-agent")
	// GOFLAGS may carry -race from the outer `go test -race` invocation;
	// clear it since -race requires cgo and this is a CGO_ENABLED=0 cross-build.
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=", "GOOS=linux", "GOARCH="+arch)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "building sandbox-agent: %s", out)
	return dest
}

// requireDocker builds a Runner (with agentDir, if given) and skips the test
// if a Docker daemon is not reachable, so this suite fails soft in
// environments without Docker (CI without a daemon, sandboxed dev
// containers) rather than failing hard.
func requireDocker(t *testing.T, agentDir string) *docker.Runner {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Docker E2E test in short mode")
	}
	if runtime.GOOS != "linux" {
		t.Skipf("skipping Docker E2E test: no Docker daemon on %s CI runners", runtime.GOOS)
	}

	runner, err := docker.New(docker.Options{
		Policy: sandbox.Policy{
			DropCaps:        []string{"ALL"},
			NoNewPrivileges: true,
			MemoryBytes:     256 * 1024 * 1024,
			CPUs:            1,
			PidsLimit:       128,
		},
		AgentDir: agentDir,
	})
	if err != nil {
		t.Skipf("skipping Docker E2E test: %v", err)
	}
	return runner
}

// sshHandshake writes the agent preamble across conn and drives the client
// side of the SSH handshake, mirroring exactly what
// internal/sandbox.Manager.Open does (see its doc comments), but inlined
// here so this test does not depend on Manager.
func sshHandshake(t *testing.T, conn net.Conn) *ssh.Client {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	hostPubKey, err := ssh.NewPublicKey(hostPriv.Public())
	require.NoError(t, err)
	hostPEMBlock, err := ssh.MarshalPrivateKey(hostPriv, "")
	require.NoError(t, err)

	_, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	clientSigner, err := ssh.NewSignerFromKey(clientPriv)
	require.NoError(t, err)

	preamble := agent.Preamble{
		Version:       1,
		HostKeyPEM:    string(pem.EncodeToMemory(hostPEMBlock)),
		AuthorizedKey: string(ssh.MarshalAuthorizedKey(clientSigner.PublicKey())),
	}
	require.NoError(t, agent.WritePreamble(conn, preamble))
	require.NoError(t, conn.SetDeadline(time.Now().Add(30*time.Second)))

	clientConn, chans, reqs, err := ssh.NewClientConn(conn, "sandbox", &ssh.ClientConfig{
		User:            "sandbox",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientSigner)},
		HostKeyCallback: ssh.FixedHostKey(hostPubKey),
	})
	require.NoError(t, err)
	require.NoError(t, conn.SetDeadline(time.Time{}))
	return ssh.NewClient(clientConn, chans, reqs)
}

func TestRunner_E2E(t *testing.T) {
	agentDir := t.TempDir()
	buildSandboxAgent(t, agentDir, runtime.GOARCH)

	runnerWithAgent := requireDocker(t, agentDir)
	defer func() { _ = runnerWithAgent.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sb, err := runnerWithAgent.Create(ctx, sandbox.Spec{Image: "alpine:latest", Network: sandbox.NetworkNone})
	require.NoError(t, err)
	require.NotEmpty(t, sb.ID)

	destroyed := false
	defer func() {
		if destroyed {
			return
		}
		_ = runnerWithAgent.Destroy(context.Background(), sb.ID)
	}()

	conn, err := runnerWithAgent.Dial(ctx, sb.ID)
	require.NoError(t, err)

	sshClient := sshHandshake(t, conn)
	defer func() { _ = sshClient.Close() }()

	t.Run("echo hi exits zero", func(t *testing.T) {
		sess, err := sshClient.NewSession()
		require.NoError(t, err)
		defer func() { _ = sess.Close() }()

		var stdout bytes.Buffer
		sess.Stdout = &stdout
		require.NoError(t, sess.Run("echo hi"))
		require.Equal(t, "hi\n", stdout.String())
	})

	t.Run("false exits nonzero", func(t *testing.T) {
		sess, err := sshClient.NewSession()
		require.NoError(t, err)
		defer func() { _ = sess.Close() }()

		err = sess.Run("false")
		var exitErr *ssh.ExitError
		require.ErrorAs(t, err, &exitErr)
		require.Equal(t, 1, exitErr.ExitStatus())
	})

	t.Run("sftp round trip", func(t *testing.T) {
		sftpClient, err := sftp.NewClient(sshClient)
		require.NoError(t, err)
		defer func() { _ = sftpClient.Close() }()

		const remotePath = "/tmp/roundtrip.txt"
		const content = "sandbox docker e2e sftp round trip"

		wf, err := sftpClient.Create(remotePath)
		require.NoError(t, err)
		_, err = wf.Write([]byte(content))
		require.NoError(t, err)
		require.NoError(t, wf.Close())

		rf, err := sftpClient.Open(remotePath)
		require.NoError(t, err)
		data := make([]byte, len(content))
		_, err = rf.Read(data)
		require.NoError(t, err)
		require.NoError(t, rf.Close())
		require.Equal(t, content, string(data))
	})

	t.Run("network none: only loopback is visible", func(t *testing.T) {
		sess, err := sshClient.NewSession()
		require.NoError(t, err)
		defer func() { _ = sess.Close() }()

		var stdout bytes.Buffer
		sess.Stdout = &stdout
		// This is the whole point of the design: SSH still works over the
		// exec/attach stream even though the sandbox has no NIC at all.
		require.NoError(t, sess.Run("ip addr show 2>/dev/null || ifconfig -a 2>/dev/null"))
		out := stdout.String()
		require.Contains(t, out, "lo")
		require.NotContains(t, out, "eth0")
	})

	require.NoError(t, runnerWithAgent.Destroy(ctx, sb.ID))
	destroyed = true

	require.Eventually(t, func() bool {
		list, err := runnerWithAgent.List(context.Background(), sandbox.Filter{})
		if err != nil {
			return false
		}
		for _, s := range list {
			if s.ID == sb.ID {
				return false
			}
		}
		return true
	}, 30*time.Second, 200*time.Millisecond, "destroyed container must disappear from List")
}

func TestRunner_E2E_UnknownImageArchitectureFailsClearly(t *testing.T) {
	runner := requireDocker(t, "")
	defer func() { _ = runner.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// No AgentDir configured and (almost certainly) no "sandbox-agent" on
	// $PATH: Create must fail with a clear error, not hang or panic.
	_, err := runner.Create(ctx, sandbox.Spec{Image: "alpine:latest"})
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "sandbox-agent")
}
