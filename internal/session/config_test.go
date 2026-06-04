package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	gosshconfig "github.com/kevinburke/ssh_config"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/sshutil"
)

func TestAuthMethods_IdentitiesOnly_MissingKeyFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	confPath := filepath.Join(tmpDir, "ssh_config")
	require.NoError(t, os.WriteFile(confPath, []byte(
		"Host testhost\n"+
			"  IdentitiesOnly yes\n"+
			"  IdentityFile /nonexistent/key\n",
	), 0o600))

	cfg := &gosshconfig.UserSettings{IgnoreErrors: false}
	cfg.ConfigFinder(func() string { return confPath })

	_, err := authMethods(cfg, Config{Machine: "testhost"}, "testhost", "user")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no authentication methods available")
}

func TestConfig_Dial_PasswordAuth(t *testing.T) {
	t.Parallel()
	const pass = "hunter2"
	srv := newTestServerPassword(t, pass)

	cfg := dialInsecure(srv.addr)
	cfg.Password = pass

	require.Equal(t, testOut, runCmd(t, cfg, testCmd))
}

func TestConfig_Dial_PasswordAuth_Wrong(t *testing.T) {
	t.Parallel()
	srv := newTestServerPassword(t, "correct")

	cfg := dialInsecure(srv.addr)
	cfg.Password = "wrong"

	_, err := cfg.dial()
	require.Error(t, err)
}

// dialInsecure returns a Config that connects to addr without host-key
// verification, suitable for in-process test servers.
func dialInsecure(addr string) Config {
	return Config{Machine: addr, KnownHosts: "insecure"}
}

// runCmd runs a single command on a just-dialed connection and returns stdout.
func runCmd(t *testing.T, cfg Config, cmd string) string {
	t.Helper()
	client, err := cfg.dial()
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	res, err := sshutil.Run(context.Background(), client, cmd, sshutil.RunOptions{})
	require.NoError(t, err)
	return res.Stdout
}

// The test server echoes the command as stdout, so "echo hello" → "echo hello\n".
const (
	testCmd = "echo hello"
	testOut = "echo hello\n"
)

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

func TestConfig_DynamicReload(t *testing.T) {
	tmpDir := t.TempDir()
	confPath := filepath.Join(tmpDir, "ssh_config")

	err := os.WriteFile(confPath, []byte("Host test-dyn\n  HostName 1.2.3.4\n  Port 2222\n  User foo\n"), 0o600)
	require.NoError(t, err)

	cfg := &gosshconfig.UserSettings{IgnoreErrors: false}
	cfg.ConfigFinder(func() string {
		return confPath
	})

	c := Config{Machine: "test-dyn"}
	cc, tcpAddr, sshAddr, err := c.clientConfig(cfg)
	require.NoError(t, err)
	require.Equal(t, "1.2.3.4:2222", tcpAddr)
	require.Equal(t, "test-dyn:2222", sshAddr)
	require.Equal(t, "foo", cc.User)

	// Now modify the file
	err = os.WriteFile(confPath, []byte("Host test-dyn\n  HostName 5.6.7.8\n  Port 4444\n  User bar\n"), 0o600)
	require.NoError(t, err)

	// A new clientConfig call with a new UserSettings instance should parse the updated file!
	cfg2 := &gosshconfig.UserSettings{IgnoreErrors: false}
	cfg2.ConfigFinder(func() string {
		return confPath
	})

	cc2, tcpAddr2, sshAddr2, err := c.clientConfig(cfg2)
	require.NoError(t, err)
	require.Equal(t, "5.6.7.8:4444", tcpAddr2)
	require.Equal(t, "test-dyn:4444", sshAddr2)
	require.Equal(t, "bar", cc2.User)
}
