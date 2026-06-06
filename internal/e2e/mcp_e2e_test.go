package e2e_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/go-faster/gooners/internal/e2e"
	"github.com/go-faster/gooners/internal/session"

	// Tool registrations for full server
	"github.com/go-faster/gooners/internal/tools/core"
	"github.com/go-faster/gooners/internal/tools/disk"
	"github.com/go-faster/gooners/internal/tools/fs"
	"github.com/go-faster/gooners/internal/tools/proc"
	"github.com/go-faster/gooners/internal/tools/sysinfo"
)

type testEnv struct {
	Client     *mcp.ClientSession
	Server     *mcp.ServerSession
	Addr       string
	User       string
	Pass       string
	UploadRoot string
	HomeDir    string
}

var (
	sharedEnv     *testEnv
	sharedEnvOnce sync.Once
	sharedEnvErr  error
)

// TestMain exists so we can use a single container + MCP harness for the entire
// package (via sync.Once). This reduces wall time from ~9 container startups
// to 1 (biggest practical improvement for the suite).
func TestMain(m *testing.M) {
	os.Unsetenv("SSH_AUTH_SOCK")
	code := m.Run()
	os.Exit(code)
}

func getSharedEnv(t *testing.T) *testEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping E2E tests in short mode")
	}

	sharedEnvOnce.Do(func() {
		// Real setup happens only once for the whole test package.
		// We keep the context alive for the lifetime of the shared pool/server.
		ctx := context.Background()

		addr, user, password, ctrCleanup, err := e2e.NewSudoTestContainer(ctx, e2e.ContainerOpts{SudoRequirePassword: false})
		if err != nil {
			sharedEnvErr = err
			return
		}
		// The container lives until the test process exits (no t.Cleanup in Once).

		homeDir, err := os.MkdirTemp("", "gooners-e2e-home-*")
		if err != nil {
			sharedEnvErr = err
			return
		}
		if err := os.MkdirAll(filepath.Join(homeDir, ".ssh"), 0o700); err != nil {
			sharedEnvErr = err
			return
		}

		p := session.NewPool(session.PoolOptions{CommandTimeout: 0, HomeDir: homeDir})
		go p.RunLoop(ctx)

		// Small delay so the pool goroutine is scheduled before first open.
		// The channel-based design means the first RPC would block anyway,
		// but this makes -race happier and matches documented "must call Run".
		time.Sleep(30 * time.Millisecond)

		s := mcp.NewServer(&mcp.Implementation{Name: "ssh-mcp-e2e", Version: "test"}, nil)
		uploadRoot := os.TempDir() // shared across tests; withinDir still applies per-call
		core.Register(s, p, core.RegisterOptions{})
		fs.Register(s, p, uploadRoot)
		sysinfo.Register(s, p)
		proc.Register(s, p)
		disk.Register(s, p)
		// NOTE: systemd intentionally not registered (per plan: evaluate later)

		st, ct := mcp.NewInMemoryTransports()
		ss, err := s.Connect(ctx, st, nil)
		if err != nil {
			ctrCleanup()
			sharedEnvErr = err
			return
		}

		c := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "0"}, nil)
		cs, err := c.Connect(ctx, ct, nil)
		if err != nil {
			_ = ss.Close()
			ctrCleanup()
			sharedEnvErr = err
			return
		}

		sharedEnv = &testEnv{
			Client:     cs,
			Server:     ss,
			Addr:       addr,
			User:       user,
			Pass:       password,
			UploadRoot: uploadRoot,
			HomeDir:    homeDir,
		}
	})

	if sharedEnvErr != nil {
		t.Skipf("skipping E2E: shared container/MCP setup failed: %v", sharedEnvErr)
	}
	return sharedEnv
}

// setupMCPEnv is kept for backward compatibility inside the package but now
// delegates to the shared fixture (one container for speed).
func setupMCPEnv(t *testing.T) *testEnv {
	return getSharedEnv(t)
}

func (e *testEnv) open(t *testing.T) string {
	t.Helper()
	data := callJSON(t, e.Client, "ssh_open_cfg", map[string]any{
		"machine":     e.Addr,
		"user":        e.User,
		"password":    e.Pass,
		"known_hosts": "insecure",
	})
	sid, _ := data["session_id"].(string)
	require.NotEmpty(t, sid)
	ua, _ := data["user_agent"].(string)
	require.Contains(t, ua, "SSH-2.0-")
	platform, _ := data["platform"].(string)
	require.Equal(t, "linux", platform)

	// Ensure the session is closed at end of this test (gives coverage for
	// ssh_close on every path that opens a session, and prevents leakage
	// across the shared fixture).
	t.Cleanup(func() {
		_, _ = e.Client.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "ssh_close",
			Arguments: map[string]any{"session_id": sid},
		})
	})

	return sid
}

func callJSON(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.NotEmpty(t, res.Content)
	tc, ok := res.Content[0].(*mcp.TextContent)
	if res.IsError {
		t.Logf("tool %s returned IsError=true. Content: %s", name, tc.Text)
	}
	require.False(t, res.IsError, "tool %s error result: %+v", name, res)
	require.True(t, ok, "expected TextContent")
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m), "text=%s", tc.Text)
	return m
}

func callRaw(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.False(t, res.IsError, "tool %s returned error result: %+v", name, res)
	if len(res.Content) == 0 {
		t.Fatalf("tool %s returned empty content", name)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)

	var m map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &m); err == nil {
		if stdout, ok := m["stdout"].(string); ok {
			return stdout
		}
	}

	return tc.Text
}

// callRawResult returns the full result (for rare cases that want to inspect errors).
func callRawResult(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	return res
}

// callRawTolerant extracts the text content even if the tool returned IsError=true.
// This is useful for diagnostic tools (proc_info, proc_lsof, some fs/sysinfo) that
// return useful stdout/stderr mixed with a non-zero exit.
func callRawTolerant(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res := callRawResult(t, cs, name, args)
	if len(res.Content) == 0 {
		t.Fatalf("tool %s returned empty content (even on error)", name)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)

	var m map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &m); err == nil {
		if stdout, ok := m["stdout"].(string); ok {
			return stdout
		}
	}

	return tc.Text
}

func TestE2E_Core_OpenListClose(t *testing.T) {
	env := setupMCPEnv(t)
	sid := env.open(t)

	// list should include our session
	listData := callJSON(t, env.Client, "ssh_list", map[string]any{})
	sessions, ok := listData["sessions"].([]any)
	require.True(t, ok)
	found := false
	for _, s := range sessions {
		m, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if m["id"] == sid {
			found = true
			ua, _ := m["user_agent"].(string)
			require.Contains(t, ua, "SSH-2.0-")
			platform, _ := m["platform"].(string)
			require.Equal(t, "linux", platform)
		}
	}
	require.True(t, found, "session %s not found in list", sid)

	// close it
	callJSON(t, env.Client, "ssh_close", map[string]any{"session_id": sid})

	// list again, should not have it (or empty-ish)
	listText2 := callRaw(t, env.Client, "ssh_list", map[string]any{})
	require.NotContains(t, listText2, sid)
}

func TestE2E_Core_Exec(t *testing.T) {
	env := setupMCPEnv(t)
	sid := env.open(t)

	// exec simple cmd
	out := callRaw(t, env.Client, "ssh_exec", map[string]any{
		"session_id": sid,
		"command":    "echo hello-e2e",
	})
	require.Contains(t, out, "hello-e2e")
}

func TestE2E_Core_SudoExec(t *testing.T) {
	env := setupMCPEnv(t)
	sid := env.open(t)

	out := callRaw(t, env.Client, "ssh_sudo_exec", map[string]any{
		"session_id":    sid,
		"command":       "whoami",
		"sudo_password": env.Pass,
	})
	require.Contains(t, out, "root")
}

func TestE2E_Core_OnceExec(t *testing.T) {
	env := setupMCPEnv(t)

	res := callRawResult(t, env.Client, "ssh_once_exec", map[string]any{
		"machine": env.Addr,
		"command": "echo once",
	})
	require.True(t, res.IsError, "ssh_once_exec should fail without ssh config entry for the test addr")
	require.NotEmpty(t, res.Content)
}

func TestE2E_FS_WriteCatGrepStat(t *testing.T) {
	env := setupMCPEnv(t)
	sid := env.open(t)

	path := "/tmp/gooners-e2e-test.txt"
	content := "hello from e2e test\nline two with pattern XYZ"

	callJSON(t, env.Client, "write_file", map[string]any{
		"session_id": sid,
		"path":       path,
		"content":    content,
		"mode":       "0644",
	})

	cat := callRaw(t, env.Client, "cat", map[string]any{"session_id": sid, "path": path})
	require.Contains(t, cat, "hello from e2e")

	grep := callRaw(t, env.Client, "grep", map[string]any{
		"session_id": sid,
		"path":       path,
		"pattern":    "XYZ",
	})
	require.Contains(t, grep, "XYZ")

	stat := callRaw(t, env.Client, "stat", map[string]any{"session_id": sid, "path": path})
	require.Contains(t, stat, "gooners-e2e-test.txt")
}

func TestE2E_FS_LsFind(t *testing.T) {
	env := setupMCPEnv(t)
	sid := env.open(t)

	dir := "/tmp/gooners-e2e-dir"
	callRaw(t, env.Client, "ssh_exec", map[string]any{
		"session_id": sid,
		"command":    "mkdir -p " + dir,
	})

	callJSON(t, env.Client, "write_file", map[string]any{
		"session_id": sid,
		"path":       dir + "/a.txt",
		"content":    "aaa",
	})

	ls := callRaw(t, env.Client, "ls", map[string]any{"session_id": sid, "path": dir})
	require.Contains(t, ls, "a.txt")

	find := callRaw(t, env.Client, "find", map[string]any{
		"session_id": sid,
		"path":       dir,
		"name":       "*.txt",
	})
	require.Contains(t, find, "a.txt")
}

func TestE2E_FS_Truncate(t *testing.T) {
	env := setupMCPEnv(t)
	sid := env.open(t)

	path := "/tmp/gooners-truncate.txt"
	callJSON(t, env.Client, "write_file", map[string]any{
		"session_id": sid,
		"path":       path,
		"content":    "0123456789",
	})

	callJSON(t, env.Client, "truncate", map[string]any{
		"session_id": sid,
		"path":       path,
		"size":       5,
	})

	cat := callRaw(t, env.Client, "cat", map[string]any{"session_id": sid, "path": path})
	require.Equal(t, "01234", strings.TrimSpace(cat))
}

func TestE2E_FS_UploadAndStatus(t *testing.T) {
	env := setupMCPEnv(t)
	sid := env.open(t)

	// create local file inside upload root (the one registered for security)
	local := filepath.Join(env.UploadRoot, "gooners-upload-src.txt")
	require.NoError(t, os.WriteFile(local, []byte("upload content for e2e test 1234567890"), 0o644))
	// no manual Remove needed: file lives under UploadRoot which is cleaned when process exits

	remote := "/tmp/gooners-uploaded.txt"

	up := callJSON(t, env.Client, "upload_file", map[string]any{
		"session_id":  sid,
		"local_path":  local,
		"remote_path": remote,
	})
	upid, _ := up["upload_id"].(string)
	require.NotEmpty(t, upid)

	// poll status until done (real upload is async)
	require.Eventually(t,
		func() bool {
			st := callJSON(t, env.Client, "upload_status", map[string]any{
				"session_id": sid,
				"upload_id":  upid,
			})
			d, _ := st["done"].(bool)
			return d
		},
		5*time.Second,
		50*time.Millisecond,
		"upload should complete",
	)

	// verify file landed
	cat := callRaw(t, env.Client, "cat", map[string]any{"session_id": sid, "path": remote})
	require.Contains(t, cat, "upload content")
}

func TestE2E_FS_UploadDownloadCompare(t *testing.T) {
	env := setupMCPEnv(t)
	sid := env.open(t)

	// create local file inside upload root (the one registered for security)
	localUp := filepath.Join(env.UploadRoot, "gooners-upload-src-compare.txt")
	content := []byte("upload download compare test content 1234567890")
	require.NoError(t, os.WriteFile(localUp, content, 0o644))

	remote := "/tmp/gooners-compare.txt"

	up := callJSON(t, env.Client, "upload_file", map[string]any{
		"session_id":  sid,
		"local_path":  localUp,
		"remote_path": remote,
	})
	upid, _ := up["upload_id"].(string)
	require.NotEmpty(t, upid)

	require.Eventually(t,
		func() bool {
			st := callJSON(t, env.Client, "upload_status", map[string]any{
				"session_id": sid,
				"upload_id":  upid,
			})
			d, _ := st["done"].(bool)
			return d
		},
		5*time.Second,
		50*time.Millisecond,
		"upload should complete",
	)

	localDown := filepath.Join(env.UploadRoot, "gooners-download-dst.txt")

	down := callJSON(t, env.Client, "download_file", map[string]any{
		"session_id":  sid,
		"remote_path": remote,
		"local_path":  localDown,
	})
	downid, _ := down["download_id"].(string)
	require.NotEmpty(t, downid)

	require.Eventually(t,
		func() bool {
			st := callJSON(t, env.Client, "download_status", map[string]any{
				"session_id":  sid,
				"download_id": downid,
			})
			d, _ := st["done"].(bool)
			return d
		},
		5*time.Second,
		50*time.Millisecond,
		"download should complete",
	)

	downloaded, err := os.ReadFile(localDown)
	require.NoError(t, err)
	require.Equal(t, content, downloaded)
}

func TestE2E_Proc_ListInfoKill(t *testing.T) {
	env := setupMCPEnv(t)
	sid := env.open(t)

	// Start sleep in background. Use a small delay + pgrep (from procps-ng which we
	// install in the container) to reliably grab the PID. $! can be tricky over ssh_exec.
	pidOut := callRaw(t, env.Client, "ssh_exec", map[string]any{
		"session_id": sid,
		"command":    "sleep 300 </dev/null >/dev/null 2>&1 & sleep 0.2; pgrep -n -f 'sleep 300' || echo NO_PID",
	})
	sleepPID := strings.TrimSpace(pidOut)
	require.NotEqual(t, "NO_PID", sleepPID, "failed to find background sleep process, got: %s", pidOut)
	require.NotEmpty(t, sleepPID, "empty PID from pgrep")

	// Also verify it shows up in proc_list (good sanity check)
	ps := callRaw(t, env.Client, "proc_list", map[string]any{"session_id": sid})
	require.Contains(t, ps, "sleep 300")

	// Give the background process a moment to fully appear in /proc (avoids
	// transient "no such process" between ps and the detailed info/lsof calls).
	time.Sleep(150 * time.Millisecond)

	// proc_info (can legitimately return IsError with useful text on transient /proc issues)
	info := callRawTolerant(t, env.Client, "proc_info", map[string]any{"session_id": sid, "pid": sleepPID})
	require.Contains(t, info, "sleep")

	// lsof (or fallback ls /proc) — same, tolerate error results
	lsof := callRawTolerant(t, env.Client, "proc_lsof", map[string]any{"session_id": sid, "pid": sleepPID})
	// may be empty or have fds, just not crash
	_ = lsof

	// Kill it.
	callRaw(t, env.Client, "proc_kill", map[string]any{"session_id": sid, "pid": sleepPID, "signal": "TERM"})

	// should be gone soon (poll instead of fixed sleep to avoid flakiness)
	require.Eventually(t,
		func() bool {
			ps2 := callRaw(t, env.Client, "proc_list", map[string]any{"session_id": sid})
			return !strings.Contains(ps2, sleepPID)
		},
		5*time.Second,
		100*time.Millisecond,
		"sleep process %s should disappear after kill", sleepPID,
	)
}

func TestE2E_Sysinfo_All(t *testing.T) {
	env := setupMCPEnv(t)
	sid := env.open(t)

	osinfo := callRaw(t, env.Client, "sys_os_info", map[string]any{"session_id": sid})
	require.Contains(t, osinfo, "Linux") // uname or hostname

	up := callRaw(t, env.Client, "sys_uptime", map[string]any{"session_id": sid})
	require.Contains(t, up, "load average") // typical uptime output

	mem := callRaw(t, env.Client, "sys_mem", map[string]any{"session_id": sid})
	require.Contains(t, mem, "Mem:") // free -h

	netAddrs := callRaw(t, env.Client, "sys_net_addrs", map[string]any{"session_id": sid})
	require.Contains(t, netAddrs, "lo") // loopback or eth
}

func TestE2E_Disk_All(t *testing.T) {
	env := setupMCPEnv(t)
	sid := env.open(t)

	df := callRaw(t, env.Client, "disk_df", map[string]any{"session_id": sid})
	require.Contains(t, df, "/") // root fs in df

	lsblk := callRaw(t, env.Client, "disk_lsblk", map[string]any{"session_id": sid})
	// lsblk may show loop/ devices; just ensure it didn't fail hard
	require.NotEmpty(t, lsblk)
}

func TestE2E_Core_Additional(t *testing.T) {
	env := setupMCPEnv(t)
	populateKnownHosts(t, env.HomeDir, env)

	// 1. Generate an ED25519 key for key-based auth testing.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	pubBytes := ssh.MarshalAuthorizedKey(signer.PublicKey())

	// PEM marshal the private key
	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	privBlock := &pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: privBytes,
	}
	privPEM := pem.EncodeToMemory(privBlock)

	sshDir := filepath.Join(env.HomeDir, ".ssh")
	require.NoError(t, os.WriteFile(filepath.Join(sshDir, "id_ed25519"), privPEM, 0o600))

	// Open a session with password to configure authorized_keys in the container
	sid := env.open(t)
	cmd := fmt.Sprintf("mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo '%s' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys", strings.TrimSpace(string(pubBytes)))
	callRaw(t, env.Client, "ssh_exec", map[string]any{
		"session_id": sid,
		"command":    cmd,
	})

	// Configure ~/.ssh/config
	host, port, err := net.SplitHostPort(env.Addr)
	require.NoError(t, err)
	sshConfigContent := fmt.Sprintf(
		"Host test-once-machine\n"+
			"  HostName %s\n"+
			"  Port %s\n"+
			"  User %s\n"+
			"  StrictHostKeyChecking no\n"+
			"  UserKnownHostsFile /dev/null\n",
		host, port, env.User,
	)
	require.NoError(t, os.WriteFile(filepath.Join(sshDir, "config"), []byte(sshConfigContent), 0o600))

	// Now we can test ssh_once_exec! (onceHandler)
	onceRes := callRaw(t, env.Client, "ssh_once_exec", map[string]any{
		"machine": "test-once-machine",
		"command": "echo once-success",
	})
	require.Contains(t, onceRes, "once-success")

	// We can test ssh_open (openHandler) using our key-based configuration.
	openRes := callJSON(t, env.Client, "ssh_open", map[string]any{
		"machine": "test-once-machine",
	})
	sidOnce, _ := openRes["session_id"].(string)
	require.NotEmpty(t, sidOnce)
	t.Cleanup(func() {
		_, _ = env.Client.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "ssh_close",
			Arguments: map[string]any{"session_id": sidOnce},
		})
	})

	// Test ssh_ping (pingHandler)
	pingRes := callJSON(t, env.Client, "ssh_ping", map[string]any{
		"session_id": sid,
	})
	require.Equal(t, "ok", pingRes["status"])
	require.NotEmpty(t, pingRes["time"])

	// Test output spooling (ssh_read_output & ssh_save_output)
	// We run a command that outputs more than the default 8KB threshold.
	// Since 10,000 characters is > 8,192 bytes, this will trigger truncation.
	largeOutputCmd := "printf 'a%.0s' {1..10000} && echo"
	execRes, err := env.Client.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "ssh_exec",
		Arguments: map[string]any{
			"session_id": sid,
			"command":    largeOutputCmd,
		},
	})
	require.NoError(t, err)
	var execM map[string]any
	tc, ok := execRes.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &execM))

	spoolID, _ := execM["stdout_spool_id"].(string)
	require.NotEmpty(t, spoolID, "Expected stdout to be truncated and spooled")

	// Read spool output (readOutputHandler)
	readResHead := callJSON(t, env.Client, "ssh_read_output", map[string]any{
		"session_id": sid,
		"spool_id":   spoolID,
		"lines":      10,
		"from_end":   false,
	})
	require.Contains(t, readResHead["text"].(string), "aaaa")

	readResTail := callJSON(t, env.Client, "ssh_read_output", map[string]any{
		"session_id": sid,
		"spool_id":   spoolID,
		"lines":      10,
		"from_end":   true,
	})
	require.Contains(t, readResTail["text"].(string), "aaaa")

	// Save spool output (saveOutputHandler)
	localSavePath := filepath.Join(env.UploadRoot, "saved_spool.txt")
	saveRes := callJSON(t, env.Client, "ssh_save_output", map[string]any{
		"session_id": sid,
		"spool_id":   spoolID,
		"local_path": localSavePath,
	})
	require.True(t, saveRes["ok"].(bool))

	// Verify file saved locally
	savedData, err := os.ReadFile(localSavePath)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(savedData), 10000)

	// Now try to read it again, it should fail because ssh_save_output deletes/consumes the spool.
	readResConsumed, err := env.Client.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "ssh_read_output",
		Arguments: map[string]any{
			"session_id": sid,
			"spool_id":   spoolID,
		},
	})
	require.NoError(t, err)
	require.True(t, readResConsumed.IsError, "Expected error trying to read consumed spool")
}

func TestE2E_Core_OpenHandler_PasswordProvider(t *testing.T) {
	env := setupMCPEnv(t)

	// Isolated home dir for this test's pool — keeps SSH config and known_hosts
	// separate from the shared pool and from the developer's real ~/.ssh.
	pwdHome := t.TempDir()
	populateKnownHosts(t, pwdHome, env)

	host, port, err := net.SplitHostPort(env.Addr)
	require.NoError(t, err)
	sshDir := filepath.Join(pwdHome, ".ssh")
	sshConfigContent := fmt.Sprintf(
		"Host test-pwd-machine\n"+
			"  HostName %s\n"+
			"  Port %s\n"+
			"  User %s\n",
		host, port, env.User,
	)
	require.NoError(t, os.WriteFile(filepath.Join(sshDir, "config"), []byte(sshConfigContent), 0o600))

	// Create a custom MCP server for this test to register core tools with a PasswordProvider.
	ctx := t.Context()
	p := session.NewPool(session.PoolOptions{CommandTimeout: 0, HomeDir: pwdHome})
	go p.RunLoop(ctx)

	s := mcp.NewServer(&mcp.Implementation{Name: "ssh-mcp-pwd-test", Version: "test"}, nil)

	mockProvider := &mockPasswordProvider{password: env.Pass}
	core.Register(s, p, core.RegisterOptions{
		Passwords: mockProvider,
	})

	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	require.NoError(t, err)
	defer ss.Close()

	c := mcp.NewClient(&mcp.Implementation{Name: "pwd-client", Version: "0"}, nil)
	cs, err := c.Connect(ctx, ct, nil)
	require.NoError(t, err)
	defer cs.Close()

	// Call ssh_open with machine: "test-pwd-machine"
	// It should query the password provider, get "secret", and connect successfully!
	openRes, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ssh_open",
		Arguments: map[string]any{"machine": "test-pwd-machine"},
	})
	require.NoError(t, err)
	require.False(t, openRes.IsError)

	var resM map[string]any
	tc, ok := openRes.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &resM))
	sid, _ := resM["session_id"].(string)
	require.NotEmpty(t, sid)
	defer func() {
		_, _ = cs.CallTool(ctx, &mcp.CallToolParams{
			Name:      "ssh_close",
			Arguments: map[string]any{"session_id": sid},
		})
	}()

	// Test ssh_sudo_exec resolving sudo password via provider (sudo_password is empty)
	sudoRes, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "ssh_sudo_exec",
		Arguments: map[string]any{
			"session_id": sid,
			"command":    "whoami",
		},
	})
	require.NoError(t, err)
	require.False(t, sudoRes.IsError)
	tcSudo, ok := sudoRes.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	require.Contains(t, tcSudo.Text, "root")
}

type mockPasswordProvider struct {
	password string
}

func (m *mockPasswordProvider) Password(ctx context.Context, machine string) (string, error) {
	return m.password, nil
}

// populateKnownHosts writes ~/.ssh/known_hosts into homeDir with entries for
// the test-once-machine, test-pwd-machine, and raw addr aliases, all pointing
// at the same container host key.
func populateKnownHosts(t *testing.T, homeDir string, env *testEnv) {
	t.Helper()
	var serverKey ssh.PublicKey
	probeClient, err := ssh.Dial("tcp", env.Addr, &ssh.ClientConfig{
		User: env.User,
		Auth: []ssh.AuthMethod{ssh.Password(env.Pass)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			serverKey = key
			return nil
		},
		Timeout: 5 * time.Second,
	})
	require.NoError(t, err)
	_ = probeClient.Close()

	sshDir := filepath.Join(homeDir, ".ssh")
	require.NoError(t, os.MkdirAll(sshDir, 0o700))

	host, port, err := net.SplitHostPort(env.Addr)
	require.NoError(t, err)

	keyLine := string(ssh.MarshalAuthorizedKey(serverKey))
	entryOnce := fmt.Sprintf("[%s]:%s", "test-once-machine", port)
	entryPwd := fmt.Sprintf("[%s]:%s", "test-pwd-machine", port)
	entryAddr := fmt.Sprintf("[%s]:%s", host, port)

	knownHostsLines := fmt.Sprintf(
		"%s %s"+
			"%s %s"+
			"%s %s",
		entryOnce, keyLine,
		entryPwd, keyLine,
		entryAddr, keyLine,
	)
	require.NoError(t, os.WriteFile(filepath.Join(sshDir, "known_hosts"), []byte(knownHostsLines), 0o600))
}
