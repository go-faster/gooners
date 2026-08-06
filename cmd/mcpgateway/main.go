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
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/gooners/internal/cmdutil"
	"github.com/go-faster/gooners/internal/gateway"
)

func main() {
	cfgPath := flag.String("config", "gateway.toml", "path to gateway.toml")
	watchInterval := flag.Duration("config-watch-interval", 0,
		"poll the config file for changes at this interval; 0 reloads on SIGHUP only")
	transport := cmdutil.TransportFlags{}
	transport.Register(flag.CommandLine)
	flag.Parse()

	app.Run(func(ctx context.Context, lg *zap.Logger, t *app.Telemetry) error {
		slogger := slog.New(zapslog.NewHandler(lg.Core()))

		src, err := gateway.NewFileSource(gateway.FileSourceOptions{
			Path:     *cfgPath,
			Interval: *watchInterval,
		})
		if err != nil {
			return errors.Wrap(err, "config source")
		}
		cfg, err := src.Load(ctx)
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

		reloader, err := gateway.NewReloader(gateway.ReloaderOptions{
			Source:        src,
			Target:        gw,
			Logger:        lg,
			MeterProvider: t.MeterProvider(),
		})
		if err != nil {
			return errors.Wrap(err, "new reloader")
		}

		grp, ctx := errgroup.WithContext(ctx)
		grp.Go(func() error {
			return reloader.Run(ctx)
		})
		grp.Go(func() error {
			return transport.Run(ctx, cmdutil.RunOptions{
				Name:       "mcpgateway",
				Handler:    gw.ServerForRequest,
				Middleware: gw.HTTPMiddleware(),
				Logger:     slogger.With("component", "transport"),
			})
		})
		return grp.Wait()
	}, app.WithServiceName("mcpgateway"))
}
