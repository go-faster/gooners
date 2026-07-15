package agent

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/go-faster/errors"
)

// Config configures a sandbox agent SSH server.
type Config struct {
	HostKey       ssh.Signer
	AuthorizedKey ssh.PublicKey
	// Shell is the interactive shell used to run exec/shell commands.
	// Default: $SHELL, else the first of /bin/bash, /bin/sh that exists.
	Shell string
	// Workdir is the working directory for exec/shell commands. Default:
	// the agent process's own working directory.
	Workdir string
	// Version is embedded in the SSH server version banner.
	Version string
	Logger  *slog.Logger
}

func (c *Config) setDefaults() error {
	if c.HostKey == nil {
		return errors.New("agent: Config.HostKey is required")
	}
	if c.AuthorizedKey == nil {
		return errors.New("agent: Config.AuthorizedKey is required")
	}
	if c.Version == "" {
		c.Version = "dev"
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Shell == "" {
		c.Shell = findShell()
	}
	if c.Shell == "" {
		return errors.New("agent: no usable shell found (checked $SHELL, /bin/bash, /bin/sh)")
	}
	return nil
}

// findShell returns $SHELL if it resolves to an executable, else the first
// of /bin/bash, /bin/sh that does. Distroless/scratch images have neither -
// callers must surface that as a clear startup error, not a confusing ENOENT
// from inside a session.
func findShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		if _, err := exec.LookPath(sh); err == nil {
			return sh
		}
	}
	for _, candidate := range []string{"/bin/bash", "/bin/sh"} {
		if _, err := exec.LookPath(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// Serve handles exactly one SSH connection on conn and returns when it
// closes. Each session (exec/shell/sftp) channel is served in its own
// goroutine: the "one connection" constraint from the stdio transport is a
// connection-level constraint, not a channel-level one - callers open a new
// channel per command, plus a concurrent SFTP channel.
func Serve(ctx context.Context, conn net.Conn, cfg Config) error {
	if err := cfg.setDefaults(); err != nil {
		return err
	}

	serverCfg := &ssh.ServerConfig{
		NoClientAuth:  false,
		ServerVersion: "SSH-2.0-gooners_sandbox_agent_" + cfg.Version,
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !bytes.Equal(key.Marshal(), cfg.AuthorizedKey.Marshal()) {
				return nil, errors.New("agent: unauthorized public key")
			}
			return nil, nil
		},
	}
	serverCfg.AddHostKey(cfg.HostKey)

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, serverCfg)
	if err != nil {
		return errors.Wrap(err, "ssh handshake")
	}
	defer func() { _ = sshConn.Close() }()

	go ssh.DiscardRequests(reqs)

	// Closing sshConn when ctx is done unblocks the range over chans below.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = sshConn.Close()
		case <-stop:
		}
	}()

	var wg sync.WaitGroup
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}
		ch, requests, err := newCh.Accept()
		if err != nil {
			cfg.Logger.Warn("agent: accept channel", "error", err)
			continue
		}
		wg.Go(func() {
			handleSession(ctx, ch, requests, cfg)
		})
	}
	wg.Wait()

	return nil
}
