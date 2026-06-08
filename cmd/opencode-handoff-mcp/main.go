// Package main is the entrypoint for the opencode-handoff-mcp MCP server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/go-faster/gooners/internal/cmdutil"
	"github.com/go-faster/gooners/internal/mcputil"
	"github.com/go-faster/gooners/internal/tools/opencode"
)

func main() {
	var (
		logging   cmdutil.LoggingFlags
		transport cmdutil.TransportFlags
	)
	logging.Register(flag.CommandLine)
	transport.Register(flag.CommandLine)
	opencodeURL := flag.String("opencode-url", envDefault("OPENCODE_URL", "http://localhost:4096"), "opencode server base URL (env: OPENCODE_URL)")
	opencodeUser := flag.String("opencode-username", envDefault("OPENCODE_USERNAME", "opencode"), "opencode basic auth username (env: OPENCODE_USERNAME)")
	opencodePassword := flag.String("opencode-password", os.Getenv("OPENCODE_PASSWORD"), "opencode basic auth password (env: OPENCODE_PASSWORD)")
	defaultDirectory := flag.String("default-directory", os.Getenv("OPENCODE_DIRECTORY"), "default x-opencode-directory value (env: OPENCODE_DIRECTORY)")
	requestTimeout := flag.Duration("request-timeout", 30*time.Second, "per-request timeout for opencode HTTP calls")
	waitTimeout := flag.Duration("wait-timeout", 10*time.Minute, "default timeout for handoff_run and handoff_wait")
	flag.Parse()

	cleanup, logger, err := logging.Setup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	client, err := opencode.NewClient(opencode.Config{
		BaseURL:          *opencodeURL,
		Username:         *opencodeUser,
		Password:         *opencodePassword,
		DefaultDirectory: *defaultDirectory,
	}, *requestTimeout)
	if err != nil {
		slog.Error("failed to configure opencode client", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := mcputil.NewServer(mcputil.ServerConfig{
		Name:         "opencode-handoff-mcp",
		Instructions: "You are connected to opencode-handoff-mcp. Use these tools to delegate coding tasks to opencode agents, monitor their sessions, and answer permission or clarification requests when needed.",
		Logger:       logger.With("component", "mcp-sdk"),
	})
	opencode.Register(s, client, opencode.RegisterOptions{WaitTimeout: *waitTimeout})

	if err := transport.Run(ctx, "opencode-handoff-mcp", s, logger.WithGroup("transport")); err != nil {
		slog.Error("failed to run server", "err", err)
		os.Exit(1)
	}
}

func envDefault(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
