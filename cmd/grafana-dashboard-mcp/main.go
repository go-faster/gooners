// Package main is the entrypoint for the grafana-dashboard-mcp MCP server.
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/common/model"

	"github.com/go-faster/gooners/internal/cmdutil"
	"github.com/go-faster/gooners/internal/mcputil"
	"github.com/go-faster/gooners/internal/tools/grafana"
)

//go:embed design-dashboard.md
var designDashboardPrompt string

func main() {
	var (
		logging   cmdutil.LoggingFlags
		transport cmdutil.TransportFlags
	)
	logging.Register(flag.CommandLine)
	transport.Register(flag.CommandLine)

	var (
		sessionTTL model.Duration // default 0 (disabled)

		sessionsDir     = flag.String("sessions-dir", os.Getenv("GRAFANA_SESSIONS_DIR"), "path to dashboard builder sessions directory (env: GRAFANA_SESSIONS_DIR)")
		grafanaURL      = flag.String("grafana-url", os.Getenv("GRAFANA_URL"), "Grafana base URL")
		grafanaToken    = flag.String("grafana-token", os.Getenv("GRAFANA_TOKEN"), "Grafana API token or service account token")
		grafanaUser     = flag.String("grafana-user", os.Getenv("GRAFANA_USER"), "Grafana username for basic auth")
		grafanaPassword = flag.String("grafana-password", os.Getenv("GRAFANA_PASSWORD"), "Grafana password for basic auth")
	)

	flag.TextVar(&sessionTTL, "session-ttl", &sessionTTL, "idle session lifetime before deletion (default: disabled; e.g. 1h, 2d, 1w)")
	flag.Parse()

	cleanup, logger, err := logging.Setup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		os.Exit(1)
	}
	defer cleanup()

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
		Logger:       logger.With("component", "mcp-sdk"),
		Prompts: []*mcp.Prompt{
			{
				Name:        "design-dashboard",
				Description: "Design and build a high-quality Grafana dashboard",
			},
		},
		PromptHandler: mcp.PromptHandler(func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			if req.Params.Name == "design-dashboard" {
				return &mcp.GetPromptResult{
					Description: "Dashboard design guidelines",
					Messages: []*mcp.PromptMessage{
						{
							Role: "user",
							Content: &mcp.TextContent{
								Text: designDashboardPrompt,
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
	go sm.StartCleanupLoop(ctx, time.Duration(sessionTTL))
	grafana.Register(s, sm, gc)

	if err := transport.Run(ctx, cmdutil.RunOptions{
		Name:   "grafana-dashboard-mcp",
		Server: s,
		Logger: logger.With("component", "transport"),
	}); err != nil {
		slog.Error("failed to run server", "err", err)
		os.Exit(1)
	}
}
