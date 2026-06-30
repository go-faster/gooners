// Package main is the entrypoint for the mcpgateway MCP proxy.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-faster/gooners/internal/cmdutil"
	"github.com/go-faster/gooners/internal/gateway"
)

func main() {
	var (
		logging   cmdutil.LoggingFlags
		transport cmdutil.TransportFlags
		cfgPath   = flag.String("config", "gateway.toml", "path to gateway.toml")
	)
	logging.Register(flag.CommandLine)
	transport.Register(flag.CommandLine)
	flag.Parse()

	cleanup, lg, err := logging.Setup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg, err := gateway.Load(*cfgPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	gw, err := gateway.New(cfg, gateway.Options{Logger: lg})
	if err != nil {
		slog.Error("new gateway", "err", err)
		os.Exit(1)
	}
	defer func() { _ = gw.Close(ctx) }()

	if err := gw.Build(ctx); err != nil {
		slog.Error("build gateway", "err", err)
		os.Exit(1)
	}

	if err := transport.Run(ctx, "mcpgateway", gw.Server(), lg.With("component", "transport")); err != nil {
		slog.Error("run", "err", err)
		os.Exit(1)
	}
}
