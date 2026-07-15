package e2e_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kballard/go-shellquote"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/sandbox"
	"github.com/go-faster/gooners/internal/sandbox/docker"
	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/tools/core"
	sandboxtools "github.com/go-faster/gooners/internal/tools/sandbox"
)

// sandboxTestEnv wires a full sandbox-mcp server (real Docker Runner, real
// Manager, the exact tool subset cmd/sandbox-mcp registers) connected to an
// MCP client over an in-memory transport - mirroring mcp_e2e_test.go's
// pattern, but for sandbox-mcp instead of ssh-mcp.
type sandboxTestEnv struct {
	Client *mcp.ClientSession
	Server *mcp.ServerSession
	Runner *docker.Runner
}

// newSandboxEnv builds a fresh environment per test (not shared): a sandbox
// Manager backed by a real Docker daemon creates and destroys real
// containers, so isolation between tests matters more than the setup cost
// of one throwaway Docker client per test.
func newSandboxEnv(t *testing.T) *sandboxTestEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping E2E tests in short mode")
	}
	if runtime.GOOS != "linux" {
		// CI's macOS/Windows Docker daemons (when present) either don't
		// exist or only serve Windows containers, so pulling a Linux sandbox
		// image fails outright rather than via a reachability error.
		t.Skipf("skipping Docker E2E test: no Linux Docker daemon on %s CI runners", runtime.GOOS)
	}

	agentDir := t.TempDir()
	buildSandboxAgentBinary(t, agentDir, runtime.GOARCH)

	policy := sandbox.Policy{
		DefaultImage:    "alpine:latest",
		AllowedImages:   []string{"alpine:latest"},
		AllowedNetworks: []sandbox.Network{sandbox.NetworkNone},
		DropCaps:        []string{"ALL"},
		NoNewPrivileges: true,
		MemoryBytes:     256 * 1024 * 1024,
		CPUs:            1,
		PidsLimit:       128,
		IdleTimeout:     time.Minute,
		Deployment:      "sandbox-e2e-" + t.Name(),
	}

	runner, err := docker.New(docker.Options{
		Policy:   policy,
		AgentDir: agentDir,
		Logger:   slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)

	ctx := context.Background()
	if _, err := runner.List(ctx, sandbox.Filter{}); err != nil {
		_ = runner.Close()
		t.Skipf("skipping sandbox E2E: Docker daemon not reachable: %v", err)
	}

	pool := session.NewPool(session.PoolOptions{Logger: slog.New(slog.DiscardHandler)})
	runCtx, runCancel := context.WithCancel(ctx)
	go pool.RunLoop(runCtx)

	manager := sandbox.NewManager(sandbox.ManagerOptions{
		Runner: runner,
		Pool:   pool,
		Policy: policy,
		Logger: slog.New(slog.DiscardHandler),
	})
	go manager.RunLoop(runCtx)

	// Mirror cmd/sandbox-mcp/main.go's registerTools exactly: this is the
	// real tool surface the binary exposes, not a stale superset of it.
	s := mcp.NewServer(&mcp.Implementation{Name: "sandbox-mcp-e2e", Version: "test"}, nil)
	sandboxtools.Register(s, manager)
	core.RegisterClose(s, pool)
	core.RegisterExec(s, pool, slog.New(slog.DiscardHandler))
	core.RegisterSudoExec(s, pool, nil, slog.New(slog.DiscardHandler))
	core.RegisterPing(s, pool)
	core.RegisterReadOutput(s, pool)

	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	require.NoError(t, err)

	c := mcp.NewClient(&mcp.Implementation{Name: "sandbox-e2e-client", Version: "0"}, nil)
	cs, err := c.Connect(ctx, ct, nil)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Close()
		runCancel()
		_ = runner.Close()
	})

	return &sandboxTestEnv{Client: cs, Server: ss, Runner: runner}
}

// buildSandboxAgentBinary compiles cmd/sandbox-agent for GOOS=linux,
// GOARCH=arch into <dir>/<arch>/sandbox-agent - the layout
// docker.Options.AgentDir expects.
func buildSandboxAgentBinary(t *testing.T, dir, arch string) {
	t.Helper()
	dest := filepath.Join(dir, arch, "sandbox-agent")
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))

	cmd := exec.Command("go", "build", "-o", dest, "github.com/go-faster/gooners/cmd/sandbox-agent")
	// GOFLAGS may carry -race from the outer `go test -race` invocation;
	// clear it since -race requires cgo and this is a CGO_ENABLED=0 cross-build.
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=", "GOOS=linux", "GOARCH="+arch)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "building sandbox-agent: %s", out)
}

func TestE2E_Sandbox_OpenExecWriteCatClose(t *testing.T) {
	env := newSandboxEnv(t)

	openRes := callJSON(t, env.Client, "sandbox_open", map[string]any{})
	sid, _ := openRes["session_id"].(string)
	require.NotEmpty(t, sid)
	require.Equal(t, "alpine:latest", openRes["image"])
	require.Equal(t, "none", openRes["network"])

	closed := false
	t.Cleanup(func() {
		if closed {
			return
		}
		_, _ = env.Client.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "sandbox_close",
			Arguments: map[string]any{"session_id": sid},
		})
	})

	// The returned session_id must work with every other SSH tool, exactly
	// like ssh_open's - this is the whole point of building sandbox-mcp on
	// top of the existing session Pool and tool registrations.
	out := callRaw(t, env.Client, "ssh_exec", map[string]any{
		"session_id": sid,
		"command":    "echo hello-from-sandbox",
	})
	require.Contains(t, out, "hello-from-sandbox")

	// sandbox-mcp does not register write_file/cat (fs.Register is
	// deliberately excluded, see cmd/sandbox-mcp/main.go's registerTools),
	// so prove the same write/read round trip through ssh_exec instead.
	const path = "/tmp/sandbox-e2e.txt"
	const content = "sandbox e2e write/cat round trip"
	callRaw(t, env.Client, "ssh_exec", map[string]any{
		"session_id": sid,
		"command":    fmt.Sprintf("echo %s > %s", shellquote.Join(content), path),
	})
	cat := callRaw(t, env.Client, "ssh_exec", map[string]any{
		"session_id": sid,
		"command":    "cat " + path,
	})
	require.Contains(t, cat, content)

	// network: none holds even through the MCP tool surface.
	netOut := callRaw(t, env.Client, "ssh_exec", map[string]any{
		"session_id": sid,
		"command":    "cat /proc/net/dev",
	})
	require.Contains(t, netOut, "lo")

	closeRes := callJSON(t, env.Client, "sandbox_close", map[string]any{"session_id": sid})
	require.Equal(t, true, closeRes["ok"])
	closed = true

	require.Eventually(t, func() bool {
		list, err := env.Runner.List(context.Background(), sandbox.Filter{})
		if err != nil {
			return false
		}
		return len(list) == 0
	}, 30*time.Second, 200*time.Millisecond, "container must be gone after sandbox_close")
}

func TestE2E_Sandbox_ConcurrentSandboxesAreIsolated(t *testing.T) {
	env := newSandboxEnv(t)

	openA := callJSON(t, env.Client, "sandbox_open", map[string]any{})
	sidA, _ := openA["session_id"].(string)
	require.NotEmpty(t, sidA)
	t.Cleanup(func() {
		_, _ = env.Client.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "sandbox_close",
			Arguments: map[string]any{"session_id": sidA},
		})
	})

	openB := callJSON(t, env.Client, "sandbox_open", map[string]any{})
	sidB, _ := openB["session_id"].(string)
	require.NotEmpty(t, sidB)
	t.Cleanup(func() {
		_, _ = env.Client.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "sandbox_close",
			Arguments: map[string]any{"session_id": sidB},
		})
	})

	require.NotEqual(t, sidA, sidB)

	// sandbox-mcp does not register write_file/cat (fs.Register is
	// deliberately excluded, see cmd/sandbox-mcp/main.go's registerTools),
	// so prove the same isolation property through ssh_exec instead.
	const path = "/tmp/isolation-marker.txt"
	const marker = "only visible in sandbox A"
	callRaw(t, env.Client, "ssh_exec", map[string]any{
		"session_id": sidA,
		"command":    fmt.Sprintf("echo %s > %s", shellquote.Join(marker), path),
	})

	// The same path must not exist in sandbox B: two sandbox_open calls must
	// never share a container. `cat` on a missing file exits non-zero but
	// ssh_exec only reports IsError for a transport/setup failure, not a
	// non-zero exit code, so assert absence from the command's own output.
	existsB := callRaw(t, env.Client, "ssh_exec", map[string]any{
		"session_id": sidB,
		"command":    fmt.Sprintf("test -f %s && echo FOUND || echo MISSING", path),
	})
	require.Contains(t, existsB, "MISSING", "the file written in sandbox A must not be visible in sandbox B")

	// Sandbox A can still read its own file.
	catA := callRaw(t, env.Client, "ssh_exec", map[string]any{
		"session_id": sidA,
		"command":    "cat " + path,
	})
	require.Contains(t, catA, marker)
}

// callJSON and callRaw are defined in mcp_e2e_test.go (same package) and
// reused here as-is.
