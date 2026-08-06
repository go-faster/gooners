# AGENTS.md / CLAUDE.md

This file provides guidance to AI coding assistants (like Claude Code, OpenCode, Cursor, and Copilot) when working with code in this repository.

## Purpose

`github.com/go-faster/gooners` is a monorepo for AI tools, skills, and MCP (Model Context Protocol) servers built on go-faster technologies.

## Module

```
module github.com/go-faster/gooners
go 1.26.3
```

Note: `cmd/mcpgateway/` is the MCP gateway proxy binary (aggregates multiple upstream MCP servers).

## Commands

```bash
# Build all packages
go build ./...

# Run

# Run all tests, including race detector
go test -race ./...

# Run a single test
go test ./path/to/pkg -run TestName

# Lint and format
golangci-lint fmt ./...
golangci-lint run ./...
```

## Architecture

```
cmd/ssh-mcp/          ← MCP server binary (go build ./cmd/ssh-mcp)
cmd/grafana-dashboard-mcp/ ← MCP server binary (go build ./cmd/grafana-dashboard-mcp)
cmd/alertmanager-mcp/ ← MCP server binary (go build ./cmd/alertmanager-mcp)
cmd/gitlab-mcp/       ← MCP server binary (go build ./cmd/gitlab-mcp)
internal/
  effect/             ← The fs/HTTP effect providers every agent-reachable side effect goes through.
                        effect.Root(dir) is a filesystem confined to dir, backed by os.Root (so a symlink
                        planted inside dir cannot lead out of it); effect.Deny(reason) refuses everything;
                        effect.OS() is unconfined and belongs only where paths are the operator's or a
                        test's. effect.NewHTTPClient(HTTPOptions) applies an egress HTTPPolicy on the
                        request, on redirects, and on the post-DNS resolved IP. See "effect providers" below.
  mcputil/            ← Standardized MCP server config, prompts, and log streaming
  session/            ← SSH session pool & async upload tracking. PoolOptions.LocalFS is the one gate on
                        host files a tool can reach; PoolOptions.SpoolFS holds overflow output.
                        Pool.OpenSpool/SaveSpool move spool content without ever handing a tool a host path.
  sshutil/            ← SSH config / known-hosts helpers
  tools/              ← MCP tool registrations
    core/             ← ssh_open, ssh_exec, ssh_close, ssh_once_exec, ssh_ping, ssh_read_output, ssh_save_output
    disk/             ← disk_df, disk_lsblk, disk_mounts
    fs/               ← ls, cat, find, grep, stat, du, truncate, upload_file, write_file
    gitlab/           ← issue_*, mr_*, release_*, repo_* (see "gitlab-mcp" below)
    grafana/          ← add_dashboard, add_panel, add_query, export_dashboard, etc.
    proc/             ← proc_list, proc_info, proc_lsof, proc_kill
    sysinfo/          ← sys_mem, sys_net_addrs, sys_os_info, sys_uptime
    systemd/          ← systemctl_* tools
skills/jx/            ← Agent skill for github.com/go-faster/jx
```

The `ssh-mcp` file in the repo root is a **compiled binary** (not a source directory) — ignore it when navigating source.

### Effect providers (issue #22)

Filesystem and HTTP side effects go through a provider from `internal/effect`, and the provider — not the
call site — enforces policy. This is a security invariant, not a style preference.

- **Never call `os.Open`/`os.Create`/`os.WriteFile`/… in a tool handler or in `internal/session`.** Take an
  `effect.FS` and call it. The one place a raw `os.*` call is still correct is operator-controlled startup
  paths (gateway TOML, known_hosts, SSH keys, CA bundles, log file), which no agent can influence.
- **Never construct an `http.Client` inline.** Take an `effect.Doer`, or build one with
  `effect.NewHTTPClient`, whose `HTTPPolicy` allowlist is derived from the configured upstream
  (`effect.AllowHostOf`). The zero-value policy allows nothing, so an unconfigured client fails closed.
- **Do not add a path check to a tool handler.** A handler passes the agent's path straight to the pool;
  `session.PoolOptions.LocalFS` decides whether it is reachable. A per-handler check is how the
  `ssh_save_output` arbitrary-write bug happened: `upload_file`/`download_file` remembered to call the old
  lexical `fs.WithinDir` helper and `ssh_save_output` did not. `WithinDir` is gone; do not reintroduce it.
  `effect.FS.Resolve` exists only to fail fast with a legible error and is explicitly *not* the gate.
- A binary declares what it may touch by what it passes to `session.NewPool`. `ssh-mcp` passes
  `LocalFS: effect.Root(cwd)`.

### mcpgateway config reload (issue #26)

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

### mcpgateway startup

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

### mcpgateway request `_meta`

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

## Key Dependencies

- `github.com/modelcontextprotocol/go-sdk` — MCP server/tool SDK; all tool registrations call `mcp.NewServer` and pass a `session.Pool` or local state.
- `github.com/grafana/grafana-foundation-sdk/go` — Official Go SDK for Grafana dashboard schema definitions and builders.

## ssh-mcp Build

```bash
go build ./cmd/ssh-mcp
# Run with default stdio transport (for Claude Code / Claude Desktop):
./ssh-mcp
# Or HTTP transport with debug logging:
./ssh-mcp -transport streamable-http -addr :8080 -log-file /tmp/ssh-mcp.log
```

## grafana-dashboard-mcp Build

```bash
go build ./cmd/grafana-dashboard-mcp
# Run with default stdio transport:
./grafana-dashboard-mcp
# Or HTTP transport with debug logging and custom session dir:
./grafana-dashboard-mcp -transport streamable-http -addr :8081 -sessions-dir /tmp/sessions -log-file /tmp/grafana-mcp.log
```

## alertmanager-mcp Build

```bash
go build ./cmd/alertmanager-mcp
# Run with default stdio transport:
./alertmanager-mcp
# Or HTTP transport with debug logging:
./alertmanager-mcp -transport streamable-http -addr :8082 -log-file /tmp/alertmanager-mcp.log
```

## gitlab-mcp Build

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

`gitlab-mcp` deliberately does **not** wrap the `glab` CLI. It calls
`gitlab.com/gitlab-org/api/client-go` directly, which is what lets every tool take a `project`
argument; glab's own MCP server hides `--repo` from its tool schemas and so requires a checkout.
Keep this property when adding tools:

- **Every tool takes `project`**, optional only when `Config.DefaultProject` is set. Never resolve a
  project from the working directory or a git remote.
- **Return a compact summary type, not the API struct.** `gl.Issue` has ~40 fields; `IssueSummary` has
  the ones an agent uses. Cap anything unbounded (descriptions, file contents, diffs) and set an
  explicit `*_truncated` field rather than silently cutting.
- Do not add merge, approve, or delete tools. The absence is the design.
- **The credential may vary per session; the instance URL may not.** `-auth=client` takes each caller's
  token off a `PRIVATE-TOKEN`/`Authorization` header and `ClientSet` builds a `Client` per token. Never
  extend this to let a caller choose the host: the server would then send its own token, or another
  user's, to an attacker-named destination. `effect.AllowHostOf(cfg.BaseURL)` is what pins it.
- A tool handler must take its `*Client` from the closure it was registered with — that is the session's
  credential. Do not reach for a package-level or `Config`-level token inside a handler.
- Release asset tools reach host files only via `Config.FS`; a nil FS means `effect.Deny`. Asset
  downloads follow a URL that project content chose, so they rely on the HTTP client's allowlist.
- Test fixtures for issues **must include `"id"`**: `gl.Issue.UnmarshalJSON` calls
  `reflect.TypeOf(raw["id"]).Kind()` unguarded and panics without it.

## Skills

Skills follow the [Agent Skills](https://agentskills.io) standard. Each skill lives in its own directory under `skills/`:

```
skills/
└── <name>/
    └── SKILL.md   ← YAML frontmatter (name, description) + instructions
```

- Add new skills as `skills/<name>/SKILL.md`.
- The `description` field drives automatic invocation — make it specific and include all relevant trigger contexts.
- Update the Skills table in `README.md` when adding or removing a skill.

## Commits

- Commit generated files (golden files, mocks, protobuf output, etc.) in a separate commit from the code that produces them. If it's unclear whether a file is generated, ask before bundling it.
- Conventional commits format: `type(scope): subject` (e.g., `fix(chstorage): fix column mapping`).
- Keep commit message body lines at 100 characters or fewer.

## Testing

- Tests must not rely on runner state
- Tests should be fast and not rely on real time passing (e.g. avoid long sleeps or timeouts), when possible. Use mocks or fakes instead, or `testing/synctest` bubbles if necessary.
- Tests should not be flaky
- Tests should be hermetic: use `t.TempDir()` for temporary files (auto-cleaned) and `t.Cleanup()` for any other teardown
- Tests should be cross-platform where possible (e.g. avoid Linux-specific syscalls or file paths)
- Tests on unsupported platforms (like Windows) should be skipped with a clear message
- E2E tests must use `testcontainers-go` for any external dependencies (like SSH servers) and clean up after themselves
- E2E tests must be skipped if `-short` is passed

## Go Standards

- Use `golangci-lint fmt ./...` for formatting (not `gofmt` or `goimports` directly).
- We use golangci-lint v2; do **not ever** `go install` golangci-lint (or any variant) — assume the binary is already available in $PATH (via nix, system package, or dev env).
- Avoid loading generated files into context; use search tools or gopls instead.
- Pass optional constructor parameters as a dedicated options struct, not as individual arguments. Use the `<Type>Options` + `(<Type>Options).setDefaults()` pattern:

```go
type FooOptions struct {
    Timeout time.Duration
    Logger  *slog.Logger
}

func (o *FooOptions) setDefaults() {
    if o.Timeout == 0 {
        o.Timeout = 30 * time.Second
    }
    if o.Logger == nil {
        o.Logger = slog.Default()
    }
}

func NewFoo(ctx context.Context, opts FooOptions) *Foo {
    opts.setDefaults()
    // ...
}
```

## README

- Keep `README.md` up to date whenever a tool or skill is added, removed, or renamed.
- Don't forget to update `README.md` in subdirectories (like `cmd/ssh-mcp/README.md`) when making changes to tools or skills.
- Update `CLAUDE.md` with any new instructions or guidelines for AI coding assistants.
- Tools go in the **Tools** table; skills go in the **Skills** table.
- Keep the **Tools** and **Skills** tables accurate and comprehensive.

## Releases

- When adding, removing, or renaming a binary under `cmd/`, update `.goreleaser.yaml`. A binary that
  builds and passes CI is still absent from every release artifact until it is listed there, and
  nothing fails to tell you — this is why `gitlab-mcp` shipped without it.
- Each binary appears in **four** places; adding it to only some is the failure mode:
  1. `builds` — the binary itself
  2. `dockers_v2` — the `ghcr.io/go-faster/gooners/<name>` image, including its registry buildcache flags
  3. `release.footer` — the image list in the release notes
  4. `nfpms` — the apk/deb/rpm/archlinux packages
- Verify with `goreleaser check`, then `goreleaser build --snapshot --clean --id <name> --single-target`.
  `check` only validates the schema; it cannot know a binary is missing.

## GitHub Labels

- When adding, removing, or renaming a component under `cmd/` or `skills/`, update the GitHub component label set if repository access allows it.
- Component labels use `component:cmd/<name>` and `component:skill/<name>`; repository-wide issues use `component:gooners`.
- If label updates were needed but could not be performed, state that explicitly to the user before finishing.
