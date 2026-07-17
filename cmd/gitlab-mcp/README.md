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

If you already run `glab auth login`, no configuration is needed:

```bash
./gitlab-mcp                                  # uses glab's host and token
./gitlab-mcp -project mygroup/myproject       # ...and defaults every tool to one project
```

Without a token, only public projects are readable, and the server logs a warning at startup.

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
