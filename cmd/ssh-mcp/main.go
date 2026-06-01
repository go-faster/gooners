// Package main is the entrypoint for the ssh-mcp MCP server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/tools/core"
	"github.com/go-faster/gooners/internal/tools/disk"
	"github.com/go-faster/gooners/internal/tools/fs"
	"github.com/go-faster/gooners/internal/tools/proc"
	"github.com/go-faster/gooners/internal/tools/sysinfo"
	"github.com/go-faster/gooners/internal/tools/systemd"
)

func runServer(ctx context.Context, s *mcp.Server, transport, addr string) error {
	handler := func(*http.Request) *mcp.Server { return s }
	switch transport {
	case "stdio", "":
		slog.Info("starting ssh-mcp on stdio transport")
		return s.Run(ctx, &mcp.StdioTransport{})

	case "streamable-http":
		handler := mcp.NewStreamableHTTPHandler(handler, &mcp.StreamableHTTPOptions{
			Logger: slog.Default(),
		})
		slog.Info("starting ssh-mcp on streamable-http transport", "at", fmt.Sprintf("http://%s/mcp", addr))
		if err := http.ListenAndServe(addr, handler); err != nil { //nolint:gosec // G114: timeouts not required for local/trusted MCP usage
			return fmt.Errorf("streamable-http server exited with error: %w", err)
		}

	case "sse":
		handler := mcp.NewSSEHandler(handler, nil)
		slog.Info("starting ssh-mcp on SSE transport", "at", fmt.Sprintf("http://%s", addr))
		if err := http.ListenAndServe(addr, handler); err != nil { //nolint:gosec // G114: timeouts not required for local/trusted MCP usage
			return fmt.Errorf("sse server exited with error: %w", err)
		}

	default:
		return fmt.Errorf("unknown transport: %q", transport)
	}
	return nil
}

func main() {
	logFile := flag.String("log-file", "", "path to log file (enables structured debug logging)")
	transport := flag.String("transport", "stdio", "transport: stdio, streamable-http, sse")
	addr := flag.String("addr", ":8080", "listen address for HTTP transports (streamable-http, sse)")
	disableSudo := flag.Bool("disable-sudo", false, "do not register the ssh_sudo_exec tool")
	sudoPasswordFile := flag.String("sudo-password-file", "", "file containing the sudo password (re-read on each use)")
	sudoPasswordEnv := flag.String("sudo-password-env", "", "env var containing the sudo password")
	sudoPasswordCmd := flag.String("sudo-password-cmd", "", "command whose stdout is used as the sudo password (result is cached)")
	flag.Parse()

	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			log.Fatalf("opening log file: %v", err)
		}
		defer func() { _ = f.Close() }()

		handler := slog.NewTextHandler(f, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})
		slog.SetDefault(slog.New(handler))
	}

	uploadRoot, err := os.Getwd()
	if err != nil {
		slog.Error("getting working directory", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := session.NewPool()
	go pool.Run(ctx)

	var sudoPasswd core.SudoPasswordProvider
	switch {
	case *sudoPasswordFile != "":
		sudoPasswd = &core.FilePasswordProvider{Path: *sudoPasswordFile}
	case *sudoPasswordEnv != "":
		sudoPasswd = &core.EnvPasswordProvider{VarName: *sudoPasswordEnv}
	case *sudoPasswordCmd != "":
		sudoPasswd = &core.CommandPasswordProvider{Command: *sudoPasswordCmd}
	}

	s := mcp.NewServer(&mcp.Implementation{Name: "ssh-mcp", Version: "0.1.0"}, nil)
	core.Register(s, pool, core.RegisterOptions{DisableSudo: *disableSudo, SudoPassword: sudoPasswd})
	fs.Register(s, pool, uploadRoot)
	systemd.Register(s, pool)
	sysinfo.Register(s, pool)
	proc.Register(s, pool)
	disk.Register(s, pool)

	if err := runServer(ctx, s, *transport, *addr); err != nil {
		slog.Error("failed to run server", "err", err)
		os.Exit(1)
	}
}
