# ssh-mcp

An MCP server that exposes SSH and SFTP operations as tools for AI agents. Designed for developer and admin workflows — run it on your local machine and let an AI assistant manage remote hosts through named sessions.

## Security notes

- **Known hosts**: host key verification is enforced against `~/.ssh/known_hosts` by default. Populate it before connecting or pass an explicit `known_hosts` path via `ssh_open_cfg`. Passing `"insecure"` disables verification entirely — avoid this.
- **Upload root**: `upload_file` and `download_file` only access files within the directory where the server process was started. Paths outside that directory are rejected.
- **Ambient credentials**: `ssh_open` and `ssh_once_exec` use your SSH agent and `~/.ssh` key files automatically — the same keys your shell would use.
- **Execution**: `ssh_exec` and `ssh_sudo_exec` run arbitrary shell commands with the privileges of the SSH user. Only connect to hosts you trust.

## Build

```bash
go build ./cmd/ssh-mcp
```

## Docker

Pre-built or build yourself:

```bash
docker build -t ssh-mcp .
```

Run (stdio, for local MCP clients that support Docker stdio):

```bash
docker run --rm -i \
  -v "$HOME/.ssh:/home/mcp/.ssh:ro" \
  -v "$(pwd):/work" -w /work \
  ssh-mcp
```

Run with streamable-http (recommended for Docker; exposes on host port 8080):

```bash
docker run --rm -p 8080:8080 \
  -v "$HOME/.ssh:/home/mcp/.ssh:ro" \
  -v "$(pwd):/work" -w /work \
  ssh-mcp -transport streamable-http -addr :8080
```

Then point your client at `http://localhost:8080/mcp`.

Notes:
- Mount your `~/.ssh` read-only so `ssh_open` / `ssh_once_exec` can use your keys and `~/.ssh/config`.
- The container working directory (`/work` above) becomes the upload root for `upload_file` / `download_file`.
- For sudo password from Docker secret: `-sudo-password-file /run/secrets/sudo_pass` (mount secret or use `--secret` with BuildKit).
- Non-root user `mcp` (uid 100) inside container.

## Flags

- `-transport <stdio|streamable-http|sse>` — protocol to use (default: `stdio`)
- `-addr <host:port>` — listen address for HTTP transports (`streamable-http`, `sse`). Default `:8080`
- `-log-file <path>` — write structured debug logs (slog TextHandler) to the given file in append mode.
- `-disable-sudo` — do not register the `ssh_sudo_exec` tool. Useful when deploying to untrusted contexts to reduce the capability surface.
- `-command-timeout <duration>` — default command timeout (default: `10s`).

### Sudo password sources

Instead of passing `sudo_password` in every `ssh_sudo_exec` call, you can configure a server-level source. Exactly one of the following flags may be set:

| Flag | Source | Re-read? |
|------|--------|----------|
| `-sudo-password-file <path>` | Contents of a file (leading/trailing newline stripped) | Every call |
| `-sudo-password-env <VAR>` | Value of an environment variable | Every call |
| `-sudo-password-cmd <cmd>` | stdout of a command (e.g. `pass show host/sudo`) | Once, then cached |

```bash
# From a file (e.g. Docker secret or tmpfs)
./ssh-mcp -sudo-password-file /run/secrets/sudo_pass

# From an env var
SUDO_PASS=hunter2 ./ssh-mcp -sudo-password-env SUDO_PASS

# From a credential helper
./ssh-mcp -sudo-password-cmd "pass show myserver/sudo"
./ssh-mcp -sudo-password-cmd "secret-tool lookup service ssh-mcp account sudo"
```

The per-call `sudo_password` field always takes precedence over the server-level source when both are provided.

**Recommendation**: `stdio` is the standard MCP transport and works perfectly with all standard clients (Claude Desktop, Claude Code, Cursor). `streamable-http` and `sse` are also available if you prefer connecting via HTTP without using the `mcp` CLI wrapper.

Examples:

```bash
# Default (stdio) – best for simple local use
./ssh-mcp

# Streamable HTTP – recommended for Claude and most modern clients
./ssh-mcp -transport streamable-http -addr :8080 -log-file /tmp/ssh-mcp.log

# SSE (older alternative)
./ssh-mcp -transport sse -addr :8080
```

## Claude Code setup

### Streamable HTTP (recommended)

Start the server:

```bash
./ssh-mcp -transport streamable-http -addr :8080
```

**`~/.claude.json`** (or project `.claude.json`):

```json
{
  "mcpServers": {
    "ssh": {
      "type": "streamable-http",
      "url": "http://localhost:8080/mcp"
    }
  }
}
```

### Stdio (simple local use)

```json
{
  "mcpServers": {
    "ssh": {
      "command": "/path/to/ssh-mcp",
      "args": ["-log-file", "/tmp/ssh-mcp.log"]
    }
  }
}
```

Or using `claude mcp add` (from the directory you want as upload root):

```bash
claude mcp add ssh /path/to/ssh-mcp
```

> **Note**: The stdio transport is compatible with all modern clients including Claude Code and Claude Desktop.

The server's **working directory at launch** becomes the upload root for `upload_file` and `download_file`.

## Known hosts setup

If `~/.ssh/known_hosts` does not already contain your target hosts, connect to them once via plain SSH first:

```bash
ssh user@host exit
```

This populates `known_hosts` and the MCP server will accept subsequent connections to those hosts.

Alternatively, use `ssh_open_cfg` with `known_hosts` pointing to a file you manage manually, or pre-populate it with `ssh-keyscan`:

```bash
ssh-keyscan host >> ~/.ssh/known_hosts
```

