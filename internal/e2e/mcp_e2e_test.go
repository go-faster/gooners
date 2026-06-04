package e2e_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

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
	CS         *mcp.ClientSession
	Addr       string
	User       string
	Pass       string
	UploadRoot string
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
	os.Exit(m.Run())
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

		p := session.NewPool(session.PoolOptions{CommandTimeout: 0})
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
			CS:         cs,
			Addr:       addr,
			User:       user,
			Pass:       password,
			UploadRoot: uploadRoot,
		}
		_ = ctrCleanup // container intentionally lives for the whole suite
		_ = ss         // keep server session alive
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
	data := callJSON(t, e.CS, "ssh_open_cfg", map[string]any{
		"machine":     e.Addr,
		"user":        e.User,
		"password":    e.Pass,
		"known_hosts": "insecure",
	})
	sid, _ := data["session_id"].(string)
	require.NotEmpty(t, sid)

	// Ensure the session is closed at end of this test (gives coverage for
	// ssh_close on every path that opens a session, and prevents leakage
	// across the shared fixture).
	t.Cleanup(func() {
		_, _ = e.CS.CallTool(context.Background(), &mcp.CallToolParams{
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
	listText := callRaw(t, env.CS, "ssh_list", map[string]any{})
	require.Contains(t, listText, sid)

	// close it
	callJSON(t, env.CS, "ssh_close", map[string]any{"session_id": sid})

	// list again, should not have it (or empty-ish)
	listText2 := callRaw(t, env.CS, "ssh_list", map[string]any{})
	require.NotContains(t, listText2, sid)
}

func TestE2E_Core_Exec(t *testing.T) {
	env := setupMCPEnv(t)
	sid := env.open(t)

	// exec simple cmd
	out := callRaw(t, env.CS, "ssh_exec", map[string]any{
		"session_id": sid,
		"command":    "echo hello-e2e",
	})
	require.Contains(t, out, "hello-e2e")
}

func TestE2E_Core_SudoExec(t *testing.T) {
	env := setupMCPEnv(t)
	sid := env.open(t)

	out := callRaw(t, env.CS, "ssh_sudo_exec", map[string]any{
		"session_id":    sid,
		"command":       "whoami",
		"sudo_password": env.Pass,
	})
	require.Contains(t, out, "root")
}

func TestE2E_Core_OnceExec(t *testing.T) {
	env := setupMCPEnv(t)

	res := callRawResult(t, env.CS, "ssh_once_exec", map[string]any{
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

	callJSON(t, env.CS, "write_file", map[string]any{
		"session_id": sid,
		"path":       path,
		"content":    content,
		"mode":       "0644",
	})

	cat := callRaw(t, env.CS, "cat", map[string]any{"session_id": sid, "path": path})
	require.Contains(t, cat, "hello from e2e")

	grep := callRaw(t, env.CS, "grep", map[string]any{
		"session_id": sid,
		"path":       path,
		"pattern":    "XYZ",
	})
	require.Contains(t, grep, "XYZ")

	stat := callRaw(t, env.CS, "stat", map[string]any{"session_id": sid, "path": path})
	require.Contains(t, stat, "gooners-e2e-test.txt")
}

func TestE2E_FS_LsFind(t *testing.T) {
	env := setupMCPEnv(t)
	sid := env.open(t)

	dir := "/tmp/gooners-e2e-dir"
	callRaw(t, env.CS, "ssh_exec", map[string]any{
		"session_id": sid,
		"command":    "mkdir -p " + dir,
	})

	callJSON(t, env.CS, "write_file", map[string]any{
		"session_id": sid,
		"path":       dir + "/a.txt",
		"content":    "aaa",
	})

	ls := callRaw(t, env.CS, "ls", map[string]any{"session_id": sid, "path": dir})
	require.Contains(t, ls, "a.txt")

	find := callRaw(t, env.CS, "find", map[string]any{
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
	callJSON(t, env.CS, "write_file", map[string]any{
		"session_id": sid,
		"path":       path,
		"content":    "0123456789",
	})

	callJSON(t, env.CS, "truncate", map[string]any{
		"session_id": sid,
		"path":       path,
		"size":       5,
	})

	cat := callRaw(t, env.CS, "cat", map[string]any{"session_id": sid, "path": path})
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

	up := callJSON(t, env.CS, "upload_file", map[string]any{
		"session_id":  sid,
		"local_path":  local,
		"remote_path": remote,
	})
	upid, _ := up["upload_id"].(string)
	require.NotEmpty(t, upid)

	// poll status until done (real upload is async)
	require.Eventually(t,
		func() bool {
			st := callJSON(t, env.CS, "upload_status", map[string]any{
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
	cat := callRaw(t, env.CS, "cat", map[string]any{"session_id": sid, "path": remote})
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

	up := callJSON(t, env.CS, "upload_file", map[string]any{
		"session_id":  sid,
		"local_path":  localUp,
		"remote_path": remote,
	})
	upid, _ := up["upload_id"].(string)
	require.NotEmpty(t, upid)

	require.Eventually(t,
		func() bool {
			st := callJSON(t, env.CS, "upload_status", map[string]any{
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

	down := callJSON(t, env.CS, "download_file", map[string]any{
		"session_id":  sid,
		"remote_path": remote,
		"local_path":  localDown,
	})
	downid, _ := down["download_id"].(string)
	require.NotEmpty(t, downid)

	require.Eventually(t,
		func() bool {
			st := callJSON(t, env.CS, "download_status", map[string]any{
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
	pidOut := callRaw(t, env.CS, "ssh_exec", map[string]any{
		"session_id": sid,
		"command":    "sleep 300 </dev/null >/dev/null 2>&1 & sleep 0.2; pgrep -n -f 'sleep 300' || echo NO_PID",
	})
	sleepPID := strings.TrimSpace(pidOut)
	require.NotEqual(t, "NO_PID", sleepPID, "failed to find background sleep process, got: %s", pidOut)
	require.NotEmpty(t, sleepPID, "empty PID from pgrep")

	// Also verify it shows up in proc_list (good sanity check)
	ps := callRaw(t, env.CS, "proc_list", map[string]any{"session_id": sid})
	require.Contains(t, ps, "sleep 300")

	// Give the background process a moment to fully appear in /proc (avoids
	// transient "no such process" between ps and the detailed info/lsof calls).
	time.Sleep(150 * time.Millisecond)

	// proc_info (can legitimately return IsError with useful text on transient /proc issues)
	info := callRawTolerant(t, env.CS, "proc_info", map[string]any{"session_id": sid, "pid": sleepPID})
	require.Contains(t, info, "sleep")

	// lsof (or fallback ls /proc) — same, tolerate error results
	lsof := callRawTolerant(t, env.CS, "proc_lsof", map[string]any{"session_id": sid, "pid": sleepPID})
	// may be empty or have fds, just not crash
	_ = lsof

	// Kill it.
	callRaw(t, env.CS, "proc_kill", map[string]any{"session_id": sid, "pid": sleepPID, "signal": "TERM"})

	// should be gone soon (poll instead of fixed sleep to avoid flakiness)
	require.Eventually(t,
		func() bool {
			ps2 := callRaw(t, env.CS, "proc_list", map[string]any{"session_id": sid})
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

	osinfo := callRaw(t, env.CS, "sys_os_info", map[string]any{"session_id": sid})
	require.Contains(t, osinfo, "Linux") // uname or hostname

	up := callRaw(t, env.CS, "sys_uptime", map[string]any{"session_id": sid})
	require.Contains(t, up, "load average") // typical uptime output

	mem := callRaw(t, env.CS, "sys_mem", map[string]any{"session_id": sid})
	require.Contains(t, mem, "Mem:") // free -h

	net := callRaw(t, env.CS, "sys_net_addrs", map[string]any{"session_id": sid})
	require.Contains(t, net, "lo") // loopback or eth
}

func TestE2E_Disk_All(t *testing.T) {
	env := setupMCPEnv(t)
	sid := env.open(t)

	df := callRaw(t, env.CS, "disk_df", map[string]any{"session_id": sid})
	require.Contains(t, df, "/") // root fs in df

	lsblk := callRaw(t, env.CS, "disk_lsblk", map[string]any{"session_id": sid})
	// lsblk may show loop/ devices; just ensure it didn't fail hard
	require.NotEmpty(t, lsblk)
}
