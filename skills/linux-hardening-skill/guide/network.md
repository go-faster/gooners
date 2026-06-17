# Network, Kernel, and Secrets — Deep Dive

Reference for the [`linux-hardening`](../SKILL.md) skill.

## Firewall — nftables vs iptables

Prefer `nftables` on modern distributions. The critical point is policy: allow loopback, allow established/related flows, drop invalid traffic, default-deny inbound, restrict SSH, open only required application ports.

See [`../snippets/nftables-minimal.nft`](../snippets/nftables-minimal.nft) for a complete deny-by-default ruleset with SSH restricted to admin CIDR ranges.

See [`../snippets/iptables-minimal.sh`](../snippets/iptables-minimal.sh) for the legacy iptables equivalent.

The cloud provider layer should mirror the same policy via security groups, NSG, or VPC firewall rules as an additional enforcement point independent of the host.

## Kernel sysctl — rationale and caveats

The skill ships `sysctl-network.conf` and `sysctl-kernel.conf` (split for clarity) and a consolidated `snippets/sysctl-hardening.conf`. Key settings:

| Setting | What it does | When to skip/change |
|---|---|---|
| `net.ipv4.conf.all.rp_filter=1` | Strict reverse-path filtering | Use `=2` (loose) for multi-WAN, asymmetric routing, some CNI plugins |
| `net.ipv4.tcp_syncookies=1` | SYN flood mitigation | Safe everywhere |
| `net.ipv4.conf.all.accept_redirects=0` | Reject ICMP redirects | Safe; breaks nothing in normal use |
| `net.ipv4.conf.all.send_redirects=0` | Do not send ICMP redirects | Skip on routers/gateways |
| `kernel.randomize_va_space=2` | Full ASLR | Safe everywhere |
| `kernel.dmesg_restrict=1` | Hide dmesg from unprivileged users | May break non-root tracing tools |
| `kernel.kexec_load_disabled=1` | Disable kexec | **Irreversible until reboot** — skip on live-patching hosts |
| `kernel.unprivileged_bpf_disabled=2` | Block unprivileged BPF | Permanent for boot session; breaks non-root `bpftrace`/`tcpdump` |

Do not completely disable ICMP — it breaks path MTU discovery and diagnostic flows more than it helps.

## Service lifecycle

Run services under dedicated unprivileged users. `systemd` sandboxing (`DynamicUser=`, `ProtectSystem=`, `PrivateTmp=`, `NoNewPrivileges=`) reduces blast radius but is not equivalent to proper process isolation.

Automatic security updates are appropriate for Internet-facing hosts. Kernel, container runtime, kubelet, and control-plane updates should follow a controlled process with rollback and node-drain plans.

## LUKS disk encryption

Protects against offline disk, snapshot, or decommissioning exposure. Does not protect a running system from live root compromise.

```bash
sudo cryptsetup luksFormat /dev/vdb
sudo cryptsetup open /dev/vdb secure_data
sudo mkfs.ext4 /dev/mapper/secure_data
```

## Secrets management

Secrets must not live in Git, permanent `.env` files, unprotected Ansible variables, or Kubernetes ConfigMaps without encryption.

Use centralized secret management (HashiCorp Vault, AWS Secrets Manager, Kubernetes Secrets with etcd encryption at rest), rotation, audit trails, and least-privilege access.

Backups must be versioned, encrypted, off-site, and restore-tested. A backup strategy that has never been restored is a belief, not a control.
