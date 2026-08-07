# cmd/alertmanager-mcp

Repo-wide rules are in the root `CLAUDE.md`.

## Build

```bash
go build ./cmd/alertmanager-mcp
# Run with default stdio transport:
./alertmanager-mcp
# Or HTTP transport with debug logging:
./alertmanager-mcp -transport streamable-http -addr :8082 -log-file /tmp/alertmanager-mcp.log
```
