# mcpgateway

MCP proxy / gateway that connects to multiple upstream MCP servers (stdio, streamable-http, sse), aggregates their tools/prompts/resources, applies optional namespacing prefixes, allow/deny filters, description trimming and output redaction, then re-exports them on a single local MCP server.

## Build

```bash
go build ./cmd/mcpgateway
./mcpgateway -config gateway.toml
```

## Example gateway.toml

```toml
[server]
name = "my-gateway"
instructions = "Gateway to prod and staging."

[[upstream]]
name = "prod"
kind = "stdio"
command = ["./some-mcp"]
env = { TOKEN = "{secret:PROD_TOKEN}" }
tools.prefix = "prod."
tools.allow = ["*"]
tools.desc_max = 200

[[upstream]]
name = "staging"
kind = "http"
url = "http://staging.internal:8080/mcp"
tools.prefix = "stg."

[secret]
name = "PROD_TOKEN"
env = "PROD_TOKEN"
```

## Flags

- `-config` path to TOML (default `gateway.toml`)
- Standard `-log-*` and `-transport` flags from cmdutil

For `streamable-http`/`sse` transports, a `/health` endpoint is also served on the same address for liveness checks.

## Limitations (scaffold)

- tools/listChanged notifications are received but only logged ("re-sync not implemented")
- Telemetry middleware is a no-op span stub; exporter wiring is minimal
- No resource/prompt proxying yet
- Collision detection happens at Build time; duplicate final names after prefixing are fatal

## Secrets

Secrets support `{secret:NAME}` interpolation in `env` and `headers` values. Sources: `value`, `env`, `file`, `command` (first non-empty wins, declaration order).
