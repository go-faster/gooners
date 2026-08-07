# cmd/gitlab-mcp

Repo-wide rules are in the root `CLAUDE.md`; the tool implementations live in
`internal/tools/gitlab`, which has its own guide for what a tool there must and must not do.

## Build

```bash
go build ./cmd/gitlab-mcp
# Run with default stdio transport (credentials come from the glab CLI config):
./gitlab-mcp
# Pin a default project and enable the release asset tools:
./gitlab-mcp -project mygroup/myproject -assets-dir ./assets
# Or HTTP transport with debug logging:
./gitlab-mcp -transport streamable-http -addr :8083 -log-file /tmp/gitlab-mcp.log
# Shared server where each caller sends its own token on PRIVATE-TOKEN/Authorization:
./gitlab-mcp -transport streamable-http -addr :8083 -auth client -tls-cert-file c.pem -tls-key-file k.pem
```
