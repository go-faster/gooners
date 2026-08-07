# internal/gateway

Guidance for the mcpgateway proxy implementation. The binary that wires it up is
`cmd/mcpgateway`; repo-wide rules are in the root `CLAUDE.md`.

## Startup

- **Nothing an upstream does may block the gateway from listening.** The transport starts before
  `Build`, and `Build` runs alongside it; the reloader is sequenced *after* `Build` so a SIGHUP
  cannot race the registration it would replace.
- **Every pre-serving upstream request is bounded.** `withCallTimeout` is for tool calls and is
  deliberately unlimited by default; feature listings use `withListTimeout`, which falls back to the
  connect timeout. Switching a `List*` method back to `withCallTimeout` reintroduces a gateway that
  never starts when an upstream stalls after the handshake — `TestListTimeoutBoundsListing` guards it.
- **`/health` is liveness, `Gateway.Ready` behind `/readyz` is readiness.** Readiness means the
  initial `Build` finished, nothing more. Do not make it depend on upstreams being connected: they
  reconnect on their own, and one broken dependency must not pull a working gateway out of rotation.

## Config reload (issue #26)

`Gateway.Reload` applies a new `*Config` to the running gateway; *where that config comes from* is
not its problem. Keep the split:

- **`Source` supplies configs, `Reloader` decides when and reports the outcome, `Gateway` only
  applies.** Do not add file paths, poll intervals, signal handling, or reload metrics to `Gateway`
  — that state belongs in `FileSource`/`Reloader`, which are testable without a gateway.
- **Reload must never take the running config down.** Validate, build the secret resolver, the
  redactor and every new `*Upstream` *before* mutating live state, so a bad config leaves the
  previous one serving. A reload that half-applies is worse than one that is refused.
- **Do not swap `g.server`.** Downstream sessions are bound to it; the whole point of reload is that
  clients keep their session and learn the new tool set through `listChanged`. Detaching an upstream
  therefore syncs it against an empty feature set (the normal removal path) instead of rebuilding.
- A config section that cannot be applied in place goes in `restartRequired`, never silently
  ignored. Anything captured once at startup (server identity, HTTP middleware, telemetry exporters,
  whether the lazy middleware is installed) is in that category.
- Reloadable state on `Gateway` (`cfg`, `resolver`, `upstreams`) is guarded by `stateMu`; read it
  through `config()`/`secretResolver()`/`upstreamList()`, never the field directly.

## blob_share

`blob_share` turns a host path an upstream reported into a URL, for the case where an upstream
writes to a directory the gateway sees through a bind mount. Upstreams need no changes.

- **The store is passed in through `Options.Blob`; the mounts come from config.** The store owns a
  listener and mints URLs from `base_url`, which a live reload cannot move, so `[blob]` minus its
  mounts is in `restartRequired`. `[[blob.mount]]` is a table the tool reads per call and reloads.
- **Nothing translates paths.** `prefix` is the only reason the gateway knows an upstream's
  `/var/lib/x/out.png` is its own `/mnt/x/out.png`. Do not add path guessing, and do not fall back to
  what happens to exist on the gateway's filesystem.
- **Do not rewrite paths out of tool results.** It was considered and rejected: matching paths in
  prose rewrites them inside error messages and code snippets, and misses upstreams that never print
  one. If this is ever added, it must key off declared structured-content fields, never a regex.
- `reservedTool` gates each gateway tool name on its own feature. A gateway without a blob store must
  not reserve `blob_share` against an upstream that happens to use the name.
- **The mount list belongs in the tool description, not in the instructions.** Instructions are handed
  to the transport once at startup while `[[blob.mount]]` reloads in place, so a list embedded there
  goes stale; `blobShareDescription` builds it and `Reload` re-registers the tool when the mounts
  change, which emits `listChanged`. `blobInstructions` stays a fixed pointer sentence naming no
  path — keep it that way.

## Request `_meta`

A downstream request's `_meta` is forwarded to the upstream **as is**, so an upstream sees the same
client it would have seen with no gateway in the path — `clientInfo` and `clientCapabilities`
included. The gateway cannot upgrade the client, and hiding it from upstreams would only make them
expect something that is not there.

The exception is `io.modelcontextprotocol/protocolVersion`, which `proxyMeta` strips. That value
describes one hop: the gateway negotiates separately with the client and with each upstream, so
forwarding the client's version announces one that was never agreed on that connection. SDK v1.7.0
validates it and rejects the request. Route every request `_meta` through `proxyMeta`; do not pass
`req.Params.Meta` straight into an upstream call, and do not extend the strip list to keys that are
genuinely about the client.

The `blob` package itself has its own guide in `blob/CLAUDE.md`.
