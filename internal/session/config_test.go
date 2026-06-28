package session

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gosshconfig "github.com/kevinburke/ssh_config"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

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

	_, err := authMethods(cfg, Config{Machine: "testhost"}, "testhost", "user", tmpDir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no authentication methods available")
}

func TestConfig_Dial_PasswordAuth(t *testing.T) {
	t.Parallel()
	const pass = "hunter2"
	srv := newTestServerPassword(t, pass)

	cfg := dialInsecure(t, srv.addr)
	cfg.Password = pass

	require.Equal(t, testOut, runCmd(t, cfg, testCmd))
}

func TestConfig_Dial_PasswordAuth_Wrong(t *testing.T) {
	t.Parallel()
	srv := newTestServerPassword(t, "correct")

	cfg := dialInsecure(t, srv.addr)
	cfg.Password = "wrong"

	_, _, _, err := cfg.dial() //nolint:dogsled // only error matters here
	require.Error(t, err)
}

// dialInsecure returns a Config that connects to addr without host-key
// verification, suitable for in-process test servers.
func dialInsecure(t *testing.T, addr string) Config {
	t.Helper()
	return Config{Machine: addr, KnownHosts: "insecure", HomeDir: t.TempDir()}
}

// runCmd runs a single command on a just-dialed connection and returns stdout.
//
//nolint:unparam // cmd is always testCmd in existing tests
func runCmd(t *testing.T, cfg Config, cmd string) string {
	t.Helper()
	client, _, _, err := cfg.dial()
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
	require.Equal(t, testOut, runCmd(t, dialInsecure(t, srv.addr), testCmd))
}

func TestConfig_Dial_ProxyJump(t *testing.T) {
	t.Parallel()
	target := newTestServer(t)
	jump := newTestServer(t)

	cfg := dialInsecure(t, target.addr)
	cfg.ProxyJump = jump.addr

	require.Equal(t, testOut, runCmd(t, cfg, testCmd))
}

func TestConfig_Dial_ProxyJump_Chain(t *testing.T) {
	t.Parallel()
	target := newTestServer(t)
	jump1 := newTestServer(t)
	jump2 := newTestServer(t)

	// ProxyJump "jump1,jump2" means local → jump1 → jump2 → target.
	cfg := dialInsecure(t, target.addr)
	cfg.ProxyJump = jump1.addr + "," + jump2.addr

	require.Equal(t, testOut, runCmd(t, cfg, testCmd))
}

func runSCPTest(t *testing.T, cfg Config, content string) {
	t.Helper()
	client, _, _, err := cfg.dial()
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
	runSCPTest(t, dialInsecure(t, srv.addr), "scp test content")
}

func TestConfig_Dial_SCP_ProxyJump(t *testing.T) {
	t.Parallel()
	target := newTestServer(t)
	jump := newTestServer(t)

	cfg := dialInsecure(t, target.addr)
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

	c := Config{Machine: "test-dyn", HomeDir: tmpDir}
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

func TestConfig_ClientConfig_ExpandsHostNameTokens(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	confPath := filepath.Join(tmpDir, "ssh_config")
	require.NoError(t, os.WriteFile(confPath, []byte(
		"Host *\n"+
			"  HostName %h\n"+
			"  Port 2222\n"+
			"  User alice\n",
	), 0o600))

	cfg := &gosshconfig.UserSettings{IgnoreErrors: false}
	cfg.ConfigFinder(func() string { return confPath })

	cc, tcpAddr, sshAddr, err := Config{Machine: "10.1.2.3", HomeDir: tmpDir}.clientConfig(cfg)
	require.NoError(t, err)
	require.Equal(t, "10.1.2.3:2222", tcpAddr)
	require.Equal(t, "10.1.2.3:2222", sshAddr)
	require.Equal(t, "alice", cc.User)
}

func TestPool_OpenCfg_LocalForward(t *testing.T) {
	t.Parallel()
	sshSrv := newTestServer(t)
	echoAddr := startTCPEchoServer(t)
	localAddr := reserveTCPAddr(t)

	host, port, err := net.SplitHostPort(sshSrv.addr)
	require.NoError(t, err)

	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	require.NoError(t, os.MkdirAll(sshDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sshDir, "config"), []byte(
		"Host forwarded\n"+
			"  HostName "+host+"\n"+
			"  Port "+port+"\n"+
			"  LocalForward "+localAddr+" "+echoAddr+"\n",
	), 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := NewPool(PoolOptions{HomeDir: home})
	go p.RunLoop(ctx)

	openCtx, openCancel := context.WithTimeout(ctx, 5*time.Second)
	defer openCancel()
	res, err := p.OpenCfg(openCtx, Config{Machine: "forwarded", KnownHosts: "insecure"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, p.Close(context.Background(), res.ID)) })

	conn, err := net.DialTimeout("tcp", localAddr, 5*time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, len("ping"))
	_, err = conn.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf))
}

func startTCPEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln.Addr().String()
}

func reserveTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func TestTruncateBanner(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"\n\n  \n", ""},
		{"Hello World", "Hello World"},
		{"\nFirst non-empty line\nSecond line", "First non-empty line"},
		{"   Leading and trailing space   \nAnother line", "Leading and trailing space"},
		{strings.Repeat("A", 120), strings.Repeat("A", 100)},
		{"\n" + strings.Repeat("B", 150) + "\nLine 2", strings.Repeat("B", 100)},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := truncateBanner(tc.input)
			require.Equal(t, tc.expected, got)
		})
	}
}

func TestDetectPlatform(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"SSH-2.0-OpenSSH_9.5p1 Ubuntu-1ubuntu3", "linux"},
		{"SSH-2.0-OpenSSH_for_Windows_8.1", "windows"},
		{"SSH-2.0-Cisco-1.25", "cisco"},
		{"SSH-2.0-ROSSSH", "mikrotik"},
		{"SSH-2.0-libssh_0.9.5", "unknown"},
		{"SSH-2.0-paramiko_2.7.2", "unknown"},
		{"", "unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := detectPlatform(tc.input)
			require.Equal(t, tc.expected, got)
		})
	}
}

func TestKnownHostsAlgorithms(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	// Write a known_hosts file containing only the server's ED25519 host key.
	tmp := t.TempDir()
	khPath := filepath.Join(tmp, "known_hosts")

	line := ssh.MarshalAuthorizedKey(srv.hostKey.PublicKey())
	// known_hosts format: "<addr> <keytype> <base64>"
	entry := srv.addr + " " + string(line)
	require.NoError(t, os.WriteFile(khPath, []byte(entry), 0o600))

	algos := knownHostsAlgorithms(khPath, tmp, []string{srv.addr}, slog.New(slog.DiscardHandler))
	require.NotEmpty(t, algos, "expected at least one algorithm for known host")
	require.Contains(t, algos, srv.hostKey.PublicKey().Type())
}

func TestExpandProxyCommandTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		command    string
		host       string
		port       string
		remoteUser string
		expected   string
	}{
		{
			name:       "no tokens",
			command:    "ssh -W host:port jump",
			host:       "example.com",
			port:       "22",
			remoteUser: "user",
			expected:   "ssh -W host:port jump",
		},
		{
			name:       "all tokens",
			command:    "ssh -l %r -p %p %h %%",
			host:       "example.com",
			port:       "2222",
			remoteUser: "admin",
			expected:   "ssh -l admin -p 2222 example.com %",
		},
		{
			name:       "partial tokens",
			command:    "nc %h %p",
			host:       "1.2.3.4",
			port:       "443",
			remoteUser: "root",
			expected:   "nc 1.2.3.4 443",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := expandProxyCommandTokens(tc.command, tc.host, tc.port, tc.remoteUser)
			require.Equal(t, tc.expected, got)
		})
	}
}

func TestMultiAddrHostKeyCallback(t *testing.T) {
	t.Parallel()

	// Generate key pairs for testing
	_, priv1, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer1, err := ssh.NewSignerFromKey(priv1)
	require.NoError(t, err)
	key1 := signer1.PublicKey()

	_, priv2, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer2, err := ssh.NewSignerFromKey(priv2)
	require.NoError(t, err)
	key2 := signer2.PublicKey()

	tmp := t.TempDir()
	khPath := filepath.Join(tmp, "known_hosts")

	// Helper to format line
	formatLine := func(host string, key ssh.PublicKey) string {
		return host + " " + string(ssh.MarshalAuthorizedKey(key))
	}

	// 1. Success: alias matches
	entry := formatLine("alias-host", key1)
	require.NoError(t, os.WriteFile(khPath, []byte(entry), 0o600))
	cb := multiAddrHostKeyCallback(khPath, tmp, slog.New(slog.DiscardHandler), "alias-host:22", "ip-host:22")
	err = cb("alias-host:22", &net.TCPAddr{}, key1)
	require.NoError(t, err)

	// 2. Failure: alias key changed (hard failure)
	err = cb("alias-host:22", &net.TCPAddr{}, key2)
	require.Error(t, err)
	require.Contains(t, err.Error(), "host key changed for alias-host:22")

	// 3. Stale fallback skipped: alias has no entry, but IP host has entry which does not match key1.
	// In this case, we call callback with key1.
	// Since i=0 (alias-host) is unknown, it's skipped.
	// For i=1 (ip-host), it has key2 in known_hosts, so key1 mismatch returns KeyError with Want > 0.
	// Since i=1 != 0, it skips (warning logged) and moves on.
	// Finally, it returns unknown error for the scanHost.
	entryStale := formatLine("ip-host", key2)
	require.NoError(t, os.WriteFile(khPath, []byte(entryStale), 0o600))
	cbStale := multiAddrHostKeyCallback(khPath, tmp, slog.New(slog.DiscardHandler), "alias-host:22", "ip-host:22")
	err = cbStale("alias-host:22", &net.TCPAddr{}, key1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "host key unknown")
	require.Contains(t, err.Error(), "ssh-keyscan")

	// 4. Success: alias has no entry, but IP host has correct entry
	entryIPCorrect := formatLine("ip-host", key1)
	require.NoError(t, os.WriteFile(khPath, []byte(entryIPCorrect), 0o600))
	cbIP := multiAddrHostKeyCallback(khPath, tmp, slog.New(slog.DiscardHandler), "alias-host:22", "ip-host:22")
	err = cbIP("alias-host:22", &net.TCPAddr{}, key1)
	require.NoError(t, err)
}

func TestDialProxyCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows: no sh/cat")
	}

	logger := slog.New(slog.DiscardHandler)

	// Test normal loopback using "cat"
	conn, err := dialProxyCommand("cat", "127.0.0.1:22", "user", logger)
	require.NoError(t, err)
	defer conn.Close()

	msg := []byte("hello proxy")
	n, err := conn.Write(msg)
	require.NoError(t, err)
	require.Equal(t, len(msg), n)

	buf := make([]byte, len(msg))
	n, err = conn.Read(buf)
	require.NoError(t, err)
	require.Equal(t, len(msg), n)
	require.Equal(t, msg, buf[:n])

	// Verify methods on proxyCommandConn
	require.Equal(t, "proxy-command", conn.LocalAddr().Network())
	require.Equal(t, "proxy-command", conn.LocalAddr().String())
	require.Equal(t, "proxy-command", conn.RemoteAddr().Network())
	require.Equal(t, "proxy-command", conn.RemoteAddr().String())
	require.NoError(t, conn.SetDeadline(time.Now()))
	require.NoError(t, conn.SetReadDeadline(time.Now()))
	require.NoError(t, conn.SetWriteDeadline(time.Now()))

	// Test address without port fallback
	conn2, err := dialProxyCommand("cat", "127.0.0.1", "user", logger)
	require.NoError(t, err)
	conn2.Close()

	// Test command execution failure (fails on Read or Write because command cannot execute)
	conn3, err := dialProxyCommand("/nonexistent/command/path/xyz", "127.0.0.1:22", "user", logger)
	require.NoError(t, err)
	defer conn3.Close()
	buf3 := make([]byte, 10)
	_, err = conn3.Read(buf3)
	require.Error(t, err)
}
