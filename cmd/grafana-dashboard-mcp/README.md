# grafana-dashboard-mcp

An MCP server that empowers AI agents to statefully build, verify, and export Grafana dashboards using the official **Grafana Foundation SDK**.

## Overview

MCP tools are invoked as independent JSON-RPC calls, so this server acts as a stateful backend. It keeps track of ongoing dashboard construction sessions in memory and persists them to disk so that restarts or transport drops do not lose state.

Once construction is completed, the agent can call `export_dashboard` to finalise compiling the dashboard with the SDK and output the completed dashboard JSON. Sessions default to Grafana dashboard `v1`; pass `version: "v2"` to `add_dashboard` only when v2 output is explicitly required.

For dashboard exports, prefer this MCP server's `export_dashboard` tool with `output_path` whenever possible. It avoids loading full dashboard JSON into the model context. For raw exports from an existing Grafana instance, direct Grafana HTTP/API access such as `curl` is usually better than a generic Grafana MCP read that returns the whole dashboard payload.

## Environment Variables

For discovery tools (like validating Prometheus/Loki queries, searching metrics, or saving dashboards to Grafana), configure the following environment variables:

- `GRAFANA_URL` — the base URL of your Grafana instance (e.g. `http://localhost:3000`)
- `GRAFANA_TOKEN` — a service account token or API key with permissions to query/save (e.g. `glsa_...`)
- `GRAFANA_USER` / `GRAFANA_PASSWORD` — basic auth fallback if no token is provided

## Build

```bash
go build ./cmd/grafana-dashboard-mcp
```

## Docker

You can build and run `grafana-dashboard-mcp` using Docker.

### Build the image

From the repository root:
```bash
docker build --target grafana-dashboard-mcp -t grafana-dashboard-mcp .
```

### Run (stdio, for local MCP clients supporting docker stdio)

```bash
docker run --rm -i \
  -e GRAFANA_URL="http://host.docker.internal:3000" \
  -e GRAFANA_TOKEN="your-token" \
  grafana-dashboard-mcp
```

### Run (streamable-http, recommended for Docker)

```bash
docker run --rm -p 8081:8081 \
  -e GRAFANA_URL="http://host.docker.internal:3000" \
  -e GRAFANA_TOKEN="your-token" \
  grafana-dashboard-mcp -transport streamable-http -addr :8081
```

A `/health` endpoint is also served on the same address (e.g. `http://localhost:8081/health`) for liveness checks.

### Docker Compose Sandbox

For a complete local sandbox environment containing both a Grafana instance and the MCP server configured to communicate, see the [examples/grafana-dashboard-mcp](../../examples/grafana-dashboard-mcp) directory.


## Flags

- `-transport <stdio|streamable-http|sse>` — protocol to use (default: `stdio`)
- `-addr <host:port>` — listen address for HTTP transports (`streamable-http`, `sse`). Default `:8080`
- `-log-file <path>` — write structured debug logs (slog TextHandler) to the given file.
- `-sessions-dir <path>` — directory to persist session snapshots (defaults to `~/.gemini/antigravity-cli/scratch/grafana-sessions`).
- `-session-ttl <duration>` — idle session lifetime before deletion (default: disabled). Supports Prometheus duration strings: `1h`, `2d`, `1w`.

## Tools

### 1. Dashboard Construction Tools
- `add_dashboard` — initializes a new dashboard building session. Defaults to `version: "v1"`; use `version: "v2"` only when needed.
- `list_dashboard_sessions` — lists active sessions.
- `import_dashboard` — fetches existing dashboard by its Grafana UID and loads it into an editable session (supports round-tripping dashboards originally built with this MCP).
- `add_param` — adds a template variable (query, custom, or datasource) to the dashboard.
- `set_time_range` — sets the default dashboard time range (e.g. `now-6h` to `now`).
- `add_row` — adds a standard Grafana row for grouping panels.
- `add_panel` — adds a panel (timeseries, stat, gauge, table, etc.) to the dashboard (or inside a row). Supports unit, decimals, and reduce_calcs directly.
- `add_panels_batch` — adds multiple panels to a dashboard or a row in a single batch operation (with queries/thresholds).
- `update_panel` — updates properties (title, description, unit, decimals, reduce_calcs) of an existing panel.
- `delete_panel` — removes a panel.
- `add_query` — attaches a query to an existing panel.
- `add_threshold` — adds a color threshold to stat/gauge panels.
- `get_dashboard_state` — returns the current in-progress structure of the dashboard session.
- `export_dashboard` — finalizes compile and outputs v1/v2 dashboard JSON. Direct Grafana save is supported for v1 dashboards.

### 2. Datasource Discovery & Verification Tools
- `resolve_datasource` — resolves a datasource name to UID and type.
- `verify_query` — validates a Prometheus or Loki query against the datasource.
- `search_metrics` — finds metric names matching a pattern.
- `lookup_labels` — fetches labels for a selector/metric.
- `lookup_label_values` — fetches available values for a label.
- `lookup_metric_metadata` — returns metric type and help string.
- `discover_telemetry_registry` — parses Weaver/OpenTelemetry semantic convention YAML files under the working directory to discover metrics, instruments, units, and attributes.
