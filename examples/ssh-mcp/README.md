# SSH MCP - Docker Setup Example

This example demonstrates how to run the [ssh-mcp](../../cmd/ssh-mcp) server inside Docker using your local SSH configuration (`~/.ssh`), SSH agent keys, and local directories. 

Unlike mock setups, this enables the MCP container to act exactly like a locally running binary, allowing your AI assistant to securely manage your real remote hosts while keeping the MCP server process isolated in a Docker container.

## Mount Features

The Compose setup mounts the following from your host system:
1. **SSH Configurations (`~/.ssh`)**: Mounted read-only to `/home/mcp/.ssh:ro`. This ensures host configurations (`~/.ssh/config`) and host key verifications (`~/.ssh/known_hosts`) are fully respected.
2. **SSH Agent socket (`$SSH_AUTH_SOCK`)**: Mounted to `/ssh-agent` and passed via the `SSH_AUTH_SOCK` environment variable. This allows the container to reuse keys loaded in your host's ssh-agent for passwordless authentication without ever exposing private key files.
3. **Local Directory (`.`)**: Mounted to `/work` and set as the container's working directory. This acts as the secure upload/download root for files transferred via the `upload_file` and `download_file` tools.

## How to Run

1. **Verify your SSH Agent is running** and has your keys loaded:
   ```bash
   ssh-add -l
   ```
   *(If your agent isn't running or has no keys, load them using `ssh-add ~/.ssh/id_ed25519` or similar).*

2. **Start the MCP server container**:
   From this directory, run:
   ```bash
   docker compose up --build -d
   ```

3. **Verify the MCP server is responding**:
   ```bash
   curl -i http://localhost:8080/mcp
   ```

4. **Stop the container**:
   ```bash
   docker compose down
   ```

---

## Client Integration Configurations

Below are setup configurations for popular MCP clients.

### 1. Claude Code

Add the server using the streamable-http transport to your `~/.claude.json` or project-specific `.claude.json`:

```json
{
  "mcpServers": {
    "ssh-mcp": {
      "type": "streamable-http",
      "url": "http://localhost:8080/mcp"
    }
  }
}
```

### 2. Claude Desktop

Add the following to your Claude Desktop configuration file (typically `~/Library/Application Support/Claude/claude_desktop_config.json` on macOS or `%APPDATA%\Claude\claude_desktop_config.json` on Windows):

```json
{
  "mcpServers": {
    "ssh-mcp": {
      "command": "docker",
      "args": [
        "run",
        "--rm",
        "-i",
        "-v", "$HOME/.ssh:/home/mcp/.ssh:ro",
        "-v", "$SSH_AUTH_SOCK:/ssh-agent",
        "-e", "SSH_AUTH_SOCK=/ssh-agent",
        "-v", "/path/to/local/workspace:/work",
        "-w", "/work",
        "gooners",
        "ssh-mcp"
      ]
    }
  }
}
```
*(Note: Requires building the Docker image locally first using `docker build -t gooners ../../` from this directory).*

### 3. Cursor / VS Code / Other Clients

If your client supports HTTP-based MCP servers, add a new server of type **command/SSE/HTTP** pointing to:
- **URL**: `http://localhost:8080/mcp`

---

## Testing the Setup

Once connected, ask your AI agent to run tools using your real ssh config:

> "Connect to my server `my-web-host` (replace with an alias from your local `~/.ssh/config`) and tell me the output of `uptime`."

The agent will interactively:
1. Call `ssh_open` with `machine: "my-web-host"`.
2. Authenticate using your SSH agent.
3. Run `ssh_exec` with the command `uptime` and return the result.
4. If you upload a file using `upload_file`, it will be read from the local directory that you started the compose stack in (or `/path/to/local/workspace` in Claude Desktop).
