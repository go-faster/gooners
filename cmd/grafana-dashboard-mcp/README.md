# grafana-dashboard-mcp

An MCP server that empowers AI agents to statefully build, verify, and export Grafana dashboards using the official **Grafana Foundation SDK**.

## Overview

MCP tools are invoked as independent JSON-RPC calls, so this server acts as a stateful backend. It keeps track of ongoing dashboard construction sessions in memory and persists them to disk so that restarts or transport drops do not lose state.

Once construction is completed, the agent can call `export_dashboard` to finalise compiling the dashboard with the SDK and output the completed dashboard JSON (and optionally save it directly to the Grafana API).

## Environment Variables

For discovery tools (like validating Prometheus/Loki queries, searching metrics, or saving dashboards to Grafana), configure the following environment variables:

- `GRAFANA_URL` — the base URL of your Grafana instance (e.g. `http://localhost:3000`)
- `GRAFANA_TOKEN` — a service account token or API key with permissions to query/save (e.g. `glsa_...`)
- `GRAFANA_USER` / `GRAFANA_PASSWORD` — basic auth fallback if no token is provided

## Build

```bash
go build ./cmd/grafana-dashboard-mcp
```

## Flags

- `-transport <stdio|streamable-http|sse>` — protocol to use (default: `stdio`)
- `-addr <host:port>` — listen address for HTTP transports (`streamable-http`, `sse`). Default `:8080`
- `-log-file <path>` — write structured debug logs (slog TextHandler) to the given file.
- `-sessions-dir <path>` — directory to persist session snapshots (defaults to `~/.gemini/antigravity-cli/scratch/grafana-sessions`).
- `-session-ttl <duration>` — idle session lifetime before deletion (default: `30m`).

## Tools

### 1. Dashboard Construction Tools
- `add_dashboard` — initializes a new dashboard building session.
- `list_dashboard_sessions` — lists active sessions.
- `add_param` — adds a template variable (query, custom, or datasource) to the dashboard.
- `set_time_range` — sets the default dashboard time range (e.g. `now-6h` to `now`).
- `add_row` — adds a standard Grafana row for grouping panels.
- `add_panel` — adds a panel (timeseries, stat, gauge, table, etc.) to the dashboard (or inside a row).
- `update_panel` — updates properties (title, unit, decimals) of an existing panel.
- `delete_panel` — removes a panel.
- `add_query` — attaches a query to an existing panel.
- `add_threshold` — adds a color threshold to stat/gauge panels.
- `get_dashboard_state` — returns the current in-progress structure of the dashboard session.
- `export_dashboard` — finalizes compile, outputs dashboard JSON, and optionally posts directly to Grafana.

### 2. Datasource Discovery & Verification Tools
- `resolve_datasource` — resolves a datasource name to UID and type.
- `verify_query` — validates a Prometheus or Loki query against the datasource.
- `search_metrics` — finds metric names matching a pattern.
- `lookup_labels` — fetches labels for a selector/metric.
- `lookup_label_values` — fetches available values for a label.
- `lookup_metric_metadata` — returns metric type and help string.
