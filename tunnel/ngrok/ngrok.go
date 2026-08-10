// Package ngrok exposes a listener through ngrok.
//
// Importing it registers the "ngrok" provider with [tunnel.Listen]. It is a
// package of its own because the ngrok SDK is a large dependency to link into a
// binary that only ever uses a different provider.
package ngrok

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"golang.ngrok.com/ngrok"
	ngrokcfg "golang.ngrok.com/ngrok/config"
	ngroklog "golang.ngrok.com/ngrok/log"

	"github.com/go-faster/gooners/tunnel"
)

func init() {
	tunnel.Register(Listen, "ngrok")
}

// Listen opens an ngrok endpoint, authenticated from the environment.
func Listen(ctx context.Context, opts tunnel.Options) (net.Listener, error) {
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
		ngrok.WithLogger(&logger{logger: opts.Logger}),
	)
}

type logger struct {
	logger *slog.Logger
}

var _ ngroklog.Logger = (*logger)(nil)

// Log implements the [ngroklog.Logger] interface.
func (l *logger) Log(ctx context.Context, level ngroklog.LogLevel, msg string, data map[string]any) {
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
