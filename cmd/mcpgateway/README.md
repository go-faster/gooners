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

## Request `_meta`

Whatever a client puts in a request's `_meta` reaches the upstream unchanged, so an upstream sees the
same client it would have seen without a gateway in front of it — including
`io.modelcontextprotocol/clientInfo` and `clientCapabilities`.

One key is dropped: `io.modelcontextprotocol/protocolVersion`. The protocol version is negotiated per
connection, and there are two of them here — client↔gateway and gateway↔upstream. Passing the
client's value through would tell an upstream a version it never agreed to, which a validating server
rejects outright.

## Flags

- `-config` path to TOML (default `gateway.toml`)
- `-config-watch-interval` poll the config file for changes at this interval; `0` (default) reloads on `SIGHUP` only
- Standard `-log-*` and `-transport` flags from cmdutil
- HTTP TLS flags: `-tls-cert-file`, `-tls-key-file`, and optional `-tls-client-ca-file` for mTLS

## Startup, `/health` and `/readyz`

The HTTP transport starts **before** upstreams are connected, so one slow or unresponsive upstream
cannot keep the gateway from answering at all. Until the initial build finishes, the gateway serves
an empty tool set; clients that connect early learn the real one through `listChanged`.

For `streamable-http`/`sse` transports, two probe endpoints are served on the same address:

| Endpoint | Answers | Fails when | Act on it by |
|---|---|---|---|
| `/health` | is the process up? | it stops listening | restarting |
| `/readyz` | can it serve its tools? | the initial build has not finished | keeping it out of rotation |

```jsonc
// 503 while starting
{"status": "not_ready", "server": "mcpgateway", "reason": "initial build has not finished"}
// 200 once built
{"status": "ready", "server": "mcpgateway"}
```

An upstream that is configured but currently unreachable does **not** make the gateway unready: its
supervisor keeps retrying and its tools appear when it returns. Failing readiness for it would take
a working gateway out of service over one broken dependency.

Startup is bounded even when an upstream misbehaves. Connecting is capped at 10s, and each feature
listing is capped too — by `call_timeout` when set, otherwise by the same 10s. That second bound
matters: `call_timeout` defaults to no limit, and an upstream that completes the handshake and then
never answers `tools/list` would otherwise park startup forever. Such an upstream is skipped with a
warning and retried in the background.

When an upstream has `[upstream.route]`, requests matching `host` and/or `path` are served by a per-upstream MCP server. Requests that do not match a route use the default aggregate gateway server.

Example HTTPS gateway with routed upstreams:

```bash
./mcpgateway -config gateway.toml -transport streamable-http -addr :8443 \
  -tls-cert-file server.crt -tls-key-file server.key
```

## Sharing files an upstream wrote (`blob_share`)

An upstream that produces a file usually returns its host path. That path is useless to an agent
that does not share the filesystem — in a container it names something the agent cannot open, and it
fails by handing back a plausible wrong answer rather than an error.

Bind-mount the upstream's output directory into the gateway and configure it as a mount. The gateway
then exposes one extra tool, `blob_share`, which turns a path into a URL:

```toml
[blob]
addr = "127.0.0.1:8090"                        # the store's own listener
base_url = "https://gw.example.com/blob"       # where the agent reaches it; optional if addr is local
ttl = "15m"

[[blob.mount]]
name = "example-mcp"
dir = "/mnt/example-mcp"                       # the directory as the gateway sees it
prefix = "/var/lib/example-mcp"                # the directory as the upstream reports it
```

The agent reads the path out of the upstream's own reply and hands it back:

```
example_mcp_get_file → "wrote /var/lib/example-mcp/out.png"
blob_share {"path": "/var/lib/example-mcp/out.png"}
  → url:  https://gw.example.com/blob/<id>/out.png
    blob: <id>
```

Nothing is copied: the file is served in place, and the reference expiring never deletes it. The URL
stops working after `ttl`; put the listener somewhere only the agent reaches, or behind a proxy that
authenticates, since the id is not an access control mechanism.

The result carries two references because it has two readers. `url` is for the agent, which has no
storage credentials. `blob` is the object's id, which another server configured against the same
store reads directly — that is what lets the file go on to, say, `ssh-mcp`'s `upload_file` without
the agent fetching and re-uploading the bytes. With the built-in listener the id is only meaningful
to this gateway; see the bucket backend below for the case where it is not.

A few things worth knowing before relying on it:

- **Nothing translates paths automatically.** `prefix` is how the gateway knows that
  `/var/lib/example-mcp/...` means its own `/mnt/example-mcp/...`. Omit `prefix` when the mount is at
  the same path on both sides.
- **The mount list is the boundary**, not the caller: a path outside every mount is refused, and each
  mount is confined to its directory, symlinks included.
- **Only the gateway serves them.** Upstreams do not need to know the store exists, which is the
  point — this works with any MCP server that writes files, unmodified.
- **The agent is told, twice.** With `[blob]` configured the gateway appends a sentence to its MCP
  `instructions` saying a returned host path is not a dead end, and `blob_share`'s own description
  names the configured mounts and their prefixes. An agent can therefore tell from `tools/list`
  whether a path it is holding is servable, without spending a call to find out. Changing
  `[[blob.mount]]` re-advertises the tool, so the list a client reads stays current across reloads.
- The listener is plaintext; front it with a TLS proxy and set `base_url` accordingly if it leaves
  the host.

Without `[blob]` the tool is not registered at all, and the name stays available to upstreams.

### Storing shared files in a bucket

The listener above is reachable from wherever its address is, and its ids mean nothing outside this
process. When the servers that should consume a shared file are not on the gateway's machine, point
the store at S3 instead:

```toml
[blob]
ttl = "15m"                                    # how long a presigned URL keeps working

[blob.s3]
endpoint = "minio.example.com:9000"            # host[:port] or an http(s) URL
bucket = "mcp-blobs"                           # must already exist
prefix = "tenants/alice"                       # key prefix; the tenancy boundary
# region = "us-east-1"                         # optional for MinIO

[[blob.mount]]
name = "playwright"
dir = "/mnt/playwright"
prefix = "/videos"
```

`endpoint` selects the backend, so `addr`, `base_url` and `dir` must not also be set — they configure
the other one, and the gateway refuses a section naming both. Credentials come from the environment
(`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`, `MINIO_ROOT_USER`/`MINIO_ROOT_PASSWORD`, or
`~/.aws/credentials`), never from the config file. The bucket is checked at startup, so a wrong
endpoint or a missing bucket fails to boot rather than at the first call.

What changes for the agent: `url` is now a presigned URL and needs no listener of the gateway's, and
`blob` is an id any server configured against the same bucket and prefix can read. That is the whole
of the cross-machine case — a Playwright container writes a video to a volume the gateway mounts,
`blob_share` uploads it, and a server on another host consumes the id.

`prefix` is the boundary between tenants: a store configured on `tenants/alice` can neither read nor
write outside it, whatever id it is handed. Give each user their own, and note that everything under
one prefix is shared — the gateway is one process serving all of its clients.

Objects are uploaded rather than served in place, so they occupy the bucket after `ttl` expires the
URL. Set a lifecycle rule on the prefix; the gateway does not delete anything.

**An unreachable bucket does not stop the gateway.** The store is built once at startup so a wrong
bucket or credential is logged immediately, but a failure only disables `blob_share` — every proxied
upstream keeps serving. The store is rebuilt on the next call, so once the endpoint is back the tool
starts working without a restart.

## Config reload

The gateway reloads `-config` in place on `SIGHUP`, and — when `-config-watch-interval` is set —
whenever the file's contents change. Downstream clients keep their sessions: the gateway applies the
new config to the running MCP server and lets the usual `listChanged` notifications carry the new
tool set, so no client has to reconnect.

```bash
kill -HUP $(pidof mcpgateway)                              # reload now
./mcpgateway -config gateway.toml -config-watch-interval 10s  # reload on change
```

What a reload does:

- **Invalid config is rejected before anything changes.** A config that does not parse or does not
  validate leaves the running one serving; the failure is logged and counted.
- **Unchanged upstreams keep their live session.** An upstream is closed and reconnected only when
  its own `[[upstream]]` section changed, when a `{secret:...}` it interpolates changed, or when the
  global `[redact]` section it inherits changed — all three are baked in at connect time.
- **Added upstreams connect, removed upstreams are unregistered** along with their route. An added
  upstream that is unreachable right now is still adopted; its supervisor retries in the background.

What a reload does **not** apply — these are reported in the logs as needing a restart, and the old
values keep running:

- `[server]` — the gateway's MCP identity is handed to the transport at startup
- `[auth]` — the HTTP middleware chain is built once
- telemetry — exporters are wired at startup from the environment (see below)
- toggling `tools.lazy` on or off across the whole config, which decides whether the discovery tools
  and lazy middleware are installed on the server
- `[blob]` and `[blob.s3]` except the mounts — the store is built once, from a listener and a
  `base_url` or from a bucket it authenticated against at startup. `[[blob.mount]]` is not in this
  list: which directories are shared does reload

Metrics:

| Metric | Type | Attributes |
|---|---|---|
| `mcpgateway.config.reload` | counter | `result` = `success` \| `failure` |
| `mcpgateway.config.reload.last_success_timestamp` | gauge (unix seconds) | — |
| `mcpgateway.upstreams` | gauge | `state` = `connected` \| `disconnected` |

`last_success_timestamp` is seeded at startup, so `now() - last_success_timestamp` is a usable
staleness alert from the first scrape — a gateway stuck on a config it cannot replace looks stale
rather than merely quiet. `mcpgateway.upstreams` separates "not configured" from "configured but
unreachable", since a disconnected upstream is still being retried by its supervisor.

## Graceful shutdown

Closing an upstream — on removal, on restart, or at process shutdown — drains it first: it stops
accepting new calls, waits for the in-flight ones to finish, and only then tears the session down.
Upstreams close concurrently, so a reload removing several waits out one drain rather than their sum.

Two independent timeouts are involved, and they are deliberately not the same knob:

| Setting | Scope | Default | Bounds |
|---|---|---|---|
| `[[upstream]] call_timeout` | per upstream | unset = no limit | one request to that upstream |
| `[server] drain_timeout` | gateway | `5s` | each phase of closing an upstream |

A feature listing is the one request that is never unbounded: it falls back to the connect timeout
(10s) when `call_timeout` is unset, because it runs during startup and after every reconnect, before
the gateway can serve anything.

An upstream with genuinely long-running tools sets no `call_timeout` and still gets a bounded
shutdown: `drain_timeout` does not depend on calls being short. Set `call_timeout` only where you
want a call itself capped. A negative `drain_timeout` disables draining and cuts in-flight calls off
immediately.

`drain_timeout` also bounds the session close, not just the wait before it. The MCP SDK's session
close waits for outstanding calls, so an upstream that never answers would otherwise hang shutdown
forever regardless of the timeout. When it stalls, the close is abandoned and the transport is torn
down underneath it: for stdio the child process is killed, and for http/sse the in-flight requests
are canceled. Without that last part an http upstream that accepts a request and never answers
would leak the request, its goroutine and its connection for the life of the process, since these
clients carry no timeout by design (both transports stream).

Polling compares a content hash rather than mtime or inotify events, which is what makes it work for
atomic rename, ConfigMap symlink swap, and editor save-with-backup, while a rewrite with identical
bytes does not churn upstream connections. A `SIGHUP` reloads unconditionally, since the secrets the
file references may have changed even when the file did not.

## Telemetry

Traces and metrics are configured **entirely from the environment**, by `go-faster/sdk`'s
`app.Run`. There is no `[telemetry]` section in the config file; one written there is ignored, since
unknown TOML keys are not an error.

```bash
OTEL_SERVICE_NAME=mcpgateway            # without it the SDK falls back to its own default,
                                        # and the gateway is unidentifiable in the backend
OTEL_EXPORTER_OTLP_ENDPOINT=http://otelcol:4317
OTEL_EXPORTER_OTLP_PROTOCOL=grpc
OTEL_EXPORTER_OTLP_INSECURE=true
```

Every proxied tool call is wrapped in a span and counted, per upstream. The gateway also exports the
reload and upstream metrics listed under [Config reload](#config-reload).

## Limitations

- Collision detection happens at Build time; duplicate final names after prefixing are fatal

## Secrets

Secrets support `{secret:NAME}` interpolation in `env` and `headers` values. Sources: `value`, `env`, `file`, `command` (first non-empty wins, declaration order).
