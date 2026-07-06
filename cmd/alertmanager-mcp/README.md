# alertmanager-mcp

An MCP server that exposes Alertmanager alerts, silences, receivers, and cluster status via structured tools, plus a guarded silence-creation workflow and PromQL validation/evaluation helpers.

## Overview

MCP tools are invoked as independent JSON-RPC calls. This server provides read access to Alertmanager's alert, silence, receiver, and status APIs, along with tools to validate matcher expressions and PromQL queries, and to safely create or expire silences with optional duration caps.

For alert inspection workflows, prefer this server's `list_alerts`, `list_alert_groups`, and `list_silences` tools to explore current state. For silence creation, use `preview_silence` first to inspect the blast radius before calling `create_silence`. PromQL validation and evaluation tools (`validate_promql_query`, `evaluate_promql_query`) require optional Prometheus configuration.

## Environment Variables

- `ALERTMANAGER_URL` — the base URL of your Alertmanager instance (e.g. `http://localhost:9093`)
- `ALERTMANAGER_TOKEN` — a bearer token for API authentication
- `ALERTMANAGER_USER` / `ALERTMANAGER_PASSWORD` — basic auth fallback if no token is provided

Optional (only needed for `evaluate_promql_query`):

- `PROMETHEUS_URL` — the base URL of your Prometheus instance (e.g. `http://localhost:9090`)
- `PROMETHEUS_TOKEN` — a bearer token for Prometheus API authentication
- `PROMETHEUS_USER` / `PROMETHEUS_PASSWORD` — basic auth fallback for Prometheus

## Build

```bash
go build ./cmd/alertmanager-mcp
```

## Docker

You can build and run `alertmanager-mcp` using Docker.

### Build the image

From the repository root:
```bash
docker build --target alertmanager-mcp -t alertmanager-mcp .
```

### Run (stdio, for local MCP clients supporting docker stdio)

```bash
docker run --rm -i \
  -e ALERTMANAGER_URL="http://host.docker.internal:9093" \
  -e ALERTMANAGER_TOKEN="your-token" \
  alertmanager-mcp
```

### Run (streamable-http, recommended for Docker)

```bash
docker run --rm -p 8081:8081 \
  -e ALERTMANAGER_URL="http://host.docker.internal:9093" \
  -e ALERTMANAGER_TOKEN="your-token" \
  alertmanager-mcp -transport streamable-http -addr :8081
```

A `/health` endpoint is also served on the same address (e.g. `http://localhost:8081/health`) for liveness checks.

## Flags

- `-transport <stdio|streamable-http|sse>` — protocol to use (default: `stdio`)
- `-addr <host:port>` — listen address for HTTP transports (`streamable-http`, `sse`). Default `:8080`
- `-log-file <path>` — write structured debug logs (slog TextHandler) to the given file.
- `-max-silence-duration <duration>` — cap on how long a single `create_silence` call may request (default: 24h). Supports Prometheus duration strings: `1h`, `2d`, `1w`.

## Tools

### Alert Tools
- `list_alerts` — list alerts matching optional filters (state, label matchers, receiver)
- `list_alert_groups` — list alerts grouped as they are routed for notification

### Silence Tools
- `list_silences` — list active and pending silences
- `get_silence` — fetch a single silence by ID
- `preview_silence` — preview what alerts would be silenced by a matcher expression without creating the silence
- `create_silence` — create a new silence (respects `-max-silence-duration` cap, requires `created_by`/`comment`, rejects catch-all-only matchers)
- `expire_silence` — expire/delete an active silence

### Receiver & Status Tools
- `list_receivers` — list configured notification receivers
- `get_status` — fetch cluster status, version info, uptime, and the running configuration

### Validation Tools
- `validate_matcher_query` — validate Alertmanager matcher expressions (e.g. `job="prometheus"`)
- `validate_promql_query` — validate PromQL syntax (requires Prometheus configuration)
- `evaluate_promql_query` — evaluate a PromQL query and return the result (requires Prometheus configuration)
