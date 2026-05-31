package session

import (
	"context"
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
