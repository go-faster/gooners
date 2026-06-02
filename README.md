# gooners

MCP (Model Context Protocol) servers, AI tools, and agent skills built on go-faster technologies.

## Tools

| Tool | Description |
|------|-------------|
| [ssh-mcp](cmd/ssh-mcp) | MCP server exposing SSH, SFTP, and other operations as agent tools (Docker support included) |

## Skills

Agent skills (slash commands) that can be installed into AI coding assistants.

| Skill | Description |
|-------|-------------|
| [jx](skills/jx/SKILL.md) | Correct, high-performance JSON encoding/decoding with `github.com/go-faster/jx` |

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
