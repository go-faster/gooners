// Package main is the entrypoint for the ssh-mcp MCP server.
package main

import (
	"flag"
	"log"
	"log/slog"
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

func main() {
	logFile := flag.String("log-file", "", "path to log file (enables structured debug logging)")
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

	pool := session.NewPool()
	defer pool.Shutdown()

	s := server.NewMCPServer("ssh-mcp", "0.1.0")
	core.Register(s, pool)
	fs.Register(s, pool, uploadRoot)
	systemd.Register(s, pool)
	sysinfo.Register(s, pool)
	proc.Register(s, pool)
	disk.Register(s, pool)

	if err := server.ServeStdio(s); err != nil {
		slog.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}
