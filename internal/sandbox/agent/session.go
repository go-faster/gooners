package agent

import (
	"context"
	"os"
	"os/exec"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/go-faster/errors"
)

// execPayload is the wire payload of an "exec" channel request (RFC 4254 6.5).
type execPayload struct {
	Command string
}

// envPayload is the wire payload of an "env" channel request (RFC 4254 6.7).
type envPayload struct {
	Name  string
	Value string
}

// subsystemPayload is the wire payload of a "subsystem" channel request
// (RFC 4254 6.5).
type subsystemPayload struct {
	Name string
}

// exitStatusPayload is the wire payload of an "exit-status" channel request
// (RFC 4254 6.10). Sending it is mandatory: the consuming SSH client reads
// *ssh.ExitError / ExitStatus() from it, and without a reply every command
// looks like it failed with EOF.
type exitStatusPayload struct {
	Status uint32
}

// handleSession serves the requests of a single SSH session channel: exactly
// one of exec/shell/subsystem, plus any number of env and pty-req requests
// preceding it.
func handleSession(ctx context.Context, ch ssh.Channel, reqs <-chan *ssh.Request, cfg Config) {
	defer func() { _ = ch.Close() }()

	var env []string
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			// Accepted and ignored: the sandbox transport carries raw SSH
			// traffic, not a terminal; a real pty would mangle it (LF->CRLF,
			// ^C/^D interpretation).
			reply(req, true)

		case "env":
			var p envPayload
			if err := ssh.Unmarshal(req.Payload, &p); err != nil {
				reply(req, false)
				continue
			}
			env = append(env, p.Name+"="+p.Value)
			reply(req, true)

		case "exec":
			var p execPayload
			if err := ssh.Unmarshal(req.Payload, &p); err != nil {
				reply(req, false)
				continue
			}
			reply(req, true)
			runCommand(ctx, ch, cfg, []string{"-c", p.Command}, env)
			return

		case "shell":
			reply(req, true)
			runCommand(ctx, ch, cfg, nil, env)
			return

		case "subsystem":
			var p subsystemPayload
			if err := ssh.Unmarshal(req.Payload, &p); err != nil {
				reply(req, false)
				continue
			}
			if p.Name != "sftp" {
				reply(req, false)
				continue
			}
			reply(req, true)
			serveSFTP(ch, cfg)
			return

		default:
			reply(req, false)
		}
	}
}

func reply(req *ssh.Request, ok bool) {
	if req.WantReply {
		_ = req.Reply(ok, nil)
	}
}

// runCommand runs cfg.Shell with args through exec.CommandContext (so `cd x
// && cmd`, pipes, and `sudo -S` work as expected of a shell), wires the
// channel as stdin/stdout, ch.Stderr() as stderr, and always replies with
// exit-status once the process is done.
func runCommand(ctx context.Context, ch ssh.Channel, cfg Config, args, env []string) {
	cmd := exec.CommandContext(ctx, cfg.Shell, args...) //nolint:gosec // G204: running a client-supplied command through a shell is the whole point of an SSH exec server
	if cfg.Workdir != "" {
		cmd.Dir = cfg.Workdir
	}
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = ch
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()

	runErr := cmd.Run()

	status := exitStatusOf(runErr, cfg)
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(exitStatusPayload{Status: status}))
}

func exitStatusOf(err error, cfg Config) uint32 {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return uint32(code)
		}
		return 1 // killed by signal or similar: no meaningful exit code
	}
	cfg.Logger.Warn("agent: command failed to start", "error", err)
	return 127
}

func serveSFTP(ch ssh.Channel, cfg Config) {
	server, err := sftp.NewServer(ch)
	if err != nil {
		cfg.Logger.Warn("agent: sftp new server", "error", err)
		return
	}
	defer func() { _ = server.Close() }()

	if err := server.Serve(); err != nil {
		cfg.Logger.Debug("agent: sftp session ended", "error", err)
	}
}
