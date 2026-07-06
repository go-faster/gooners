// Package main is the entrypoint for the alertmanager-mcp MCP server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/common/model"

	"github.com/go-faster/gooners/internal/cmdutil"
	"github.com/go-faster/gooners/internal/mcputil"
	"github.com/go-faster/gooners/internal/tools/alertmanager"
)

func main() {
	var (
		logging   cmdutil.LoggingFlags
		transport cmdutil.TransportFlags
	)
	logging.Register(flag.CommandLine)
	transport.Register(flag.CommandLine)

	var (
		maxSilenceDuration model.Duration // default 0 (disabled, uses package default)

		alertmanagerURL      = flag.String("alertmanager-url", os.Getenv("ALERTMANAGER_URL"), "Alertmanager base URL")
		alertmanagerToken    = flag.String("alertmanager-token", os.Getenv("ALERTMANAGER_TOKEN"), "Alertmanager API token")
		alertmanagerUser     = flag.String("alertmanager-user", os.Getenv("ALERTMANAGER_USER"), "Alertmanager username for basic auth")
		alertmanagerPassword = flag.String("alertmanager-password", os.Getenv("ALERTMANAGER_PASSWORD"), "Alertmanager password for basic auth")

		prometheusURL      = flag.String("prometheus-url", os.Getenv("PROMETHEUS_URL"), "Prometheus base URL (optional, only needed for evaluate_promql_query)")
		prometheusToken    = flag.String("prometheus-token", os.Getenv("PROMETHEUS_TOKEN"), "Prometheus API token")
		prometheusUser     = flag.String("prometheus-user", os.Getenv("PROMETHEUS_USER"), "Prometheus username for basic auth")
		prometheusPassword = flag.String("prometheus-password", os.Getenv("PROMETHEUS_PASSWORD"), "Prometheus password for basic auth")
	)

	flag.TextVar(&maxSilenceDuration, "max-silence-duration", &maxSilenceDuration, "maximum duration a create_silence call may request (default: 24h; e.g. 1h, 2d)")
	flag.Parse()

	cleanup, logger, err := logging.Setup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := alertmanager.Config{
		AlertmanagerURL:      *alertmanagerURL,
		AlertmanagerToken:    *alertmanagerToken,
		AlertmanagerUser:     *alertmanagerUser,
		AlertmanagerPassword: *alertmanagerPassword,
		PrometheusURL:        *prometheusURL,
		PrometheusToken:      *prometheusToken,
		PrometheusUser:       *prometheusUser,
		PrometheusPassword:   *prometheusPassword,
		MaxSilenceDuration:   time.Duration(maxSilenceDuration),
	}

	c, err := alertmanager.NewClient(cfg)
	if err != nil {
		slog.Error("failed to create alertmanager client", "err", err)
		os.Exit(1)
	}

	s := mcputil.NewServer(mcputil.ServerConfig{
		Name:         "alertmanager-mcp",
		Instructions: "You are connected to alertmanager-mcp. Use these tools to inspect Alertmanager alerts, silences, receivers, and cluster status, and to validate/evaluate PromQL queries. Prefer preview_silence before create_silence to check blast radius.",
		Logger:       logger.With("component", "mcp-sdk"),
	})

	alertmanager.Register(s, c)

	if err := transport.Run(ctx, "alertmanager-mcp", s, logger.With("component", "transport")); err != nil {
		slog.Error("failed to run server", "err", err)
		os.Exit(1)
	}
}
