# Grafana Dashboard MCP - Docker Example

This example demonstrates how to set up and run the [grafana-dashboard-mcp](../../cmd/grafana-dashboard-mcp) server inside Docker alongside a Grafana instance. This is perfect for local testing, dashboard design sandboxing, and integration with AI coding assistants.

## Services Included

1. **Grafana (`grafana-sandbox`)**: A standard Grafana OSS container.
   - Accessible at [http://localhost:3000](http://localhost:3000)
   - Configured with anonymous admin access for frictionless local development.
   - Default credentials: `admin` / `admin`.
2. **Grafana MCP Server (`grafana-dashboard-mcp`)**: The MCP server running on the `streamable-http` transport.
   - Exposed at `http://localhost:8081/mcp`
   - Configured to talk to the local Grafana container using Basic Auth.
   - Persists dashboard sessions to a named Docker volume (`mcp-sessions`).

## How to Run

1. **Start the containers**:
   From this directory, run:
   ```bash
   docker compose up --build -d
   ```

2. **Verify it is running**:
   - Check if Grafana is accessible by opening [http://localhost:3000](http://localhost:3000) in your browser.
   - Check if the MCP server is responding on its HTTP endpoint:
     ```bash
     curl -i http://localhost:8081/mcp
     ```
     You should receive an HTTP response (usually a `405 Method Not Allowed` or similar since it expects JSON-RPC POST requests, but it proves the server is listening).

3. **Stop the containers**:
   ```bash
   docker compose down -v
   ```

---

## Client Integration Configurations

Below are setup configurations for popular MCP clients.

### 1. Claude Code

Add the server using the streamable-http transport to your `~/.claude.json` or project-specific `.claude.json`:

```json
{
  "mcpServers": {
    "grafana-dashboard-mcp": {
      "type": "streamable-http",
      "url": "http://localhost:8081/mcp"
    }
  }
}
```

### 2. Claude Desktop

Add the following to your Claude Desktop configuration file (typically `~/Library/Application Support/Claude/claude_desktop_config.json` on macOS or `%APPDATA%\Claude\claude_desktop_config.json` on Windows):

```json
{
  "mcpServers": {
    "grafana-dashboard-mcp": {
      "command": "docker",
      "args": [
        "run",
        "--rm",
        "-i",
        "-e", "GRAFANA_URL=http://host.docker.internal:3000",
        "-e", "GRAFANA_USER=admin",
        "-e", "GRAFANA_PASSWORD=admin",
        "gooners",
        "grafana-dashboard-mcp"
      ]
    }
  }
}
```
*(Note: Requires building the Docker image locally first using `docker build -t gooners ../../` from this directory).*

### 3. Cursor / VS Code / Other Clients

If your client supports HTTP-based MCP servers, add a new server of type **command/SSE/HTTP** pointing to:
- **URL**: `http://localhost:8081/mcp`

---

## Testing the Setup

Once connected, you can prompt your AI agent to test the connection. Here are some sample commands:

> "Create a new Grafana dashboard named 'System Metrics' and add a Time Series panel showing CPU usage."

The agent will interactively:
1. Initialize a dashboard session (`add_dashboard`)
2. Add a panel (`add_panel`)
3. Compile and export the finished dashboard directly to the local Grafana instance (`export_dashboard`)

Open [http://localhost:3000](http://localhost:3000) to view your freshly created dashboard!
