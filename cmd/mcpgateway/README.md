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

# Optional: expose this upstream as its own MCP endpoint in addition to the
# aggregate gateway endpoint. Routed endpoints use the upstream's original
# tool/prompt names, while the aggregate endpoint still uses tools.prefix.
[upstream.route]
host = "staging-mcp.example.com"
path = "/staging"

[secret]
name = "PROD_TOKEN"
env = "PROD_TOKEN"
```

## Flags

- `-config` path to TOML (default `gateway.toml`)
- Standard `-log-*` and `-transport` flags from cmdutil
- HTTP TLS flags: `-tls-cert-file`, `-tls-key-file`, and optional `-tls-client-ca-file` for mTLS

For `streamable-http`/`sse` transports, a `/health` endpoint is also served on the same address for liveness checks.

When an upstream has `[upstream.route]`, requests matching `host` and/or `path` are served by a per-upstream MCP server. Requests that do not match a route use the default aggregate gateway server.

Example HTTPS gateway with routed upstreams:

```bash
./mcpgateway -config gateway.toml -transport streamable-http -addr :8443 \
  -tls-cert-file server.crt -tls-key-file server.key
```

## Limitations

- Telemetry middleware is a no-op span stub; exporter wiring is minimal
- Collision detection happens at Build time; duplicate final names after prefixing are fatal

## Secrets

Secrets support `{secret:NAME}` interpolation in `env` and `headers` values. Sources: `value`, `env`, `file`, `command` (first non-empty wins, declaration order).
