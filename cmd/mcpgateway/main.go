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
		// The transport starts before Build, so an upstream that is slow or
		// hung cannot keep the process from answering at all. Until Build
		// finishes the gateway serves an empty tool set, which is what /readyz
		// reports; clients learn the real one through listChanged.
		grp.Go(func() error {
			return transport.Run(ctx, cmdutil.RunOptions{
				Name:       "mcpgateway",
				Handler:    gw.ServerForRequest,
				Middleware: gw.HTTPMiddleware(),
				Logger:     slogger.With("component", "transport"),
				Ready:      gw.Ready,
			})
		})
		// Reloading is sequenced after the initial build rather than run
		// alongside it: a SIGHUP arriving mid-build would otherwise race the
		// registration it is trying to replace.
		grp.Go(func() error {
			if err := gw.Build(ctx); err != nil {
				return errors.Wrap(err, "build gateway")
			}
			return reloader.Run(ctx)
		})
		return grp.Wait()
	}, app.WithServiceName("mcpgateway"))
}
