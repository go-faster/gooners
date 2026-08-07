# cmd/grafana-dashboard-mcp

Repo-wide rules are in the root `CLAUDE.md`.

## Build

```bash
go build ./cmd/grafana-dashboard-mcp
# Run with default stdio transport:
./grafana-dashboard-mcp
# Or HTTP transport with debug logging and custom session dir:
./grafana-dashboard-mcp -transport streamable-http -addr :8081 -sessions-dir /tmp/sessions -log-file /tmp/grafana-mcp.log
```
