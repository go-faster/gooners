// Package cloudflared exposes a listener through a cloudflared subprocess.
//
// Importing it registers the "cloudflared" and "cloudflare" providers with
// [tunnel.Listen].
package cloudflared

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os/exec"

	"github.com/go-faster/gooners/tunnel"
)

func init() {
	tunnel.Register(Listen, "cloudflared", "cloudflare")
}

// Listen starts cloudflared against a local listener and returns that listener.
//
// cloudflared does not provide a native [net.Listener] cleanly, so it runs as a
// subprocess pointed at a loopback port, and the returned listener owns its
// lifetime.
func Listen(ctx context.Context, opts tunnel.Options) (_ net.Listener, rerr error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen local: %w", err)
	}
	defer func() {
		if rerr != nil {
			_ = ln.Close()
		}
	}()

	u := url.URL{
		Scheme: "http",
		Host:   ln.Addr().String(),
	}

	args := []string{"tunnel", "--no-autoupdate", "--management-diagnostics=false"}
	if opts.Config != "" {
		args = append(args, "--config", opts.Config)
	}
	if opts.Config != "" || opts.Name != "" {
		args = append(args, "run", "--url", u.String())
		if opts.Name != "" {
			args = append(args, opts.Name)
		}
	} else {
		args = append(args, "--url", u.String())
	}

	stdoutWriter := &slogioWriter{logger: opts.Logger, level: slog.LevelDebug}
	stderrWriter := &slogioWriter{logger: opts.Logger, level: slog.LevelDebug}

	opts.Logger.Info("starting cloudflared subprocess", "args", args)
	cmd := exec.CommandContext(ctx, "cloudflared", args...) //nolint:gosec // local trusted execution
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start cloudflared: %w", err)
	}

	opts.Logger.Info("cloudflared subprocess started", "pid", cmd.Process.Pid)
	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		defer func() {
			_ = stdoutWriter.Close()
			_ = stderrWriter.Close()
		}()

		err := cmd.Wait()
		switch {
		case err == nil:
			opts.Logger.Info("cloudflared subprocess exited gracefully")
		case ctx.Err() != nil:
			opts.Logger.Info("cloudflared subprocess stopped (context canceled)", "error", err)
		default:
			opts.Logger.Error("cloudflared subprocess exited", "error", err)
		}
	}()

	return &listener{
		Listener: ln,
		cmd:      cmd,
		waitDone: waitDone,
	}, nil
}

type listener struct {
	net.Listener
	cmd      *exec.Cmd
	waitDone chan struct{}
}

// Close stops the subprocess with the listener, so closing the server does not
// leave a tunnel pointing at a port nothing answers on.
func (l *listener) Close() error {
	err := l.Listener.Close()
	if l.cmd != nil && l.cmd.Process != nil {
		_ = l.cmd.Process.Kill()
	}
	if l.waitDone != nil {
		<-l.waitDone
	}

	return err
}
