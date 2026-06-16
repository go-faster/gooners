---
name: linux-hardening
description: Use this skill when asked to harden, review, or generate a baseline for a Linux server exposed to the public internet. Favor layered controls.
---

# Linux Public-IP Hardening Skill

Use this skill when asked to harden, review, or generate a baseline for a Linux server exposed to the public internet. Favor layered controls: reduce exposed services, require strong identity, restrict network paths, log enough to investigate, and keep recovery tested.

## Operating rules

1. Do not produce destructive commands without warning. Prefer audit/read-only commands first.
2. Do not recommend disabling SSH passwords until the user has verified key login in a separate active session.
3. Do not blindly apply sysctl/network settings to routers, VPN hubs, Kubernetes nodes, asymmetric-routing hosts, or cloud control-plane machines.
4. Treat MFA, VPN, SPA, Fail2Ban, and cloud security groups as layers, not replacements for SSH keys, least privilege, and host firewall.
5. Separate human accounts from automation accounts before requiring PAM-based MFA.

## Audit script

The skill ships `hardening-audit.sh` — a read-only inventory helper that collects SSH settings, listening ports, firewall state, enabled services, sysctl values, sudo config, automatic-update status, MAC enforcement, and audit-rule status. It does not modify system state. Run it first to get a baseline before making changes:

```bash
bash hardening-audit.sh 2>&1 | tee audit-$(hostname)-$(date +%Y%m%d).txt
```

## Minimal hardening baseline

### SSH

Require named users, SSH keys, no root login, and no password login.

```conf
# /etc/ssh/sshd_config.d/10-hardening.conf
PermitRootLogin no
PubkeyAuthentication yes
PasswordAuthentication no
PermitEmptyPasswords no
AllowUsers admin deploy   # REPLACE with actual account names; omitting this directive allows all users
LoginGraceTime 30
MaxAuthTries 3
MaxStartups 10:30:60
ClientAliveInterval 300
ClientAliveCountMax 2
DisableForwarding yes
```

Validate before reload:

```bash
sshd -t && systemctl reload sshd
```

**Only add the following if PAM MFA (e.g. Google Authenticator, TOTP) is already configured. Setting `KbdInteractiveAuthentication yes` without a working PAM stack will break login.**

```conf
KbdInteractiveAuthentication yes
UsePAM yes
AuthenticationMethods publickey,keyboard-interactive
```

### Firewall

Default deny inbound. Open only required public services.

```bash
ufw default deny incoming
ufw default allow outgoing
ufw allow OpenSSH
ufw allow 80/tcp
ufw allow 443/tcp
ufw status verbose
```

Always inventory live listeners:

```bash
ss -plnt
ss -plnu
systemctl list-unit-files --type=service --state=enabled
```

### Attack surface

Remove or mask unneeded public services: telnet/rsh/rexec, cups, avahi, rpcbind/NFS, web panels, Docker TCP API, LXD remote API, libvirt TCP exposure. Avoid compilers/build tools on production unless needed.

```bash
systemctl disable --now cups avahi-daemon rpcbind 2>/dev/null || true
systemctl mask cups.service 2>/dev/null || true
```

### Kernel and network sysctl

Use conservative defaults for ordinary hosts only. Do **not** apply blindly to routers, VPN hubs, Kubernetes nodes, BGP speakers, or any host with asymmetric routing.

The skill ships two annotated drop-in files. Each key includes an inline comment explaining what it does and when it can break things. Copy to the target paths and apply:

```bash
# network: SYN cookies, ICMP redirects, source routing, martians, rp_filter, IPv6
cp sysctl-network.conf /etc/sysctl.d/60-network-hardening.conf
sysctl -p /etc/sysctl.d/60-network-hardening.conf

# kernel: ASLR, dmesg/kptr restrict, unprivileged BPF, kexec, fs protections
cp sysctl-kernel.conf /etc/sysctl.d/61-kernel-hardening.conf
sysctl -p /etc/sysctl.d/61-kernel-hardening.conf
```

Key impact notes:
- `rp_filter=1` breaks asymmetric routing, multi-WAN, and some CNI plugins — use `rp_filter=2` (loose) if drops occur on legitimate traffic.
- `kernel.kexec_load_disabled=1` is **irreversible until reboot** — skip on hosts that use kexec for live-update procedures.
- `kernel.unprivileged_bpf_disabled=2` is also permanent for the boot session; breaks non-root network tracing tools.
- Apply IPv6 keys even when v6 is "not in use" — the NIC may still have a v6 address reachable from outside.

### MAC and privilege

Keep SELinux enforcing on RHEL/Fedora/Alma/Rocky. Keep AppArmor enforcing on Ubuntu/SUSE. Do not leave permissive/complain mode as a permanent workaround.

Use sudo with named users. Avoid `NOPASSWD: ALL`; scope automation to exact commands.

```sudoers
%admins ALL=(ALL:ALL) ALL
Defaults use_pty
Defaults logfile="/var/log/sudo.log"
```

### Detection and recovery

Minimum stack: journald, auditd, AIDE or Wazuh, remote log shipping, and tested encrypted off-site backups. Do not run auditd and osquery process-auditing mode at the same time.

```bash
journalctl -u ssh -S today
systemctl is-active auditd
auditctl -l   # verify rules are actually loaded, not just the daemon running
```

Backups must be versioned, encrypted, off-site, and restore-tested. For databases, use application-consistent dumps or snapshots, not only file-level backup.

## Agent checklist

When assisting a user, produce outputs in this order:

1. Assumptions: distro, role, public services, cloud/on-prem, SSH access model.
2. Read-only inventory commands.
3. Critical fixes: SSH, firewall, exposed services, root/sudo.
4. Important hardening: MAC, sysctl, updates, logging, backups.
5. Optional barriers: VPN before SSH, bastion/IAP/JIT, SPA/fwknop.
6. Rollback notes and lockout warnings.
7. Verification commands.

## Verification commands

```bash
sshd -T | grep -E 'permitrootlogin|passwordauthentication|kbdinteractiveauthentication|authenticationmethods|allowusers|disableforwarding'
sshd -t
ss -plnt
ss -plnu
systemctl --type=service --state=running
sysctl net.ipv4.tcp_syncookies kernel.randomize_va_space kernel.dmesg_restrict net.ipv6.conf.all.accept_redirects
auditctl -l
grep -rn 'NOPASSWD' /etc/sudoers /etc/sudoers.d/ 2>/dev/null
journalctl -u ssh -S today --no-pager
```
