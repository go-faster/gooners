# gooners

MCP (Model Context Protocol) servers, AI tools, and agent skills built on go-faster technologies.

## Tools

| Tool | Description |
|------|-------------|
| [ssh-mcp](cmd/ssh-mcp) | MCP server exposing SSH, SFTP, and other operations as agent tools (Docker support included) |
| [grafana-dashboard-mcp](cmd/grafana-dashboard-mcp) | MCP server for building, verifying, and exporting Grafana dashboards using the Grafana Foundation SDK (Docker support included) |
| [opencode-handoff-mcp](cmd/opencode-handoff-mcp) | MCP server for delegating coding tasks to opencode agents and monitoring their sessions |
| [mcpgateway](cmd/mcpgateway) | MCP proxy that aggregates tools from multiple upstream MCP servers (stdio, streamable-http, sse) with per-upstream prefixes, allow/deny globs, description trim and output redaction |

## Prompts

The MCP servers expose predefined prompts that can be invoked by AI agents:

| Server | Prompt | Description |
|--------|--------|-------------|
| `ssh-mcp` | `troubleshoot-ssh` | Provides instructions on how to use SSH tools to debug a remote machine |
| `grafana-dashboard-mcp` | `design-dashboard` | Guides the agent through metric discovery, RED/USE dashboard design, query verification, and export |

## Skills

Agent skills (slash commands) that can be installed into AI coding assistants.

| Skill | Description |
|-------|-------------|
| [alert-writing](skills/alerts-writing/SKILL.md) | Create, review, rewrite, or tune monitoring alerts, alert rules, SLO/burn-rate alerts, routing, and runbooks |
| [jx](skills/jx/SKILL.md) | Correct, high-performance JSON encoding/decoding with `github.com/go-faster/jx` |
| [golang-refactor-tools](skills/golang-refactor-tools/SKILL.md) | Mechanical Go rewrites with `gofmt -r`, `gopatch`, `rsc.io/rf`, and `eg` |
| [linux-hardening-skill](skills/linux-hardening-skill/SKILL.md) | Harden Linux servers, Docker/containerd, and Kubernetes workloads on public IPs — host OS, SSH, network, container runtime, supply chain, and K8s security |

## 🚀 Installing skills

**Install with [skills.sh](https://skills.sh/) CLI** (works with any [Agent Skills](https://agentskills.io)-compatible tool):

```bash
npx skills add https://github.com/go-faster/gooners --all
# or a single skill:
npx skills add https://github.com/go-faster/gooners --skill jx
```

<!-- prettier-ignore-start -->

<details>
<summary>Claude Code</summary>

```bash
/plugin marketplace add go-faster/gooners
```

Or clone and install manually:

```bash
git clone https://github.com/go-faster/gooners.git ~/.claude/skills/gooners
```

</details>

<details>
<summary>OpenCode</summary>

```bash
git clone https://github.com/go-faster/gooners.git ~/.agents/skills/gooners
```

OpenCode auto-discovers skills from `.agents/skills/`, `.opencode/skills/`, and `.claude/skills/`.

</details>

<details>
<summary>Cursor</summary>

```bash
git clone https://github.com/go-faster/gooners.git ~/.cursor/skills/gooners
```

Cursor auto-discovers skills from `.agents/skills/` and `.cursor/skills/`.

</details>

<details>
<summary>Codex (OpenAI)</summary>

```bash
git clone https://github.com/go-faster/gooners.git ~/.agents/skills/gooners
```

Codex auto-discovers skills from `~/.agents/skills/` and `.agents/skills/`.

</details>

<!-- prettier-ignore-end -->
