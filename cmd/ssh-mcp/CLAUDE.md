# cmd/ssh-mcp

Repo-wide rules are in the root `CLAUDE.md`.

## Build

```bash
go build ./cmd/ssh-mcp
# Run with default stdio transport (for Claude Code / Claude Desktop):
./ssh-mcp
# Or HTTP transport with debug logging:
./ssh-mcp -transport streamable-http -addr :8080 -log-file /tmp/ssh-mcp.log
```

## Blob uploads

`upload_file` takes either `local_path` or a `blob` id. Keep the two apart:

- **A blob id is not a path.** It never reaches `session.PoolOptions.LocalFS`, because there is no
  file of this host's involved — the confinement `LocalFS` provides is about reading local files.
  `handleUpload` therefore skips `Resolve` when a `Source` is set; that is deliberate, not an
  oversight.
- **A tool takes an id, never a URL.** The id names an object in the store this process was
  configured with, so there is no destination for a caller to choose. A URL argument would be a
  request forgery unless validated against the configured origin, and it would also stop working
  when the URL expires mid-transfer — which asynchronous uploads make likely.
- **The pool owns the reader.** `UploadRequest.Source` is closed by the pool however the request
  ends, including when the session is gone before a job starts. A caller that closes it too gets a
  double close; a path that returns without closing leaks it for the life of the process.
