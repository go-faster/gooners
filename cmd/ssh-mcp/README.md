# ssh-mcp

An MCP server that exposes SSH and SFTP operations as tools for AI agents. Designed for developer and admin workflows — run it on your local machine and let an AI assistant manage remote hosts through named sessions.

## Tools

**Session management**
- `ssh_open` — open a session using ambient credentials (`~/.ssh` keys, SSH agent)
- `ssh_open_cfg` — open a session with explicit parameters (key path, password, port, known_hosts)
- `ssh_close` — close a session
- `ssh_list` — list open sessions

**Execution**
- `ssh_exec` — run a command on an open session
- `ssh_sudo_exec` — run a command with `sudo -n` (requires passwordless sudo)
- `ssh_once_exec` — open a session, run one command, close it

**Filesystem (via SFTP/SSH)**
- `ls`, `cat`, `grep`, `find`, `stat` — read-only inspection
- `write_file` — create or overwrite a remote file
- `upload_file` — upload a local file to a remote path (restricted to the upload root; see Security)

**systemd**
- `systemctl_status`, `systemctl_list_units` — read-only
- `systemctl_start`, `systemctl_stop`, `systemctl_restart`, `systemctl_reload` — mutating (uses `sudo -n`)
- `journald_tail` — query journal entries

**Processes**
- `proc_list` — `ps aux` with optional user and grep filter
- `proc_info` — status, cmdline, exe, and cwd for a PID (reads `/proc/<pid>/…`)
- `proc_lsof` — open files for a PID; uses `lsof` if available, falls back to `/proc/<pid>/fd`
- `proc_kill` — send a signal to a PID via `sudo -n kill` (default: TERM; whitelist: TERM KILL HUP INT QUIT USR1 USR2 STOP CONT ABRT)

**System info**
- `sys_os_info` — hostname, `uname -a`, and `/etc/os-release` in one call
- `sys_uptime` — uptime and load averages
- `sys_mem` — memory and swap usage (`free -h`)
- `sys_net_addrs` — network interface addresses and link status (`ip addr show`); optional `iface` filter

**Disks & mounts**
- `disk_lsblk` — block devices with size, type, filesystem, mount point, label, UUID
- `disk_mounts` — current mounts from `/proc/mounts`; optional `fstype` filter
- `disk_df` — disk space usage (`df -h`); optional `path` to narrow to one filesystem

## Security notes

- **Known hosts**: host key verification is enforced against `~/.ssh/known_hosts` by default. Populate it before connecting or pass an explicit `known_hosts` path via `ssh_open_cfg`. Passing `"insecure"` disables verification entirely — avoid this.
- **Upload root**: `upload_file` only reads files within the directory where the server process was started. Paths outside that directory are rejected.
- **Ambient credentials**: `ssh_open` and `ssh_once_exec` use your SSH agent and `~/.ssh` key files automatically — the same keys your shell would use.
- **Execution**: `ssh_exec` and `ssh_sudo_exec` run arbitrary shell commands with the privileges of the SSH user. Only connect to hosts you trust.

## Build

```bash
go build ./cmd/ssh-mcp
```

## Claude Code setup

Add the server to your Claude Code MCP configuration. The server communicates over stdio.

**`~/.claude.json`** (global, all projects):
```json
{
  "mcpServers": {
    "ssh": {
      "command": "/path/to/ssh-mcp",
      "args": []
    }
  }
}
```

Or using `claude mcp add` (run from a directory you want as the upload root):

```bash
claude mcp add ssh /path/to/ssh-mcp
```

The server's **working directory at launch** becomes the upload root for `upload_file`. If you want to upload files from a project directory, start Claude Code from that directory or configure the `cwd` in your MCP config:

```json
{
  "mcpServers": {
    "ssh": {
      "command": "/path/to/ssh-mcp",
      "args": [],
      "cwd": "/home/you/projects/myproject"
    }
  }
}
```

## Known hosts setup

If `~/.ssh/known_hosts` does not already contain your target hosts, connect to them once via plain SSH first:

```bash
ssh user@host exit
```

This populates `known_hosts` and the MCP server will accept subsequent connections to those hosts.

Alternatively, use `ssh_open_cfg` with `known_hosts` pointing to a file you manage manually, or pre-populate it with `ssh-keyscan`:

```bash
ssh-keyscan host >> ~/.ssh/known_hosts
```
