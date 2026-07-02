// Package cmdutil provides shared command-line helpers for MCP binaries.
package cmdutil

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/tunnel"
)

// LoggingFlags are common flags for configuring slog output.
type LoggingFlags struct {
	LogFile   string
	LogFormat string
	LogLevel  slog.Level
}

// Register registers common logging flags on fs.
func (flags *LoggingFlags) Register(fs *flag.FlagSet) {
	flags.LogLevel = slog.LevelInfo
	fs.TextVar(&flags.LogLevel, "log-level", &flags.LogLevel, "log level: debug, info, warn, error")
	fs.StringVar(&flags.LogFile, "log-file", "", "path to log file; stderr is used when empty")
	fs.StringVar(&flags.LogFormat, "log-format", "text", "log format: text, json")
}

// Setup configures slog from common logging flags.
func (flags *LoggingFlags) Setup() (func(), *slog.Logger, error) {
	// Validate format before creating the log file to avoid leaking the fd.
	var newHandler func(io.Writer, *slog.HandlerOptions) slog.Handler
	switch flags.LogFormat {
	case "json":
		newHandler = func(w io.Writer, o *slog.HandlerOptions) slog.Handler { return slog.NewJSONHandler(w, o) }
	case "text", "":
		newHandler = func(w io.Writer, o *slog.HandlerOptions) slog.Handler { return slog.NewTextHandler(w, o) }
	default:
		return func() {}, slog.Default(), fmt.Errorf("unknown log format: %q", flags.LogFormat)
	}

	var (
		out     = io.Writer(os.Stderr)
		cleanup = func() {}
	)

	if flags.LogFile != "" {
		f, err := os.OpenFile(flags.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return cleanup, slog.Default(), fmt.Errorf("open logging file: %w", err)
		}
		out = f
		cleanup = func() { _ = f.Close() }
	}

	opts := &slog.HandlerOptions{Level: flags.LogLevel}
	handler := newHandler(out, opts)
	logger := slog.New(handler)
	slog.SetDefault(logger)

	return cleanup, logger, nil
}

// TransportFlags are common MCP server transport flags.
type TransportFlags struct {
	Transport                  string
	Addr                       string
	ExposeProvider             string
	ExposeType                 string
	ExposeConfig               string
	ExposeName                 string
	DisableLocalhostProtection bool
}

// Register registers common MCP transport flags on fs.
func (flags *TransportFlags) Register(fs *flag.FlagSet) {
	fs.StringVar(&flags.Transport, "transport", "stdio", "transport: stdio, streamable-http, sse")
	fs.StringVar(&flags.Addr, "addr", ":8080", "listen address for HTTP transports (streamable-http, sse)")
	fs.StringVar(&flags.ExposeProvider, "expose-provider", "", "expose provider: ngrok, cloudflared")
	fs.StringVar(&flags.ExposeType, "expose-type", "", "expose type: http, tcp (depends on provider)")
	fs.StringVar(&flags.ExposeConfig, "expose-config", "", "expose config file (for cloudflared)")
	fs.StringVar(&flags.ExposeName, "expose-name", "", "expose tunnel name (for cloudflared)")
	fs.BoolVar(&flags.DisableLocalhostProtection, "disable-localhost-protection", false, "disable localhost protection (for exposure via external tunnels)")
}

// Run starts an MCP server using the selected transport.
func (flags TransportFlags) Run(ctx context.Context, name string, s *mcp.Server, lg *slog.Logger) error {
	if flags.Transport == "stdio" && (flags.ExposeProvider != "" || flags.ExposeType != "" || flags.ExposeConfig != "" || flags.ExposeName != "") {
		return errors.New("cannot use expose flags with stdio transport")
	}
	if err := flags.applyExposeDefaults(); err != nil {
		return err
	}

	handler := func(*http.Request) *mcp.Server { return s }

	switch flags.Transport {
	case "stdio", "":
		lg.Info("starting MCP server on stdio transport", "server", name)
		return s.Run(ctx, &mcp.StdioTransport{})

	case "streamable-http":
		h := mcp.NewStreamableHTTPHandler(handler, &mcp.StreamableHTTPOptions{
			Logger:                     slog.Default(),
			DisableLocalhostProtection: flags.DisableLocalhostProtection,
		})
		lg.Info("starting MCP server on streamable-http transport", "server", name, "at", fmt.Sprintf("http://%s/mcp", flags.Addr))
		return flags.runHTTPServer(ctx, &http.Server{Addr: flags.Addr, Handler: h}, lg) //nolint:gosec // G114: local/trusted MCP usage follows existing repo pattern.

	case "sse":
		opts := &mcp.SSEOptions{
			DisableLocalhostProtection: flags.DisableLocalhostProtection,
		}
		h := mcp.NewSSEHandler(handler, opts)
		lg.Info("starting MCP server on SSE transport", "server", name, "at", fmt.Sprintf("http://%s", flags.Addr))
		return flags.runHTTPServer(ctx, &http.Server{Addr: flags.Addr, Handler: h}, lg) //nolint:gosec // G114: local/trusted MCP usage follows existing repo pattern.

	default:
		return fmt.Errorf("unknown transport: %q", flags.Transport)
	}
}

func (flags TransportFlags) runHTTPServer(ctx context.Context, srv *http.Server, lg *slog.Logger) error {
	var ln net.Listener
	var err error

	provider, err := flags.resolveExposeProvider()
	if err != nil {
		return err
	}
	if provider != "" {
		ln, err = tunnel.Listen(ctx, provider, tunnel.Options{
			Type:   flags.ExposeType,
			Config: flags.ExposeConfig,
			Name:   flags.ExposeName,
			Logger: lg.With("component", "tunnel"),
		})
		if err != nil {
			return fmt.Errorf("tunnel listen: %w", err)
		}
		lg.Info("started tunnel", "provider", provider)
	} else {
		ln, err = net.Listen("tcp", srv.Addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", srv.Addr, err)
		}
	}

	parentCtx := ctx
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		<-ctx.Done()
		lg.Info("shutting down HTTP server")
		shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	})
	g.Go(func() error {
		if err := srv.Serve(ln); err != nil {
			if errors.Is(err, http.ErrServerClosed) && parentCtx.Err() != nil {
				lg.Info("HTTP server closed gracefully")
				return nil
			}
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})
	return g.Wait()
}

func (flags TransportFlags) resolveExposeProvider() (string, error) {
	provider := flags.ExposeProvider
	if flags.ExposeName != "" || flags.ExposeConfig != "" {
		if provider != "" && provider != "cloudflare" && provider != "cloudflared" {
			return "", fmt.Errorf("expose-name and expose-config require cloudflare provider")
		}
		provider = "cloudflare"
	}
	if provider == "cloudflared" {
		provider = "cloudflare"
	}
	if provider == "cloudflare" && flags.ExposeType == "tcp" {
		return "", fmt.Errorf("expose-type tcp is not supported with cloudflare provider")
	}
	return provider, nil
}

func (flags *TransportFlags) applyExposeDefaults() error {
	provider, err := flags.resolveExposeProvider()
	if err != nil {
		return err
	}
	flags.ExposeProvider = provider
	if provider != "" {
		flags.DisableLocalhostProtection = true
	}
	return nil
}
