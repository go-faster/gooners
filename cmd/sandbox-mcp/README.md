# sandbox-mcp

An MCP server that gives every session a **fresh, isolated container** instead of a static SSH host. It reuses `ssh-mcp`'s own session pool and a curated subset of its tool registrations (`ssh_exec`, `ssh_sudo_exec`, ...), and adds container lifecycle underneath them: `sandbox_open` creates and starts a container, injects a static SSH+SFTP agent binary into it, and dials it — all over the container's `exec`/`attach` stdio stream, never the network. `sandbox_close` (or `ssh_close`) tears the container down.

## Why this exists

`ssh-mcp` connects to one static, long-lived host per deployment. Every MCP client sharing that upstream gets the *same* shell: state leaks across unrelated conversations, and a wedged session can only be fixed by restarting the host for everyone. `sandbox-mcp` gives each `sandbox_open` call its own container, torn down on session close (or after an idle timeout).

## Security model

- **No sandbox escape.** `sandbox-mcp` never registers `ssh_open`, `ssh_open_cfg`, or `ssh_once_exec` — those would let the sandboxed agent SSH *out* to any host this process can reach.
- **No token leak.** `sandbox-mcp` never registers `ssh_list` — it would return every session in the process (including other conversations' sandbox capability tokens) to every caller.
- **Isolation is by capability token.** Each `sandbox_open` call returns an unguessable `session_id`; there is no tool to enumerate or reach anyone else's sandbox. A leaked `session_id` grants access until the idle timeout expires — treat it like a credential.
- **Network is `none` by default**: the sandbox container has no NIC at all. SSH still works because it rides the container's `exec`/`attach` stdio stream, not the network. Opt into `open` (full egress) only if you need it, and only if your Policy (`-sandbox-network`) allows it.
- **Never mount the Docker socket into a sandbox**, and hardening (`CapDrop: ["ALL"]`, `NoNewPrivileges`, memory/CPU/pids limits) always comes from server-side Policy — a caller's `sandbox_open` request can never widen it.

## Build

```bash
go build ./cmd/sandbox-mcp
go build ./cmd/sandbox-agent  # the in-container SSH+SFTP agent sandbox-mcp injects
```

`sandbox-mcp` needs a Docker daemon (rootful or rootless) reachable at `-docker-host` (or `$DOCKER_HOST`), and a `sandbox-agent` binary for every architecture your sandbox images run on, laid out as `<-sandbox-agent-path>/<arch>/sandbox-agent` (e.g. `.../amd64/sandbox-agent`, `.../arm64/sandbox-agent`). The goreleaser-built Docker image for `sandbox-mcp` ships both.

## Docker

```bash
docker build --target sandbox-mcp -t sandbox-mcp .
```

Run, mounting the host's Docker socket so `sandbox-mcp` can talk to the daemon:

```bash
docker run --rm -i \
  -v /var/run/docker.sock:/var/run/docker.sock \
  sandbox-mcp -transport streamable-http -addr :8083
```

See [examples/sandbox-mcp](../../examples/sandbox-mcp) for a docker-compose setup.

Notes:
- The image runs as a non-root user (`mcp`). If the mounted Docker socket isn't world-writable, add
  `--group-add $(stat -c %g /var/run/docker.sock)` (or run the container as `root`) so `mcp` can reach it.

## Flags

- `-transport <stdio|streamable-http|sse>` — protocol to use (default: `stdio`)
- `-addr <host:port>` — listen address for HTTP transports. Default `:8080`
- `-log-file <path>` — write structured debug logs to the given file.
- `-disable-sudo` — do not register `ssh_sudo_exec`.
- `-command-timeout <duration>` — default command timeout (default `10s`).
- `-docker-host <endpoint>` — Docker daemon endpoint (e.g. `unix:///var/run/docker.sock`); empty uses `$DOCKER_HOST`.
- `-sandbox-image <image>` — default sandbox image (default `alpine:latest`).
- `-sandbox-allowed-images <glob,glob,...>` — `path.Match` glob patterns of images `sandbox_open` may request; empty allows only `-sandbox-image`.
- `-sandbox-network <tier,tier,...>` — network tiers `sandbox_open` may request: `none` (default), `open`. `egress-proxy` is defined but not implemented yet.
- `-sandbox-memory <bytes>` — memory limit per sandbox (default 512MiB).
- `-sandbox-cpus <n>` — CPU limit per sandbox (default `1`).
- `-sandbox-pids-limit <n>` — pids limit per sandbox (default `256`).
- `-sandbox-runtime <name>` — alternative container runtime (e.g. `runsc` for gVisor, `kata` for Kata Containers); empty uses the daemon default.
- `-sandbox-user <user>` — user the sandboxed process runs as; empty uses the image default.
- `-sandbox-idle-timeout <duration>` — how long a sandbox may sit with no SSH activity before it's torn down (default `15m`).
- `-sandbox-agent-path <dir>` — base directory of per-architecture `sandbox-agent` binaries (default: a well-known path baked into the Docker image).
- `-sandbox-deployment <name>` — deployment name scoping sandbox container labels, so two `sandbox-mcp` processes sharing one Docker host never reap each other's sandboxes; defaults to the hostname.

## Tools

`sandbox_open` and `sandbox_close` are the only tools this server adds. Everything else is a deliberately
small slice of `ssh-mcp`'s tool implementations, pointed at a sandbox's SSH session instead of a static
host — the sandbox is a disposable, fully exec'able container, so `ssh_exec` covers what a static-host
deployment would otherwise need specialized read-only tools for.

| Tool | Description |
|------|-------------|
| `sandbox_open` | Create a fresh, isolated sandbox container and return a `session_id` that works with every other tool below |
| `sandbox_close` | Tear down a sandbox: close its SSH session and destroy its container |
| `ssh_close` | Same effect as `sandbox_close` (closes any session by ID) |
| `ssh_exec` / `ssh_sudo_exec` | Run commands in the sandbox |
| `ssh_ping` | Check a sandbox session is alive |
| `ssh_read_output` | Read truncated command output |

**Deliberately not registered**:
- `ssh_open`, `ssh_open_cfg`, `ssh_once_exec` — sandbox escape: they'd let the sandboxed agent SSH *out* to
  any host this process can reach.
- `ssh_list` — token leak: it returns every session in the process, including other conversations' sandbox
  capability tokens.
- `ssh_list_machines`, and `systemd`/`journald` tools — meaningless inside a container.
- `ls`, `cat`, `grep`, `find`, `stat`, `du`, `truncate`, `write_file` (the `fs` package's SFTP-backed file
  tools) — redundant with `ssh_exec` against a disposable full container.
- `upload_file`, `download_file` — also from the `fs` package, but dropped for a security reason, not just
  redundancy: their "local path" is on the **sandbox-mcp host process**, not inside the container, and every
  sandbox shares one `uploadRoot` directory. Two unrelated sandboxes could read/write into that same host
  directory — a covert channel between sandboxes that are supposed to be isolated from each other.
- `proc_list`, `proc_info`, `proc_lsof`, `proc_kill`, `disk_lsblk`, `disk_df`, `sys_uptime`, `sys_mem`,
  `sys_net_addrs`, `sys_os_info` — pure surface reduction: these are exec-based, remote-scoped wrappers that
  add nothing a sandboxed `ssh_exec` can't already do; they exist for real hosts where an operator wants a
  fine-grained tool subset (see `ssh-mcp`), not for a disposable per-session sandbox.
- `ssh_save_output` — redundant: `internal/session/pool_handlers.go`'s `closeSession` already deletes every
  spool file and the session's whole tempdir on every teardown path (explicit close, idle sweep, and
  shutdown), so there is nothing left to persist that isn't already cleaned up automatically.
