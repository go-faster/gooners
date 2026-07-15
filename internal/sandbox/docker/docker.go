// Package docker implements internal/sandbox.Runner on top of the Docker
// Engine API: each sandbox is a container whose stdio (via `docker exec`,
// not the container's PID 1) carries the sandbox agent's SSH traffic.
package docker

import (
	"log/slog"
	"sync"

	"github.com/google/uuid"
	"github.com/moby/moby/client"

	"github.com/go-faster/errors"

	"github.com/go-faster/gooners/internal/sandbox"
)

// Options configures a [Runner].
type Options struct {
	// Client is used as-is if set (e.g. for tests). Otherwise New builds one
	// from Host (or the daemon's usual environment defaults).
	Client *client.Client
	// Host is the Docker daemon endpoint (e.g. "unix:///var/run/docker.sock"
	// or "tcp://host:2375"). Only consulted when Client is nil; empty uses
	// the client library's own environment-variable defaults (DOCKER_HOST).
	Host string
	// Policy is the operator's sandbox policy: hardening (CapDrop,
	// NoNewPrivileges, resource limits, RuntimeHandler, User) is read from
	// here, never from a caller's Spec. Callers must have already run it
	// through [sandbox.Policy.Validate] at least once (e.g. a startup
	// warm-up) so its defaults are filled in - Runner has no business
	// fabricating a Spec just to trigger that itself.
	Policy sandbox.Policy
	// AgentDir is the base directory containing per-architecture
	// sandbox-agent binaries; see [resolveAgentBinary]. Defaults to
	// [DefaultAgentDir].
	AgentDir string
	// Instance scopes container labels for the startup orphan sweep. It
	// must be the exact same value passed to
	// sandbox.ManagerOptions.Instance. Defaults to a fresh random value.
	Instance string
	Logger   *slog.Logger
}

func (o *Options) setDefaults() {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Instance == "" {
		o.Instance = uuid.NewString()
	}
}

// Runner implements [sandbox.Runner] on Docker.
type Runner struct {
	cli     *client.Client
	ownsCli bool

	policy   sandbox.Policy
	agentDir string
	instance string
	logger   *slog.Logger

	// createMu serializes the create→copy→start window of concurrent
	// sandbox creations. Some rootless-overlayfs daemons race their
	// snapshotter when a container Start lands while another container
	// sharing the same image layers is starting or being torn down,
	// returning "device or resource busy". Container create is fast and the
	// expensive per-sandbox work (the SSH handshake in Dial) stays fully
	// concurrent, so serializing just this window is cheap and makes
	// concurrent sandbox_open deterministic. The retry in Create remains as
	// a backstop for the cross-process case one mutex cannot cover.
	createMu sync.Mutex
}

var _ sandbox.Runner = (*Runner)(nil)

// New builds a Runner. If opts.Client is nil, it dials the daemon from
// opts.Host (or the environment) and negotiates the API version.
func New(opts Options) (*Runner, error) {
	opts.setDefaults()

	cli := opts.Client
	ownsCli := false
	if cli == nil {
		// API version negotiation is on by default since this client
		// version; no separate opt-in is needed anymore.
		clientOpts := []client.Opt{client.FromEnv}
		if opts.Host != "" {
			clientOpts = append(clientOpts, client.WithHost(opts.Host))
		}
		var err error
		cli, err = client.New(clientOpts...)
		if err != nil {
			return nil, errors.Wrap(err, "create docker client")
		}
		ownsCli = true
	}

	return &Runner{
		cli:      cli,
		ownsCli:  ownsCli,
		policy:   opts.Policy,
		agentDir: opts.AgentDir,
		instance: opts.Instance,
		logger:   opts.Logger,
	}, nil
}

// Close releases the underlying Docker client, if this Runner created it
// itself (an injected opts.Client is the caller's to close).
func (r *Runner) Close() error {
	if !r.ownsCli {
		return nil
	}
	return r.cli.Close()
}

// labels returns the full label set a newly-created container gets. See
// sandbox.Label{Sandbox,Deployment,Instance,Session} for what each one is
// for and why LabelSession is only a pre-adoption correlation id.
func (r *Runner) labels() map[string]string {
	return map[string]string{
		sandbox.LabelSandbox:    "true",
		sandbox.LabelDeployment: r.policy.Deployment,
		sandbox.LabelInstance:   r.instance,
		sandbox.LabelSession:    uuid.NewString(),
	}
}
