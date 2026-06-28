# ssh-mcp

An MCP server that exposes SSH and SFTP operations as tools for AI agents. Designed for developer and admin workflows — run it on your local machine and let an AI assistant manage remote hosts through named sessions.

## Security notes

- **Known hosts**: host key verification is enforced against `~/.ssh/known_hosts` by default. Populate it before connecting or pass an explicit `known_hosts` path via `ssh_open_cfg`. Passing `"insecure"` disables verification entirely — avoid this.
- **Working directory guard**: `upload_file`, `download_file`, and `stdin_file` for exec tools only access files within the directory where the server process was started. Paths outside that directory are rejected.
- **Ambient credentials**: `ssh_open` and `ssh_once_exec` use your SSH agent and `~/.ssh` key files automatically — the same keys your shell would use.
- **Execution**: `ssh_exec` and `ssh_sudo_exec` run arbitrary shell commands with the privileges of the SSH user. Only connect to hosts you trust.

## Build

```bash
go build ./cmd/ssh-mcp
```

## Docker

Pre-built or build yourself:

```bash
docker build --target ssh-mcp -t ssh-mcp .
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
- The container working directory (`/work` above) becomes the file access root for `upload_file`, `download_file`, and exec `stdin_file`.
- `LocalForward` directives from `~/.ssh/config` are opened when a matching SSH session opens and closed with that session.
- For passwords from Docker secret: `-password-file /run/secrets/ssh_pass` (mount secret or use `--secret` with BuildKit). The same file is used for both SSH login and sudo.
- Non-root user `mcp` (uid 100) inside container.

### Docker Compose Sandbox

For a complete local sandbox environment containing both a test SSH target server and the `ssh-mcp` server configured to communicate, see the [examples/ssh-mcp](../../examples/ssh-mcp) directory.


## Flags

- `-transport <stdio|streamable-http|sse>` — protocol to use (default: `stdio`)
- `-addr <host:port>` — listen address for HTTP transports (`streamable-http`, `sse`). Default `:8080`
- `-log-file <path>` — write structured debug logs (slog TextHandler) to the given file in append mode.
- `-disable-sudo` — do not register the `ssh_sudo_exec` tool. Useful when deploying to untrusted contexts to reduce the capability surface.
- `-disable-specialized-tools` — register only core SSH tools: session management, command execution, and file transfer (`upload_file`/`download_file` plus status, wait, and cancel tools).
- `-command-timeout <duration>` — default command timeout (default: `10s`).

### Password sources

A single provider supplies passwords for both SSH login (`ssh_open`) and sudo (`ssh_sudo_exec`). Exactly one of the following flags may be set:

| Flag | Per-machine? | Source | Re-read? |
|------|-------------|--------|----------|
| `-password-env <VAR>` | No | Value of an environment variable | Every call |
| `-password-file <path>` | No | Contents of a file (trailing newline stripped) | Every call |
| `-password-config <path>` | Yes | `machine = password` config file (see format below) | Every call |
| `-password-cmd <cmd>` | Yes | stdout of a command invoked with the machine name as its first argument | Cached per machine |

The per-call `sudo_password` field in `ssh_sudo_exec` always takes precedence over the server-level source. `ssh_open_cfg` does not consult the provider — use it when you want full explicit control over connection parameters.

#### Config file format

```
# ~/.ssh/passwords
web-01 = hunter2
db-01  = s3cr3t
```

Lines starting with `#` and blank lines are ignored. The file is re-read on every connection or sudo call, so edits take effect immediately.

#### Examples

```bash
# Global password from a file (e.g. Docker secret or tmpfs)
./ssh-mcp -password-file /run/secrets/ssh_pass

# Global password from an env var
SSH_PASS=hunter2 ./ssh-mcp -password-env SSH_PASS

# Per-machine config file
./ssh-mcp -password-config ~/.ssh/passwords

# Per-machine credential helper — machine name is passed as the first argument
./ssh-mcp -password-cmd "pass show"
# → runs: pass show web-01

# For tools that require the machine name to be part of the path (e.g. 1Password),
# use a wrapper script so the shell can interpolate it:
# #!/bin/sh
# op read "op://vault/ssh-passwords/$1/password"
./ssh-mcp -password-cmd "/path/to/get-password.sh"
```

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

With per-machine passwords from a config file:

```json
{
  "mcpServers": {
    "ssh": {
      "command": "/path/to/ssh-mcp",
      "args": ["-password-config", "/home/user/.ssh/passwords", "-log-file", "/tmp/ssh-mcp.log"]
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

## File transfer tools

| Tool | Description |
|------|-------------|
| `upload_file` | Start an asynchronous local-to-remote SFTP upload and return `upload_id` |
| `upload_status` | Check upload progress |
| `upload_wait` | Wait for an upload to complete |
| `upload_cancel` | Cancel an upload |
| `download_file` | Start an asynchronous remote-to-local SFTP download and return `download_id` |
| `download_status` | Check download progress |
| `download_wait` | Wait for a download to complete |
| `download_cancel` | Cancel a download |

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
