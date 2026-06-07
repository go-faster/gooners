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
	"github.com/prometheus/common/model"

	"github.com/go-faster/gooners/internal/mcputil"
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
	sessionsDir := flag.String("sessions-dir", os.Getenv("GRAFANA_SESSIONS_DIR"), "path to dashboard builder sessions directory (env: GRAFANA_SESSIONS_DIR)")
	var sessionTTL time.Duration // default 0 (disabled)
	flag.Func("session-ttl", "idle session lifetime before deletion (default: disabled; e.g. 1h, 2d, 1w)", func(s string) error {
		dur, err := model.ParseDuration(s)
		if err != nil {
			return err
		}
		sessionTTL = time.Duration(dur)
		return nil
	})
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

	gc := grafana.NewGrafanaClient(*grafanaURL, *grafanaToken, *grafanaUser, *grafanaPassword)

	s := mcputil.NewServer(mcputil.ServerConfig{
		Name:         "grafana-dashboard-mcp",
		Instructions: "You are connected to grafana-dashboard-mcp. Use these tools to incrementally build and deploy Grafana dashboards.",
		Logger:       slog.Default().With("component", "mcp-sdk"),
		Prompts: []*mcp.Prompt{
			{
				Name:        "design-dashboard",
				Description: "Design and build a high-quality Grafana dashboard",
				Arguments: []*mcp.PromptArgument{
					{Name: "telemetry_standard", Description: "Telemetry standard in use (e.g., Prometheus, OpenTelemetry)", Required: false},
				},
			},
		},
		PromptHandler: mcp.PromptHandler(func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			if req.Params.Name == "design-dashboard" {
				telemetry := "Prometheus or OpenTelemetry (determine from context)"
				if t, ok := req.Params.Arguments["telemetry_standard"]; ok && t != "" {
					telemetry = t
				}

				promptText := fmt.Sprintf("You are designing a Grafana dashboard for a system using %s.\n\n"+
					"**Design Best Practices:**\n"+
					"1. **Layout**: Place high-level SLIs and summary statistics at the top row. Detailed, per-instance metrics go at the bottom.\n"+
					"2. **Methodology**: Use the RED method (Rate, Errors, Duration) for services. Use the USE method (Utilization, Saturation, Errors) for infrastructure.\n"+
					"3. **Consistency**: Use unified unit scales, avoid overlapping lines where stacked charts are better, and keep the number of panels manageable.\n\n"+
					"**Tools Workflow**:\n"+
					"1. Call 'add_dashboard' to create the skeleton.\n"+
					"2. Call 'add_panel' for each visualization.\n"+
					"3. Call 'add_query' to attach the correct PromQL to each panel based on the telemetry standard.", telemetry)

				return &mcp.GetPromptResult{
					Description: "Dashboard design guidelines",
					Messages: []*mcp.PromptMessage{
						{
							Role: "user",
							Content: &mcp.TextContent{
								Text: promptText,
							},
						},
					},
				}, nil
			}
			return nil, fmt.Errorf("unknown prompt: %q", req.Params.Name)
		}),
	})

	sm := grafana.NewSessionManager(*sessionsDir)
	sm.OnEvict = func(id string) {
		mcputil.BroadcastWarning(s, "grafana-mcp", fmt.Sprintf("Dashboard session %q evicted due to inactivity", id))
	}
	go sm.StartCleanupLoop(ctx, sessionTTL)

	grafana.Register(s, sm, gc)

	if err := runServer(ctx, s, *transport, *addr); err != nil {
		slog.Error("failed to run server", "err", err)
		os.Exit(1)
	}
}
