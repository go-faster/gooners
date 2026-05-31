package session

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-faster/gooners/internal/sshutil"
	"github.com/stretchr/testify/require"
)

// dialInsecure returns a Config that connects to addr without host-key
// verification, suitable for in-process test servers.
func dialInsecure(addr string) Config {
	return Config{Machine: addr, KnownHosts: "insecure"}
}

// runCmd runs a single command on a just-dialled connection and returns stdout.
func runCmd(t *testing.T, cfg Config, cmd string) string {
	t.Helper()
	client, err := cfg.dial()
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	res, err := sshutil.Run(context.Background(), client, cmd)
	require.NoError(t, err)
	return res.Stdout
}

// The test server echoes the command as stdout, so "echo hello" → "echo hello\n".
const testCmd = "echo hello"
const testOut = "echo hello\n"

func TestConfig_Dial_Direct(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	require.Equal(t, testOut, runCmd(t, dialInsecure(srv.addr), testCmd))
}

func TestConfig_Dial_ProxyJump(t *testing.T) {
	t.Parallel()
	target := newTestServer(t)
	jump := newTestServer(t)

	cfg := dialInsecure(target.addr)
	cfg.ProxyJump = jump.addr

	require.Equal(t, testOut, runCmd(t, cfg, testCmd))
}

func TestConfig_Dial_ProxyJump_Chain(t *testing.T) {
	t.Parallel()
	target := newTestServer(t)
	jump1 := newTestServer(t)
	jump2 := newTestServer(t)

	// ProxyJump "jump1,jump2" means local → jump1 → jump2 → target.
	cfg := dialInsecure(target.addr)
	cfg.ProxyJump = jump1.addr + "," + jump2.addr

	require.Equal(t, testOut, runCmd(t, cfg, testCmd))
}

func runSCPTest(t *testing.T, cfg Config, content string) {
	t.Helper()
	client, err := cfg.dial()
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	sess, err := client.NewSession()
	require.NoError(t, err)
	defer sess.Close()

	w, err := sess.StdinPipe()
	require.NoError(t, err)
	r, err := sess.StdoutPipe()
	require.NoError(t, err)

	err = sess.Start("scp -t /tmp/test.txt")
	require.NoError(t, err)

	// Wait for 0x00
	buf := make([]byte, 1)
	_, err = r.Read(buf)
	require.NoError(t, err)
	require.Equal(t, byte(0), buf[0])

	// Send header
	fmt.Fprintf(w, "C0644 %d test.txt\n", len(content))

	// Wait for 0x00
	_, err = r.Read(buf)
	require.NoError(t, err)
	require.Equal(t, byte(0), buf[0])

	// Send content + 0x00
	w.Write([]byte(content))
	w.Write([]byte{0})

	// Wait for 0x00
	_, err = r.Read(buf)
	require.NoError(t, err)
	require.Equal(t, byte(0), buf[0])

	err = sess.Wait()
	require.NoError(t, err)
}

func TestConfig_Dial_SCP(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	runSCPTest(t, dialInsecure(srv.addr), "scp test content")
}

func TestConfig_Dial_SCP_ProxyJump(t *testing.T) {
	t.Parallel()
	target := newTestServer(t)
	jump := newTestServer(t)

	cfg := dialInsecure(target.addr)
	cfg.ProxyJump = jump.addr

	runSCPTest(t, cfg, "scp proxyjump test content")
}
