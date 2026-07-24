# gitlab-mcp

An MCP server for GitLab issues, merge requests, releases, and repository browsing.

It talks to the GitLab API directly rather than shelling out to the `glab` CLI. That is what lets every
tool take a `project` argument: `glab`'s own MCP server registers ~194 commands as tools and derives the
project from the git remote of its working directory, because it hides the `--repo` flag from the tool
schema (`cmdutils.AddGlobalRepoOverride` calls `MarkHidden("repo")`). This server needs no checkout, no
`glab` binary, and exposes 20 tools instead.

## Build

```bash
go build ./cmd/gitlab-mcp
# stdio transport (for Claude Code / Claude Desktop):
./gitlab-mcp
# HTTP transport with debug logging:
./gitlab-mcp -transport streamable-http -addr :8083 -log-file /tmp/gitlab-mcp.log
```

## Configuration

| Flag | Environment | Description |
|------|-------------|-------------|
| `-gitlab-url` | `GITLAB_URL` | Instance URL. Falls back to the `glab` config's host, then `https://gitlab.com`. |
| `-gitlab-token` | `GITLAB_TOKEN` | API token. Falls back to the token `glab auth login` stored for the host. |
| `-project` | `GITLAB_PROJECT` | Default project for tool calls that omit one. Optional. |
| `-glab-config-dir` | | Where to read `glab`'s `config.yml`. Defaults to `glab`'s own location (`$GLAB_CONFIG_DIR`, else `$XDG_CONFIG_HOME/glab-cli`, else `~/.config/glab-cli`). |
| `-no-glab-config` | | Do not read credentials from the `glab` config. |
| `-assets-dir` | | Directory the release asset tools may read and write. They are disabled when unset. |
| `-auth` | | Credential source: `server` (default), `client`, or `client-optional`. See [Authentication](#authentication). |
| `-auth-insecure` | | Allow caller-supplied tokens over plaintext HTTP. |

If you already run `glab auth login`, no configuration is needed:

```bash
./gitlab-mcp                                  # uses glab's host and token
./gitlab-mcp -project mygroup/myproject       # ...and defaults every tool to one project
```

Without a token, only public projects are readable, and the server logs a warning at startup.

## Authentication

`-auth` decides whose GitLab credential a session runs as.

| Mode | Credential | Use |
|------|-----------|-----|
| `server` (default) | `-gitlab-token`, or the `glab` config | One operator, one token. On stdio this is the only sensible mode. |
| `client` | The caller's, per session; sessions without one are refused | A shared HTTP server where each user brings their own token. |
| `client-optional` | The caller's, falling back to the server's | Migrating to `client` without breaking existing clients. |

Callers send their token on either header, `PRIVATE-TOKEN` taking precedence:

```
PRIVATE-TOKEN: glpat-xxxxxxxxxxxx
Authorization: Bearer glpat-xxxxxxxxxxxx
```

The credential is read once per MCP session, not per tool call, and clients presenting the same token
share one GitLab client. Under `-auth=client` the server holds no credential of its own: the `glab`
config is read only for the instance URL, and an explicit `-gitlab-token` is ignored with a warning.

Two guardrails refuse to start rather than fail quietly:

- **`client`/`client-optional` require an HTTP transport.** stdio has no headers, so a caller could never
  present a token and every session would silently read as anonymous.
- **They require TLS**, or an explicit `-auth-insecure`. Passthrough puts a credential on the wire.

The **instance URL is always the operator's**, in every mode. A server that let a caller supply both host
and token would be a shared server; one that let a caller supply the host while the server supplies the
token would be a credential-exfiltration endpoint. `effect.AllowHostOf` pins egress to the configured
instance and nothing in a request can widen it.

Note what `client` mode is not: authentication of the caller *to this server*. GitLab validates the token
on the first API call, so a session opened with a garbage token is accepted here and fails there. If you
need the server itself to reject unknown callers, put it behind something that does that.

Because the credential is bound at session creation, the **`Mcp-Session-Id` is as sensitive as the token
itself**: requests on an existing session run as whoever opened it, and a `PRIVATE-TOKEN` header sent
later in that session is ignored rather than honoured. Isolation between users is therefore only as good
as the secrecy of the session ID, which is another reason `-auth-insecure` is a deliberate opt-out.

## Tools

Every tool takes `project` — a path like `group/project`, a nested path like `group/sub/project`, or a
numeric ID. It is optional only when `-project` was given. `repo_search` finds the path when you do not
know it. Issues and merge requests are addressed by their project-scoped number (`#123`, `!123`).

| Tool | Description |
|------|-------------|
| `issue_list` | Lists issues, filtered by state, labels, assignee, author, milestone, or search |
| `issue_view` | Reads one issue, optionally with its comment thread |
| `issue_create` | Creates an issue |
| `issue_update` | Updates title, description, labels, assignees, milestone, or open/closed state |
| `issue_note` | Posts a comment on an issue |
| `mr_list` | Lists merge requests, filtered by state, labels, author, assignee, reviewer, branch, milestone, or search |
| `mr_view` | Reads one merge request: description, merge status, head pipeline, optionally approvals and comments |
| `mr_diff` | Per-file diffs, narrowable by `paths` |
| `mr_create` | Opens a merge request from a source branch |
| `mr_update` | Updates title, description, labels, assignees, reviewers, target branch, milestone, or state |
| `mr_note` | Posts a comment on a merge request |
| `release_list` | Lists releases |
| `release_view` | Reads one release by tag, or the latest; includes notes and assets |
| `release_create` | Creates a release from a tag |
| `release_asset_upload` | Uploads a local file and attaches it to a release |
| `release_asset_download` | Downloads a release asset to a local file |
| `repo_view` | Project metadata, optionally with the README |
| `repo_search` | Finds projects by name or path |
| `repo_tree` | Lists files and directories at a path and ref |
| `repo_file` | Reads a file at a ref |

There is deliberately no `mr_merge`, no `mr_approve`, and nothing that deletes.

### Labels on update

`labels` **replaces** the whole set; `add_labels` and `remove_labels` edit it. Sending `labels` when you
meant to add one drops every other label.

## Resources

| URI | Description |
|-----|-------------|
| `gitlab:///{project}` | The `-project` default's metadata. The one entry point listed without arguments. |
| `gitlab:///{project}/-/issues/{iid}` | One issue, as `issue_view` returns it |
| `gitlab:///{project}/-/merge_requests/{iid}` | One merge request, as `mr_view` returns it |
| `gitlab:///{project}/-/releases/{tag}` | One release, as `release_view` returns it |
| `gitlab:///{project}/-/blob/{ref}/{path}` | A file's contents, served as text or as a blob |

The URIs mirror GitLab's own web URLs, `/-/` included: a project path has an unbounded number of group
segments, so something has to mark where it ends. The `ref` in a blob URI cannot contain a slash, so
address a branch like `feat/x` by commit SHA. The tools have no such limit.

## Response size

A single API response can dwarf everything else in an agent's context, so the tools cap what they return
and say when they did: descriptions and READMEs at 20k runes, `repo_file` at 200k, `mr_diff` at 100k of
total diff text. `mr_diff` keeps the **file list** complete even when it drops diff text — an agent that
sees a name can ask for that file with `repo_file`; one that never saw it cannot. Lists default to 20
results per page and accept `page`/`per_page`.

## Security

Per the effect-provider invariant in `CLAUDE.md`:

- Every request goes through an `effect.NewHTTPClient` whose egress allowlist is exactly the configured
  instance. A release asset link can point at any host — GitLab allows external links — so
  `release_asset_download` is the one place a tool follows a URL chosen by project content. The allowlist
  is what stops that from turning this server into a fetch-anything proxy.
- The release asset tools reach host files only through `effect.Root(-assets-dir)`. With no `-assets-dir`
  they get an `effect.Deny` provider and can touch nothing. No handler checks paths itself.
- Reading `glab`'s config uses `os.ReadFile` because it is an operator-controlled startup path that no
  agent can influence.
- Under `-auth=server` on an HTTP transport, the server is a shared-credential proxy: whoever reaches
  `-addr` acts as the token it started with. That is the default because it is what stdio needs; it is
  `-auth=client` that makes an exposed server safe to share.
- The per-token client cache is bounded at 128 entries and dropped wholesale past it, because under
  `-auth=client` the set of tokens is caller-supplied and so is attacker-driven input.
