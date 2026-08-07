# internal/tools/gitlab

Guidance for the gitlab-mcp tools. The binary is `cmd/gitlab-mcp`; repo-wide rules are in the root
`CLAUDE.md`.

## Tool design

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
- `release_asset_download` writes to `path` when given and to `Config.Blob` when not. A host path is
  useless to an agent that does not share this filesystem, so neither destination is the default and
  a missing store is an error naming `-blob-addr`. Its MIME type is guessed from the asset name, not
  taken from the response: the type decides how a client renders bytes that project content supplied.
- Test fixtures for issues **must include `"id"`**: `gl.Issue.UnmarshalJSON` calls
  `reflect.TypeOf(raw["id"]).Kind()` unguarded and panics without it.
