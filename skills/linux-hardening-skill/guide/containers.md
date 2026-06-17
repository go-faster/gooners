# Container and Docker Hardening — Deep Dive

Reference for the [`linux-hardening`](../SKILL.md) skill. See [`../snippets/docker-daemon.json`](../snippets/docker-daemon.json) and [`../snippets/docker-run-hardened.sh`](../snippets/docker-run-hardened.sh).

## Container-optimized operating systems

Container-optimized OS images reduce attack surface compared with general-purpose Linux:

| Feature | Implementation | Security value |
|---|---|---|
| Immutable root filesystem | Read-only root; integrity checked via `dm-verity` | Reduces persistence and system tampering |
| Minimal userland | No shell, compilers, package managers, sometimes no SSH | Reduces post-exploitation tooling |
| Atomic updates | Whole-image updates with rollback, signed by TUF | Reduces dependency drift and partial-update failures |
| Mandatory access control | SELinux/AppArmor and kernel hardening (IMA, KPTI) by default | Limits container-escape impact |

Examples: AWS Bottlerocket, Google Container-Optimized OS, Flatcar Container Linux, RHEL CoreOS.

## Docker daemon hardening

In classic Docker, `dockerd` runs with root privileges. The socket `/var/run/docker.sock` is root-equivalent — mounting it read-only does not meaningfully limit this.

Key daemon practices (see `snippets/docker-daemon.json`):

- Do not expose unauthenticated TCP API (`tcp://0.0.0.0:2375`). If remote access is unavoidable, use mTLS on port 2376 with `--tlsverify`.
- `icc: false` disables direct inter-container routing on the default bridge network.
- `live-restore: true` allows containers to continue running during daemon restarts.
- `userns-remap: default` maps container root to a high unprivileged UID (see user namespaces below).
- Do not add ordinary users to the `docker` group — they gain root-equivalent host access.

File permission baselines per CIS Docker Benchmark:

| Path | Owner | Mode |
|---|---|---|
| `/var/run/docker.sock` | `root:docker` | `660` |
| `/etc/docker/daemon.json` | `root:root` | `644` |
| Docker server certificate key | `root:root` | `400` |

Audit commands:

```bash
auditctl -w /usr/bin/dockerd -k docker_audit
auditctl -w /var/run/docker.sock -k docker_audit
```

## User namespaces and rootless containers

Without user namespace remapping, UID 0 inside a container maps to UID 0 on the host — a container escape immediately yields root.

`userns-remap: default` in `daemon.json` maps container root to a high unprivileged UID from `/etc/subuid`/`/etc/subgid` (e.g., UID 231072+65535). A container escape then hits an unprivileged host UID.

Rootless Docker or Podman goes further: the daemon itself runs without root privileges. Rootless Podman integrates with `systemd` natively. Tradeoffs: restricted port binding below 1024; requires `slirp4netns` for networking.

## Linux capabilities

Drop all capabilities and add back only what is required:

```bash
docker run \
  --cap-drop ALL \
  --cap-add NET_BIND_SERVICE \
  --security-opt no-new-privileges:true \
  --read-only \
  --pids-limit 256 \
  --memory 256m \
  --cpus 1 \
  example/app:1.0.0
```

Avoid `--privileged` — it disables all isolation and grants all capabilities plus `/dev` access.

Capability risk reference:

| Capability | Typical reason | Risk if misused |
|---|---|---|
| `CAP_NET_BIND_SERVICE` | Bind ports below 1024 | Usually acceptable if truly needed |
| `CAP_NET_RAW` | Ping, raw sockets, packet tools | Packet crafting, sniffing, ARP spoofing |
| `CAP_NET_ADMIN` | Routing, iptables, interface changes | Network stack manipulation and MitM paths |
| `CAP_SYS_ADMIN` | Mounts, namespaces, many admin ops | Near root-equivalent; avoid unless critical |
| `CAP_DAC_OVERRIDE` | Bypass file permission checks | Unauthorized read/write of any path |

Always pair capability reduction with `--security-opt no-new-privileges:true` (`allowPrivilegeEscalation: false` in Kubernetes). This kernel bit prevents setuid binaries from escalating inside the container.

## Seccomp, AppArmor, and SELinux

These work best as independent layers:

- **Seccomp** filters syscalls; Docker's default profile blocks 40+ dangerous syscalls. Kubernetes `RuntimeDefault` applies it cluster-wide via `--seccomp-default` on kubelet (1.22+).
- **AppArmor** restricts filesystem paths and capabilities; `docker-default` profile blocks reads of `/etc/shadow`, `/proc/*/maps`, etc. even for root-inside-container.
- **SELinux** uses labels and type enforcement; container processes get `container_t`, host files get `etc_t`/`bin_t`, etc. Cross-type access denied at the kernel level regardless of Unix permissions. Standard on RHEL/Fedora/AlmaLinux.

## Resource limits

Resource limits are DoS containment, not just capacity planning:

- Memory limits trigger OOM Kill (exit 137) when exceeded, protecting other workloads on the node.
- CPU limits throttle rather than kill; sustained CPU exhaustion without limits can destabilize an entire Kubernetes node.
- PID limits (`--pids-limit`) and file-descriptor limits (`--ulimit nofile`) prevent fork bombs and descriptor exhaustion.
