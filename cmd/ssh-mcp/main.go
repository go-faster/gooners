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

	"github.com/mark3labs/mcp-go/server"

	"github.com/go-faster/gooners/internal/session"
	"github.com/go-faster/gooners/internal/tools/core"
	"github.com/go-faster/gooners/internal/tools/disk"
	"github.com/go-faster/gooners/internal/tools/fs"
	"github.com/go-faster/gooners/internal/tools/proc"
	"github.com/go-faster/gooners/internal/tools/sysinfo"
	"github.com/go-faster/gooners/internal/tools/systemd"
)

func runServer(s *server.MCPServer, transport, addr string) error {
	switch transport {
	case "stdio", "":
		slog.Info("starting ssh-mcp on stdio transport")
		if err := server.ServeStdio(s); err != nil {
			return fmt.Errorf("stdio server exited with error: %w", err)
		}
	case "streamable-http":
		h := server.NewStreamableHTTPServer(s)
		slog.Info("starting ssh-mcp on streamable-http transport", "at", fmt.Sprintf("http://%s/mcp", addr))
		if err := http.ListenAndServe(addr, h); err != nil { //nolint:gosec // G114: timeouts not required for local/trusted MCP usage
			return fmt.Errorf("streamable-http server exited with error: %w", err)
		}
	case "sse":
		sse := server.NewSSEServer(s)
		http.Handle("/sse", sse.SSEHandler())
		http.Handle("/message", sse.MessageHandler())
		slog.Info("starting ssh-mcp on SSE transport", "at", fmt.Sprintf("http://%s", addr))
		if err := http.ListenAndServe(addr, nil); err != nil { //nolint:gosec // G114: timeouts not required for local/trusted MCP usage
			return fmt.Errorf("sse server exited with error: %W", err)
		}
	default:
		return fmt.Errorf("unknown transport: %q", transport)
	}
	panic("unreachable")
}

func main() {
	logFile := flag.String("log-file", "", "path to log file (enables structured debug logging)")
	transport := flag.String("transport", "stdio", "transport: stdio, streamable-http, sse")
	addr := flag.String("addr", ":8080", "listen address for HTTP transports (streamable-http, sse)")
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

	s := server.NewMCPServer("ssh-mcp", "0.1.0")
	core.Register(s, pool)
	fs.Register(s, pool, uploadRoot)
	systemd.Register(s, pool)
	sysinfo.Register(s, pool)
	proc.Register(s, pool)
	disk.Register(s, pool)

	if err := runServer(s, *transport, *addr); err != nil {
		slog.Error("failed to run server", "err", err)
		os.Exit(1)
	}
}
