// Package cmdutil provides shared command-line helpers for MCP binaries.
package cmdutil

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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
	TLSCertFile                string
	TLSKeyFile                 string
	TLSClientCAFile            string
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
	fs.StringVar(&flags.TLSCertFile, "tls-cert-file", "", "TLS certificate file for HTTP transports")
	fs.StringVar(&flags.TLSKeyFile, "tls-key-file", "", "TLS private key file for HTTP transports")
	fs.StringVar(&flags.TLSClientCAFile, "tls-client-ca-file", "", "CA file for verifying client certificates on HTTP transports")
	fs.StringVar(&flags.ExposeProvider, "expose-provider", "", "expose provider: ngrok, cloudflared")
	fs.StringVar(&flags.ExposeType, "expose-type", "", "expose type: http, tcp (depends on provider)")
	fs.StringVar(&flags.ExposeConfig, "expose-config", "", "expose config file (for cloudflared)")
	fs.StringVar(&flags.ExposeName, "expose-name", "", "expose tunnel name (for cloudflared)")
	fs.BoolVar(&flags.DisableLocalhostProtection, "disable-localhost-protection", false, "disable localhost protection (for exposure via external tunnels)")
}

// Run starts an MCP server using the selected transport.
func (flags TransportFlags) Run(ctx context.Context, name string, s *mcp.Server, lg *slog.Logger) error {
	handler := func(*http.Request) *mcp.Server { return s }
	return flags.RunWithHandler(ctx, name, handler, lg)
}

// RunWithHandler starts an MCP server using a request-aware server selector.
func (flags TransportFlags) RunWithHandler(ctx context.Context, name string, handler func(*http.Request) *mcp.Server, lg *slog.Logger) error {
	if flags.Transport == "stdio" {
		if flags.ExposeProvider != "" || flags.ExposeType != "" || flags.ExposeConfig != "" || flags.ExposeName != "" {
			return errors.New("cannot use expose flags with stdio transport")
		}
		if flags.TLSCertFile != "" || flags.TLSKeyFile != "" || flags.TLSClientCAFile != "" {
			return errors.New("cannot use TLS flags with stdio transport")
		}
	}
	if err := flags.applyExposeDefaults(); err != nil {
		return err
	}

	switch flags.Transport {
	case "stdio", "":
		lg.Info("starting MCP server on stdio transport", "server", name)
		return handler(nil).Run(ctx, &mcp.StdioTransport{})

	case "streamable-http":
		scheme := flags.httpScheme()
		h := mcp.NewStreamableHTTPHandler(handler, &mcp.StreamableHTTPOptions{
			Logger:                     slog.Default(),
			DisableLocalhostProtection: flags.DisableLocalhostProtection,
		})
		mux := http.NewServeMux()
		mux.Handle("/health", healthHandler(name))
		mux.Handle("/", h)
		lg.Info("starting MCP server on streamable-http transport", "server", name, "at", fmt.Sprintf("%s://%s/mcp", scheme, flags.Addr))
		return flags.runHTTPServer(ctx, &http.Server{Addr: flags.Addr, Handler: mux}, lg) //nolint:gosec // G114: local/trusted MCP usage follows existing repo pattern.

	case "sse":
		scheme := flags.httpScheme()
		opts := &mcp.SSEOptions{
			DisableLocalhostProtection: flags.DisableLocalhostProtection,
		}
		h := mcp.NewSSEHandler(handler, opts)
		mux := http.NewServeMux()
		mux.Handle("/health", healthHandler(name))
		mux.Handle("/", h)
		lg.Info("starting MCP server on SSE transport", "server", name, "at", fmt.Sprintf("%s://%s", scheme, flags.Addr))
		return flags.runHTTPServer(ctx, &http.Server{Addr: flags.Addr, Handler: mux}, lg) //nolint:gosec // G114: local/trusted MCP usage follows existing repo pattern.

	default:
		return fmt.Errorf("unknown transport: %q", flags.Transport)
	}
}

// healthHandler returns a handler for a liveness check endpoint, reporting
// that the process is up and serving requests.
func healthHandler(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "server": name})
	})
}

func (flags TransportFlags) runHTTPServer(ctx context.Context, srv *http.Server, lg *slog.Logger) error {
	var ln net.Listener
	var err error
	tlsConfig, err := flags.tlsConfig()
	if err != nil {
		return err
	}

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
	if tlsConfig != nil {
		ln = tls.NewListener(ln, tlsConfig)
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

func (flags TransportFlags) httpScheme() string {
	if flags.TLSCertFile != "" || flags.TLSKeyFile != "" {
		return "https"
	}
	return "http"
}

func (flags TransportFlags) tlsConfig() (*tls.Config, error) {
	if flags.TLSCertFile == "" && flags.TLSKeyFile == "" && flags.TLSClientCAFile == "" {
		return nil, nil
	}
	if flags.TLSCertFile == "" || flags.TLSKeyFile == "" {
		return nil, errors.New("tls-cert-file and tls-key-file must be set together")
	}
	cert, err := tls.LoadX509KeyPair(flags.TLSCertFile, flags.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS certificate: %w", err)
	}
	config := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	if flags.TLSClientCAFile != "" {
		pem, err := os.ReadFile(flags.TLSClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("read TLS client CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("parse TLS client CA file %q", flags.TLSClientCAFile)
		}
		config.ClientCAs = pool
		config.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return config, nil
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
