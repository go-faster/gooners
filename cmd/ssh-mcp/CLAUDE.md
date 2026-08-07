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

## Blob transfers

`upload_file` takes either `local_path` or a `blob` id; `download_file` takes either `local_path` or
`to_blob`. Keep the two routes apart:

- **A blob transfer names no path.** It never reaches `session.PoolOptions.LocalFS`, because there
  is no file of this host's involved — the confinement `LocalFS` provides is over this host's
  filesystem. `handleUpload` and `handleDownload` therefore skip `Resolve` when `Source`/`Sink` is
  set; that is deliberate, not an oversight, and
  `TestPoolBlobTransferBypassesLocalFS` pins it. Gating it on `LocalFS` would make a server with no
  host file access unable to exchange files at all, which is the deployment the route exists for.
- **A tool takes an id, never a URL.** The id names an object in the store this process was
  configured with, so there is no destination for a caller to choose. A URL argument would be a
  request forgery unless validated against the configured origin, and it would also stop working
  when the URL expires mid-transfer — which asynchronous uploads make likely.
- **The pool owns the reader.** `UploadRequest.Source` is closed by the pool however the request
  ends, including when the session is gone before a job starts. A caller that closes it too gets a
  double close; a path that returns without closing leaks it for the life of the process.
- **A `to_blob` download reports its result on `download_status`/`download_wait`, not on
  `download_file`.** The transfer has not happened when the tool returns, so there is nothing to
  report yet; `TransferJob.Result` is filled in by the transfer goroutine under the job's lock.
