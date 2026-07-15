package sandbox

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/go-faster/errors"

	"github.com/go-faster/gooners/internal/sandbox/agent"
	"github.com/go-faster/gooners/internal/session"
)

// Label keys a Runner attaches to every sandbox it creates. Manager reads
// them back (via [Runner.List]) for the startup orphan sweep and reconcile;
// a Runner implementation (e.g. internal/sandbox/docker) writes them at
// container-create time.
const (
	// LabelSandbox marks every container this project ever created, so an
	// operator (or [Runner.List] with an empty [Filter]) can distinguish a
	// gooners sandbox from unrelated containers on the same host.
	LabelSandbox = "dev.gooners.sandbox"
	// LabelDeployment scopes sandboxes to one sandbox-mcp deployment
	// ([Policy.Deployment]): two deployments sharing one container host must
	// never reap each other's sandboxes.
	LabelDeployment = "dev.gooners.deployment"
	// LabelInstance scopes sandboxes to one sandbox-mcp process (a fresh
	// value each time the process starts). The startup orphan sweep destroys
	// same-deployment, different-instance sandboxes: containers a previous
	// run of this same deployment left behind.
	LabelInstance = "dev.gooners.instance"
	// LabelSession is a best-effort, human-debugging correlation id set by
	// the Runner at Create time. It is NOT the session Pool's capability
	// token: that token is only generated later by pool.Adopt, after Create
	// and Dial have already happened, so it cannot be known yet when the
	// Runner assembles container labels.
	LabelSession = "dev.gooners.session"
)

// reconcileInterval is how often RunLoop retries destroying sandboxes this
// Manager no longer considers live but that a Runner still reports existing
// (i.e. a previous Destroy call failed transiently). This is deliberately
// slow: the fast path for tearing down a sandbox is the session Pool's own
// idle sweep firing OnClose, not this loop.
const reconcileInterval = 5 * time.Minute

// destroyTimeout bounds every Destroy call the Manager makes on its own
// (destroy-on-error, OnClose, orphan sweep, reconcile) so a wedged container
// runtime can never hang the caller indefinitely.
const destroyTimeout = 30 * time.Second

// ManagerOptions configures a [Manager].
type ManagerOptions struct {
	Runner Runner
	Pool   *session.Pool
	Policy Policy
	// AgentPath is the same -sandbox-agent-path value used to construct the
	// Runner. Manager does not inject the agent binary itself - only a
	// concrete backend (e.g. internal/sandbox/docker) knows how to do that,
	// inside Runner.Create - so this field is purely informational, logged
	// at startup so an operator can tell which agent path a given
	// sandbox-mcp process is running with.
	AgentPath string
	// Instance scopes the startup orphan sweep and reconcile to sandboxes
	// created by this process. It must be the exact same value passed to the
	// Runner's own construction (e.g. docker.Options.Instance), since the
	// Runner - not the Manager - is what attaches [LabelInstance] to
	// containers it creates. Defaults to a fresh random value.
	Instance string
	Logger   *slog.Logger
}

func (o *ManagerOptions) setDefaults() {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Instance == "" {
		o.Instance = uuid.NewString()
	}
}

// Manager drives sandbox lifecycle on top of a [Runner]: it applies Policy,
// creates and dials a sandbox, performs the SSH handshake over the agent's
// exec stream, and adopts the result into the session Pool so every existing
// SSH tool (ssh_exec, fs/*, proc/*, ...) works against it unchanged.
type Manager struct {
	runner    Runner
	pool      *session.Pool
	policy    Policy
	agentPath string
	instance  string
	logger    *slog.Logger

	mu   sync.Mutex
	live map[string]struct{} // sandbox IDs Manager currently considers alive
}

// NewManager builds a Manager from opts.
func NewManager(opts ManagerOptions) *Manager {
	opts.setDefaults()
	m := &Manager{
		runner:    opts.Runner,
		pool:      opts.Pool,
		policy:    opts.Policy,
		agentPath: opts.AgentPath,
		instance:  opts.Instance,
		logger:    opts.Logger,
		live:      make(map[string]struct{}),
	}
	m.logger.Info("sandbox manager started",
		"instance", m.instance,
		"deployment", m.policy.Deployment,
		"agent_path", m.agentPath,
	)
	return m
}

// OpenResult is the result of [Manager.Open]: everything
// [session.OpenResult] carries (the pool's session_id, its display label,
// ...) plus the image and network tier Policy actually assigned - not
// necessarily what the caller requested, since Policy can default or
// override it.
type OpenResult struct {
	session.OpenResult
	Image   string
	Network Network
}

// Open validates spec against Policy, creates and dials a sandbox, and
// adopts the resulting SSH connection into the Pool.
//
// A failure anywhere after Create succeeds (Dial, the SSH handshake, or
// Adopt) destroys the just-created sandbox before returning: without this,
// every one of those failures would leak a container forever.
func (m *Manager) Open(ctx context.Context, spec Spec) (OpenResult, error) {
	validated, err := m.policy.Validate(spec)
	if err != nil {
		return OpenResult{}, err
	}

	sb, err := m.runner.Create(ctx, validated)
	if err != nil {
		return OpenResult{}, errors.Wrap(err, "create sandbox")
	}
	m.markLive(sb.ID)

	ok := false
	defer func() {
		if ok {
			return
		}
		m.destroyOnError(sb.ID)
	}()

	conn, err := m.runner.Dial(ctx, sb.ID)
	if err != nil {
		return OpenResult{}, errors.Wrap(err, "dial sandbox agent")
	}

	hostPub, hostKeyPEM, err := generateHostKey()
	if err != nil {
		_ = conn.Close()
		return OpenResult{}, errors.Wrap(err, "generate sandbox host key")
	}
	clientSigner, authorizedKey, err := generateClientKey()
	if err != nil {
		_ = conn.Close()
		return OpenResult{}, errors.Wrap(err, "generate sandbox client key")
	}

	preamble := agent.Preamble{
		Version:       1,
		HostKeyPEM:    hostKeyPEM,
		AuthorizedKey: authorizedKey,
		Workdir:       validated.Workdir,
	}
	if err := agent.WritePreamble(conn, preamble); err != nil {
		_ = conn.Close()
		return OpenResult{}, errors.Wrap(err, "write sandbox agent preamble")
	}

	// ssh.ClientConfig.Timeout only bounds ssh.Dial's own net.Dial - it does
	// nothing for ssh.NewClientConn, which takes an already-established
	// conn. Without an explicit deadline here, a wedged or dead sandbox
	// agent would hang Open forever.
	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout(ctx))); err != nil {
		_ = conn.Close()
		return OpenResult{}, errors.Wrap(err, "set sandbox handshake deadline")
	}

	machine := "sandbox/" + sb.ID
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, machine, &ssh.ClientConfig{
		User:            "sandbox",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientSigner)},
		HostKeyCallback: ssh.FixedHostKey(hostPub),
	})
	if err != nil {
		_ = conn.Close()
		return OpenResult{}, errors.Wrap(err, "ssh handshake with sandbox agent")
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		m.logger.Warn("sandbox: failed to clear post-handshake deadline", "id", sb.ID, "err", err)
	}
	sshClient := ssh.NewClient(clientConn, chans, reqs)

	adopted, err := m.pool.Adopt(ctx, session.AdoptRequest{
		Machine: machine,
		Client:  sshClient,
		// OnClose runs in its own goroutine, for any reason the pool session
		// closes: ssh_close, idle sweep, agent death, or shutdown. This is
		// the ONLY place a live sandbox's container is destroyed on the
		// happy path; Manager.Close must never also destroy here, or a
		// close-then-idle race would double-destroy.
		OnClose: func() {
			m.unmarkLive(sb.ID)
			destroyCtx, cancel := context.WithTimeout(context.Background(), destroyTimeout)
			defer cancel()
			if err := m.runner.Destroy(destroyCtx, sb.ID); err != nil {
				m.logger.Warn("sandbox: destroy on session close failed", "id", sb.ID, "err", err)
			}
		},
	})
	if err != nil {
		_ = sshClient.Close() // also closes conn
		return OpenResult{}, errors.Wrap(err, "adopt sandbox session")
	}

	ok = true
	return OpenResult{OpenResult: adopted, Image: sb.Image, Network: sb.Network}, nil
}

// Close closes sessionID's pool session. That alone tears down the sandbox:
// the pool's own teardown fires the OnClose callback registered in Open,
// which destroys the container. Close must never also call Destroy - that
// would double-destroy.
func (m *Manager) Close(ctx context.Context, sessionID string) error {
	return m.pool.Close(ctx, sessionID)
}

// RunLoop performs a one-time startup orphan sweep, then periodically
// reconciles, until ctx is done.
//
// This is deliberately independent of session idle expiry:
// [session.PoolOptions.IdleTimeout] already closes idle sessions, which
// fires OnClose and destroys their container - one idle timer, not two.
func (m *Manager) RunLoop(ctx context.Context) {
	m.sweepOrphans(ctx)

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcile(ctx)
		}
	}
}

// sweepOrphans destroys every sandbox in this deployment left behind by a
// different instance (e.g. a previous process that crashed, or restarted,
// before its own containers were torn down). Scoping by deployment - not
// touching other deployments' sandboxes - is what stops two sandbox-mcp
// processes sharing one container host from destroying each other's
// sandboxes.
func (m *Manager) sweepOrphans(ctx context.Context) {
	sandboxes, err := m.runner.List(ctx, Filter{Deployment: m.policy.Deployment})
	if err != nil {
		m.logger.Warn("sandbox: startup orphan sweep: list failed", "err", err)
		return
	}
	for _, sb := range sandboxes {
		if sb.Labels[LabelInstance] == m.instance {
			continue
		}
		m.logger.Info("sandbox: destroying orphan from a previous instance",
			"id", sb.ID, "instance", sb.Labels[LabelInstance])
		destroyCtx, cancel := context.WithTimeout(ctx, destroyTimeout)
		err := m.runner.Destroy(destroyCtx, sb.ID)
		cancel()
		if err != nil {
			m.logger.Warn("sandbox: destroying orphan failed", "id", sb.ID, "err", err)
		}
	}
}

// reconcile retries destroying any sandbox in this instance that Manager no
// longer considers live (its pool session already closed and OnClose fired)
// but that the Runner still reports as existing - i.e. a previous Destroy
// call failed transiently. It is a slow safety net, not the primary teardown
// path.
func (m *Manager) reconcile(ctx context.Context) {
	sandboxes, err := m.runner.List(ctx, Filter{
		Deployment: m.policy.Deployment,
		Labels:     map[string]string{LabelInstance: m.instance},
	})
	if err != nil {
		m.logger.Warn("sandbox: reconcile: list failed", "err", err)
		return
	}
	for _, sb := range sandboxes {
		if m.isLive(sb.ID) {
			continue
		}
		m.logger.Info("sandbox: reconcile: retrying destroy of a sandbox no longer tracked as live", "id", sb.ID)
		destroyCtx, cancel := context.WithTimeout(ctx, destroyTimeout)
		err := m.runner.Destroy(destroyCtx, sb.ID)
		cancel()
		if err != nil {
			m.logger.Warn("sandbox: reconcile destroy failed", "id", sb.ID, "err", err)
		}
	}
}

func (m *Manager) destroyOnError(id string) {
	m.unmarkLive(id)
	destroyCtx, cancel := context.WithTimeout(context.Background(), destroyTimeout)
	defer cancel()
	if err := m.runner.Destroy(destroyCtx, id); err != nil {
		m.logger.Warn("sandbox: destroy-on-error failed, container may be leaked", "id", id, "err", err)
	}
}

func (m *Manager) markLive(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.live[id] = struct{}{}
}

func (m *Manager) unmarkLive(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.live, id)
}

func (m *Manager) isLive(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.live[id]
	return ok
}

// defaultHandshakeTimeout bounds the SSH handshake when ctx carries no
// deadline of its own.
const defaultHandshakeTimeout = 30 * time.Second

// handshakeTimeout derives ssh.ClientConfig.Timeout from ctx: the ssh
// package has no context support of its own, so a caller-supplied deadline
// (e.g. a short one in tests, driving a fast failure instead of a real
// 30-second wait) is honored by translating it into the duration the ssh
// handshake is given.
func handshakeTimeout(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > 0 {
			return d
		}
	}
	return defaultHandshakeTimeout
}

// generateHostKey returns a fresh ed25519 SSH host key: its public key (for
// the client to pin via ssh.FixedHostKey - no TOFU, no known_hosts) and its
// PEM-encoded private key (for the agent's Preamble.HostKeyPEM).
func generateHostKey() (ssh.PublicKey, string, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		return nil, "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, "", err
	}
	return pub, string(pem.EncodeToMemory(block)), nil
}

// generateClientKey returns a fresh ed25519 SSH client key: an ssh.Signer
// for the client's own auth, and its authorized_keys-format public key (for
// the agent's Preamble.AuthorizedKey, the only key allowed to log in).
func generateClientKey() (ssh.Signer, string, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, "", err
	}
	return signer, string(ssh.MarshalAuthorizedKey(signer.PublicKey())), nil
}
