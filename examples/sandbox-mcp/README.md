# sandbox-mcp - Docker Compose Example

This example demonstrates how to run the [sandbox-mcp](../../cmd/sandbox-mcp) server inside Docker, so
every `sandbox_open` MCP call gets its own fresh, isolated container from the host's Docker daemon.

## Prerequisites

1. **A reachable Docker daemon.** This example mounts the host's Docker socket into the `sandbox-mcp`
   container - `sandbox-mcp` needs it to create/destroy sandboxes on the host, exactly like any other
   Docker-in-Docker tool. **Never** mount this socket into a *sandbox* container itself; only into
   `sandbox-mcp`.
2. **Per-architecture `sandbox-agent` binaries.** `sandbox-mcp` injects a static SSH+SFTP agent into every
   sandbox it creates, matched to that sandbox image's architecture. Build one per architecture your
   sandbox images use, laid out as `agent-bin/<arch>/sandbox-agent`:

   ```bash
   cd ../..
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o examples/sandbox-mcp/agent-bin/amd64/sandbox-agent ./cmd/sandbox-agent
   CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o examples/sandbox-mcp/agent-bin/arm64/sandbox-agent ./cmd/sandbox-agent
   ```

## How to Run

1. Build the agent binaries (above).
2. Start the MCP server:
   ```bash
   docker compose up --build -d
   ```
3. Verify it's responding:
   ```bash
   curl -i http://localhost:8083/mcp
   ```
4. Stop it:
   ```bash
   docker compose down
   ```

## Trying it out

Call `sandbox_open` to get a `session_id`, then use it with every other tool exactly like `ssh_open`'s
session IDs:

```jsonc
// sandbox_open -> {"session_id": "alpine-...-...", "image": "alpine:latest", "network": "none"}
// ssh_exec {"session_id": "...", "command": "echo hi"}
// sandbox_close {"session_id": "..."}
```

See the [sandbox-mcp README](../../cmd/sandbox-mcp/README.md) for the full flag/policy reference,
including how to widen the allowed image list or network tier.
