package session

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/go-faster/gooners/internal/e2e"
)

// TestConfig_Dial_KnownHostsAlgorithmMismatch tests that a connection succeeds
// when known_hosts contains only an ED25519 key but the server advertises ECDSA
// first. The fix in knownHostsAlgorithms detects the stored key type and
// restricts HostKeyAlgorithms so the server sends the matching key.
func TestConfig_Dial_KnownHostsAlgorithmMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	addr, user, password, cleanup, err := e2e.NewSudoTestContainer(t.Context(), e2e.ContainerOpts{PreferECDSA: true})
	if err != nil {
		t.Skipf("skipping: could not start container: %v", err)
	}
	t.Cleanup(cleanup)

	// Give sshd a moment to reload its config after SIGHUP.
	time.Sleep(2 * time.Second)

	// Probe the server requesting only ED25519 to capture that key.
	var ed25519Key ssh.PublicKey
	probeClient, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			ed25519Key = key
			return nil
		},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		Timeout:           30 * time.Second,
	})
	require.NoError(t, err, "probe connection to capture ED25519 key")
	_ = probeClient.Close()
	require.NotNil(t, ed25519Key)
	require.Equal(t, ssh.KeyAlgoED25519, ed25519Key.Type())

	// Write only the ED25519 key to a known_hosts file.
	// The server now prefers ECDSA; without the fix, the client would receive an
	// ECDSA key and fail with "key mismatch" because ED25519 is all that's stored.
	tmp := t.TempDir()
	khPath := filepath.Join(tmp, "known_hosts")
	entry := addr + " " + string(ssh.MarshalAuthorizedKey(ed25519Key))
	require.NoError(t, os.WriteFile(khPath, []byte(entry), 0o600))

	// Connect via the pool using proper host key verification.
	pool := NewPool(PoolOptions{})
	ctx := t.Context()
	go pool.RunLoop(ctx)

	openRes, err := pool.OpenCfg(ctx, Config{
		Machine:    addr,
		User:       user,
		Password:   password,
		KnownHosts: khPath,
	})
	require.NoError(t, err, "OpenCfg must succeed with knownHostsAlgorithms fix")

	res := pool.Exec(ctx, ExecRequest{
		SessionID: openRes.ID,
		Command:   "echo hello",
	})
	require.NoError(t, res.Err)
	require.Equal(t, 0, res.ExitCode)
	require.Contains(t, res.Stdout, "hello")
}

func TestSudoExec(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
		return
	}

	addr, user, password, cleanup, err := e2e.NewSudoTestContainer(t.Context(), e2e.ContainerOpts{SudoRequirePassword: true})
	if err != nil {
		t.Skipf("skipping sudo integration test: could not start container: %v", err)
	}
	t.Cleanup(cleanup)

	p := NewPool(PoolOptions{CommandTimeout: 0})
	ctx := t.Context()

	go p.RunLoop(ctx)

	openRes, err := p.OpenCfg(ctx, Config{
		Machine:    addr,
		User:       user,
		Password:   password,
		KnownHosts: "insecure",
	})
	require.NoError(t, err)
	id := openRes.ID

	// Execute a command with Sudo and SudoPassword.
	// Since the container requires "secret" for sudo, this should succeed.
	res := p.Exec(ctx, ExecRequest{
		SessionID:    id,
		Command:      "whoami",
		Sudo:         true,
		SudoPassword: password,
	})

	require.NoError(t, res.Err)
	require.Equal(t, 0, res.ExitCode)
	require.Equal(t, "root\n", res.Stdout)

	// Wrong password must fail — container is started with SudoRequirePassword.
	resFail := p.Exec(ctx, ExecRequest{
		SessionID:    id,
		Command:      "whoami",
		Sudo:         true,
		SudoPassword: "wrong_password",
	})
	require.NotEqual(t, 0, resFail.ExitCode)
}
