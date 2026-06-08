# opencode-handoff-mcp

`opencode-handoff-mcp` is an MCP server for delegating coding tasks to agents running in an opencode HTTP server.

## Prerequisites

Start opencode separately before running this MCP server:

```bash
opencode serve
```

The full `handoff_run` and `handoff_fire` create-session flow requires opencode v2 `POST /api/session` support. If the connected opencode server does not expose that route, those tools return an actionable `404` error.

## Build

```bash
go build ./cmd/opencode-handoff-mcp
```

## Run

```bash
./opencode-handoff-mcp
```

HTTP transport:

```bash
./opencode-handoff-mcp -transport streamable-http -addr :8082
```

## Configuration

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `-opencode-url` | `OPENCODE_URL` | `http://localhost:4096` | opencode server base URL |
| `-opencode-username` | `OPENCODE_USERNAME` | `opencode` | Basic auth username |
| `-opencode-password` | `OPENCODE_PASSWORD` | empty | Basic auth password |
| `-default-directory` | `OPENCODE_DIRECTORY` | empty | Default `x-opencode-directory` header |
| `-request-timeout` | | `30s` | Per-request opencode HTTP timeout |
| `-wait-timeout` | | `10m` | Default `handoff_run` / `handoff_wait` timeout |
| `-transport` | | `stdio` | `stdio`, `streamable-http`, or `sse` |
| `-addr` | | `:8080` | HTTP transport listen address |
| `-log-file` | | empty | Write logs to a file instead of stdout |
| `-log-format` | | `text` | `text` or `json` |
| `-log-level` | | `info` | `debug`, `info`, `warn`, or `error` |

## Tools

| Tool | Description |
|------|-------------|
| `handoff_health` | Check opencode HTTP connectivity |
| `handoff_agents` | List agents for a directory/workspace |
| `handoff_models` | List providers and models |
| `handoff_sessions` | List sessions |
| `handoff_run` | Create a session, submit a prompt, wait, and return session output |
| `handoff_fire` | Create or reuse a session, submit a prompt, and return immediately |
| `handoff_check` | Inspect messages/context and pending unblock requests |
| `handoff_wait` | Wait for an existing session to finish |
| `handoff_permissions` | List pending permission requests |
| `handoff_permission_reply` | Reply to a permission request |
| `handoff_questions` | List pending questions |
| `handoff_question_reply` | Reply to or reject a question |

## Example

```json
{
  "prompt": "Run the tests and fix the failing package.",
  "agent": "build",
  "directory": "/home/me/project",
  "timeout_seconds": 600
}
```

Use `handoff_fire` for background delegation, then call `handoff_check` or `handoff_wait` with the returned `session_id`.

`handoff_run`, `handoff_check`, and `handoff_wait` return compact message and pending-request summaries by default. Pass `verbose=true` when raw opencode messages/context are needed for debugging.

## Security

This server delegates to opencode, which may edit files or run shell commands depending on opencode agent permissions. The MCP server does not auto-approve permissions. Use `handoff_permissions` and `handoff_permission_reply` to inspect and answer permission requests explicitly.
