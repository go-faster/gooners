// Package main is the entrypoint for the mcpgateway MCP proxy.
package main

import (
	"context"
	"flag"
	"log/slog"

	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/app"
	"go.uber.org/zap"
	"go.uber.org/zap/exp/zapslog"

	"github.com/go-faster/gooners/internal/cmdutil"
	"github.com/go-faster/gooners/internal/gateway"
)

func main() {
	cfgPath := flag.String("config", "gateway.toml", "path to gateway.toml")
	transport := cmdutil.TransportFlags{}
	transport.Register(flag.CommandLine)
	flag.Parse()

	app.Run(func(ctx context.Context, lg *zap.Logger, t *app.Telemetry) error {
		slogger := slog.New(zapslog.NewHandler(lg.Core()))

		cfg, err := gateway.Load(*cfgPath)
		if err != nil {
			return errors.Wrap(err, "load config")
		}
		gw, err := gateway.New(cfg, gateway.Options{
			Logger:         lg,
			Slogger:        slogger,
			MeterProvider:  t.MeterProvider(),
			TracerProvider: t.TracerProvider(),
		})
		if err != nil {
			return errors.Wrap(err, "new gateway")
		}
		defer func() { _ = gw.Close(ctx) }()
		if err := gw.Build(ctx); err != nil {
			return errors.Wrap(err, "build gateway")
		}
		return transport.RunWithHandler(ctx, "mcpgateway", gw.ServerForRequest, slogger.With("component", "transport"))
	}, app.WithServiceName("mcpgateway"))
}
