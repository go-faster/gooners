// Package main is the entrypoint for the grafana-dashboard-mcp MCP server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/tools/grafana"
)

func runServer(ctx context.Context, s *mcp.Server, transport, addr string) error {
	handler := func(*http.Request) *mcp.Server { return s }
	switch transport {
	case "stdio", "":
		slog.Info("starting grafana-dashboard-mcp on stdio transport")
		return s.Run(ctx, &mcp.StdioTransport{})

	case "streamable-http":
		handler := mcp.NewStreamableHTTPHandler(handler, &mcp.StreamableHTTPOptions{
			Logger: slog.Default(),
		})
		slog.Info("starting grafana-dashboard-mcp on streamable-http transport", "at", fmt.Sprintf("http://%s/mcp", addr))
		if err := http.ListenAndServe(addr, handler); err != nil { //nolint:gosec // G114: timeouts not required for local/trusted MCP usage
			return fmt.Errorf("streamable-http server exited with error: %w", err)
		}

	case "sse":
		handler := mcp.NewSSEHandler(handler, nil)
		slog.Info("starting grafana-dashboard-mcp on SSE transport", "at", fmt.Sprintf("http://%s", addr))
		if err := http.ListenAndServe(addr, handler); err != nil { //nolint:gosec // G114: timeouts not required for local/trusted MCP usage
			return fmt.Errorf("sse server exited with error: %w", err)
		}

	default:
		return fmt.Errorf("unknown transport: %q", transport)
	}
	return nil
}

func main() {
	logFile := flag.String("log-file", "", "path to log file (enables structured debug logging)")
	transport := flag.String("transport", "stdio", "transport: stdio, streamable-http, sse")
	addr := flag.String("addr", ":8080", "listen address for HTTP transports (streamable-http, sse)")
	sessionsDir := flag.String("sessions-dir", "", "path to dashboard builder sessions directory")
	sessionTTL := flag.Duration("session-ttl", 30*time.Minute, "idle session lifetime before deletion")
	grafanaURL := flag.String("grafana-url", os.Getenv("GRAFANA_URL"), "Grafana base URL")
	grafanaToken := flag.String("grafana-token", os.Getenv("GRAFANA_TOKEN"), "Grafana API token or service account token")
	grafanaUser := flag.String("grafana-user", os.Getenv("GRAFANA_USER"), "Grafana username for basic auth")
	grafanaPassword := flag.String("grafana-password", os.Getenv("GRAFANA_PASSWORD"), "Grafana password for basic auth")
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

	if *sessionsDir == "" {
		xdgData := os.Getenv("XDG_DATA_HOME")
		if xdgData != "" {
			*sessionsDir = filepath.Join(xdgData, "grafana-dashboard-mcp", "sessions")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				slog.Error("getting user home directory", "err", err)
				os.Exit(1)
			}
			*sessionsDir = filepath.Join(home, ".local", "share", "grafana-dashboard-mcp", "sessions")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sm := grafana.NewSessionManager(*sessionsDir)
	go sm.StartCleanupLoop(ctx, *sessionTTL)

	gc := grafana.NewGrafanaClient(*grafanaURL, *grafanaToken, *grafanaUser, *grafanaPassword)

	s := mcp.NewServer(&mcp.Implementation{Name: "grafana-dashboard-mcp", Version: "0.1.0"}, nil)
	grafana.Register(s, sm, gc)

	if err := runServer(ctx, s, *transport, *addr); err != nil {
		slog.Error("failed to run server", "err", err)
		os.Exit(1)
	}
}
