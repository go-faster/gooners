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

## Tool discovery (`tools.lazy`)

An LLM client resends the whole tool list on every completion, so a large upstream costs context on
every turn. Set `tools.lazy = true` on an upstream to omit its tools from `tools/list` while keeping
them callable:

```toml
[[upstream]]
name = "grafana"
kind = "http"
url = "http://grafana.internal:8080/mcp"
tools.lazy = true
```

`[server] lazy_tools = true` does it for every upstream at once. Per-upstream `tools.lazy` then
overrides it in either direction, which is how you keep a handful of tools always in front of the
model:

```toml
[server]
lazy_tools = true

[[upstream]]
name = "grafana"          # inherits lazy_tools: hidden
kind = "http"
url = "http://grafana.internal:8080/mcp"

[[upstream]]
name = "ssh"              # opts back out: always in tools/list
kind = "stdio"
command = ["./ssh-mcp"]
tools.lazy = false
```

Lazy listing anywhere adds two gateway tools to the aggregate endpoint:

- `search_tools(query, limit)` — searches the catalog by name and description, returning names,
  owning upstream and truncated descriptions, but **no input schemas**. `hidden: true` marks a tool
  absent from `tools/list`. `query` is space-separated terms matched case-insensitively; all must
  match. An empty query returns the whole catalog.
- `describe_tools(names)` — returns the full definitions, input schemas included, of the named tools,
  identical to what `tools/list` reports. A client splices these into the tool set it sends to the
  model.

There is no per-session state: `tools.lazy` filters `tools/list` only and never blocks `tools/call`,
so a client can call a tool as soon as it has the schema. Both tools apply the same OAuth scope and
`tools.allow`/`tools.deny` filtering as `tools/list`, so they never reveal a tool the caller could
not otherwise see. Routed per-upstream endpoints are unaffected, and `search_tools`/`describe_tools`
become reserved names — an upstream tool that resolves to either is skipped with a warning.

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
