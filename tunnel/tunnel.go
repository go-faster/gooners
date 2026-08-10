// Package tunnel provides tunneling listeners.
package tunnel

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os/exec"

	"golang.ngrok.com/ngrok"
	ngrokcfg "golang.ngrok.com/ngrok/config"
	ngroklog "golang.ngrok.com/ngrok/log"
)

// Options configuration for the tunnel listener.
type Options struct {
	Type   string // "http", "tcp"
	Config string // expose config file (for cloudflared)
	Name   string // expose tunnel name (for cloudflared)
	Logger *slog.Logger
}

func (opts *Options) setDefaults() {
	if opts.Type == "" {
		opts.Type = "http"
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
}

// Listen creates a net.Listener that exposes a local port via the specified provider.
func Listen(ctx context.Context, provider string, opts Options) (_ net.Listener, rerr error) {
	opts.setDefaults()

	switch provider {
	case "ngrok":
		var endpoint ngrokcfg.Tunnel
		switch opts.Type {
		case "http", "":
			endpoint = ngrokcfg.HTTPEndpoint()
		case "tcp":
			endpoint = ngrokcfg.TCPEndpoint()
		default:
			return nil, fmt.Errorf("unsupported ngrok type %q", opts.Type)
		}
		return ngrok.Listen(ctx, endpoint,
			ngrok.WithAuthtokenFromEnv(),
			ngrok.WithLogger(&ngrokLogger{logger: opts.Logger}),
		)

	case "cloudflare", "cloudflared":
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("listen local: %w", err)
		}
		defer func() {
			if rerr != nil {
				_ = ln.Close()
			}
		}()

		// cloudflared does not provide a native net.Listener cleanly.
		// we run it as a subprocess and wrap the listener.
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

		stdoutWriter := &slogioWriter{
			logger: opts.Logger,
			level:  slog.LevelDebug,
		}
		stderrWriter := &slogioWriter{
			logger: opts.Logger,
			level:  slog.LevelDebug,
		}

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
			if err != nil {
				if ctx.Err() != nil {
					opts.Logger.Info("cloudflared subprocess stopped (context canceled)", "error", err)
				} else {
					opts.Logger.Error("cloudflared subprocess exited", "error", err)
				}
			} else {
				opts.Logger.Info("cloudflared subprocess exited gracefully")
			}
		}()

		return &cfListener{
			Listener: ln,
			cmd:      cmd,
			waitDone: waitDone,
		}, nil

	default:
		return nil, fmt.Errorf("unknown expose provider %q", provider)
	}
}

type cfListener struct {
	net.Listener
	cmd      *exec.Cmd
	waitDone chan struct{}
}

func (l *cfListener) Close() error {
	err := l.Listener.Close()
	if l.cmd != nil && l.cmd.Process != nil {
		_ = l.cmd.Process.Kill()
	}
	if l.waitDone != nil {
		<-l.waitDone
	}
	return err
}

type ngrokLogger struct {
	logger *slog.Logger
}

var _ ngroklog.Logger = (*ngrokLogger)(nil)

// Log implements the [ngroklog.Logger] interface.
func (l *ngrokLogger) Log(ctx context.Context, level ngroklog.LogLevel, msg string, data map[string]any) {
	var lvl slog.Level
	switch level {
	case ngroklog.LogLevelDebug:
		lvl = slog.LevelDebug
	case ngroklog.LogLevelInfo:
		lvl = slog.LevelInfo
	case ngroklog.LogLevelWarn:
		lvl = slog.LevelWarn
	case ngroklog.LogLevelError:
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	l.logger.LogAttrs(ctx, lvl, msg, dataToAttrs(data)...)
}

func dataToAttrs(data map[string]any) []slog.Attr {
	if len(data) == 0 {
		return nil
	}
	attrs := make([]slog.Attr, 0, len(data))
	for k, v := range data {
		attrs = append(attrs, slog.Any(k, v))
	}
	return attrs
}
