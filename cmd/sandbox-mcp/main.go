// Package main is the entrypoint for sandbox-mcp: like ssh-mcp, but every
// session gets a fresh, isolated container instead of a static SSH host.
// See internal/sandbox for the lifecycle and internal/tools/sandbox for the
// sandbox_open/sandbox_close tools.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/gooners/internal/cmdutil"
	"github.com/go-faster/gooners/internal/mcputil"
	"github.com/go-faster/gooners/internal/sandbox"
	"github.com/go-faster/gooners/internal/sandbox/docker"
	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/tools/core"
	sandboxtools "github.com/go-faster/gooners/internal/tools/sandbox"
)

func main() {
	var (
		logging   cmdutil.LoggingFlags
		transport cmdutil.TransportFlags
	)
	logging.Register(flag.CommandLine)
	transport.Register(flag.CommandLine)

	var (
		disableSudo    = flag.Bool("disable-sudo", false, "do not register the ssh_sudo_exec tool")
		commandTimeout = flag.Duration("command-timeout", 10*time.Second, "default command timeout")

		sandboxImage         = flag.String("sandbox-image", "alpine:latest", "default sandbox image, used when sandbox_open does not request one")
		sandboxAllowedImages = flag.String("sandbox-allowed-images", "", "comma-separated path.Match glob patterns of images sandbox_open may request; empty allows only -sandbox-image")
		sandboxNetwork       = flag.String("sandbox-network", "none", "comma-separated network tiers sandbox_open may request: none, open, egress-proxy (egress-proxy is not implemented yet)")
		sandboxMemory        = flag.Int64("sandbox-memory", 512*1024*1024, "memory limit per sandbox, in bytes")
		sandboxCPUs          = flag.Float64("sandbox-cpus", 1, "CPU limit per sandbox")
		sandboxPidsLimit     = flag.Int64("sandbox-pids-limit", 256, "pids limit per sandbox")
		sandboxRuntime       = flag.String("sandbox-runtime", "", "alternative container runtime (e.g. runsc for gVisor, kata for Kata Containers); empty uses the daemon's default")
		sandboxUser          = flag.String("sandbox-user", "", "user the sandboxed process runs as; empty uses the image's default")
		sandboxIdleTimeout   = flag.Duration("sandbox-idle-timeout", 15*time.Minute, "how long a sandbox may sit with no SSH activity before it is torn down")
		sandboxAgentPath     = flag.String("sandbox-agent-path", docker.DefaultAgentDir, "base directory containing per-architecture sandbox-agent binaries, laid out as <dir>/<arch>/sandbox-agent")
		sandboxDeployment    = flag.String("sandbox-deployment", "", "deployment name scoping sandbox labels; defaults to the hostname")
		dockerHost           = flag.String("docker-host", "", "Docker daemon endpoint (e.g. unix:///var/run/docker.sock); empty uses the environment default (DOCKER_HOST)")
	)
	flag.Parse()

	cleanup, logger, err := logging.Setup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deployment := *sandboxDeployment
	if deployment == "" {
		if h, hostErr := os.Hostname(); hostErr == nil {
			deployment = h
		}
	}

	policy := sandbox.Policy{
		DefaultImage:    *sandboxImage,
		AllowedImages:   splitCommaList(*sandboxAllowedImages),
		AllowedNetworks: parseNetworks(*sandboxNetwork),
		MemoryBytes:     *sandboxMemory,
		CPUs:            *sandboxCPUs,
		PidsLimit:       *sandboxPidsLimit,
		RuntimeHandler:  *sandboxRuntime,
		User:            *sandboxUser,
		IdleTimeout:     *sandboxIdleTimeout,
		Deployment:      deployment,
	}
	// Warm up defaults once: Policy.Validate has a pointer receiver and
	// fills in defaults in place, so both the Runner and the Manager below
	// end up with an identically-defaulted Policy. This also catches an
	// inconsistent flag combination (e.g. -sandbox-image not covered by
	// -sandbox-allowed-images) at startup instead of on the first
	// sandbox_open call.
	if _, err := policy.Validate(sandbox.Spec{}); err != nil {
		logger.Error("invalid sandbox policy flags", "err", err)
		os.Exit(1)
	}

	// Shared by the Runner and the Manager: the startup orphan sweep only
	// works if both agree on which containers belong to "this process".
	instance := uuid.NewString()

	runner, err := docker.New(docker.Options{
		Host:     *dockerHost,
		Policy:   policy,
		AgentDir: *sandboxAgentPath,
		Instance: instance,
		Logger:   logger.With("component", "docker"),
	})
	if err != nil {
		logger.Error("failed to create docker runner", "err", err)
		os.Exit(1)
	}
	defer func() { _ = runner.Close() }()

	s := mcputil.NewServer(mcputil.ServerConfig{
		Name: "sandbox-mcp",
		Instructions: "You are connected to sandbox-mcp. Call sandbox_open to get a fresh, isolated " +
			"container and a session_id; use that session_id with ssh_exec and every other SSH tool " +
			"exactly as you would with ssh-mcp. Call sandbox_close when done.",
		Logger: logger.With("component", "mcp-sdk"),
	})

	pool := session.NewPool(session.PoolOptions{
		CommandTimeout: *commandTimeout,
		Logger:         logger,
		IdleTimeout:    policy.IdleTimeout,
		// No LocalFS: the pool's host filesystem provider defaults to denying
		// everything, so no tool in this process can read or write a host file,
		// whatever it is asked for. The tool subset below is the first line of
		// defense and this is the second — the one that holds if a future tool
		// is registered here by mistake.
		OnDisconnect: func(machine string, err error) {
			mcputil.BroadcastWarning(s, "sandbox-mcp", fmt.Sprintf("sandbox session to %s disconnected: %v", machine, err))
		},
	})

	manager := sandbox.NewManager(sandbox.ManagerOptions{
		Runner:    runner,
		Pool:      pool,
		Policy:    policy,
		AgentPath: *sandboxAgentPath,
		Instance:  instance,
		Logger:    logger.With("component", "sandbox-manager"),
	})

	logger.Debug("registering MCP tools")
	registerTools(s, pool, manager, *disableSudo, logger)
	logger.Info("MCP tools registered successfully", "disable_sudo", *disableSudo)

	// Run the background loops and the transport together so shutdown waits
	// for all three to actually finish tearing down instead of letting main
	// exit while pool/manager teardown is still in flight.
	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error { pool.RunLoop(gCtx); return nil })
	g.Go(func() error { manager.RunLoop(gCtx); return nil })
	g.Go(func() error {
		return transport.Run(gCtx, cmdutil.RunOptions{
			Name:   "sandbox-mcp",
			Server: s,
			Logger: logger.With("component", "transport"),
		})
	})
	if err := g.Wait(); err != nil {
		slog.Error("failed to run server", "err", err)
		os.Exit(1)
	}
}

// registerTools registers only the tool subset safe to expose from inside a
// sandbox. This is a security boundary, not a preference:
//
//   - NEVER core.RegisterOpen / RegisterOpenCfg / RegisterOnceExec: they
//     would let the agent SSH *out* of the sandbox to any host this process
//     can reach - a sandbox escape.
//   - NEVER core.RegisterList (ssh_list): it returns every session in the
//     process to every caller, leaking other conversations' capability
//     tokens and breaking the isolation model outright.
//   - NEVER core.RegisterListMachines or systemd.Register: meaningless
//     inside a container, and listing machines has no purpose here.
//   - NEVER fs.Register (ls/cat/grep/find/stat/du/write_file, upload_file,
//     download_file): the file tools are redundant with ssh_exec against a
//     disposable full container, and upload_file/download_file's "local
//     path" is on the sandbox-mcp host process, not inside the container -
//     since every sandbox shares one uploadRoot directory, two unrelated
//     sandboxes could read/write into the same host directory, a covert
//     channel between sandboxes that are supposed to be isolated from each
//     other.
//   - NOT registered (pure surface reduction, no host-crossing risk):
//     proc.Register, disk.Register, sysinfo.Register. They're exec-based and
//     remote-scoped already, but the container is fully exec'able, so these
//     curated read-only wrappers add nothing a sandboxed ssh_exec can't do;
//     they exist for real hosts where an operator wants a fine-grained tool
//     subset (see ssh-mcp), not for a disposable per-session sandbox.
//   - NOT registered: core.RegisterSaveOutput (ssh_save_output). It exists to
//     persist spool output past a session's lifetime to a chosen host path,
//     but internal/session/pool_handlers.go's closeSession already removes
//     every spool file and the session's whole tempdir on every teardown
//     path (explicit close, idle sweep, and shutdown), so there is nothing
//     left to "save" that isn't already cleaned up automatically.
//     ssh_read_output (kept) already returns spool content as text directly.
//
// None of the above is load-bearing on its own any more. This process gives
// its session pool no LocalFS, so its host filesystem provider denies every
// read and write (internal/effect). A tool registered here by mistake gets an
// error, not the host's filesystem. Keep the list correct anyway: defense in
// depth means both, not either.
func registerTools(s *mcp.Server, pool *session.Pool, manager sandboxtools.Manager, disableSudo bool, logger *slog.Logger) {
	sandboxtools.Register(s, manager)

	core.RegisterClose(s, pool)
	core.RegisterExec(s, pool, logger)
	if !disableSudo {
		core.RegisterSudoExec(s, pool, nil, logger)
	}
	core.RegisterPing(s, pool)
	core.RegisterReadOutput(s, pool)
}

func splitCommaList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseNetworks(s string) []sandbox.Network {
	names := splitCommaList(s)
	out := make([]sandbox.Network, 0, len(names))
	for _, n := range names {
		out = append(out, sandbox.Network(n))
	}
	return out
}
