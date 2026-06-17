---
name: linux-hardening
description: Use this skill when asked to harden, review, or generate a baseline for Linux servers, containers, Docker/containerd, or Kubernetes workloads exposed to the public internet. Covers host OS, SSH, network, container runtime, supply chain, and Kubernetes security.
---

# Linux & Container Hardening Skill

Use this skill when asked to harden, review, or generate a baseline for Linux servers, containers, Docker/containerd, or Kubernetes workloads exposed to the public internet. Favor layered controls: minimize exposure, require strong identity, restrict network paths, isolate containers, control the supply chain, and keep detection and recovery tested.

## Operating rules

1. Do not produce destructive commands without warning. Prefer audit/read-only commands first.
2. Do not recommend disabling SSH passwords until the user has verified key login in a separate active session.
3. Do not blindly apply sysctl/network settings to routers, VPN hubs, Kubernetes nodes, asymmetric-routing hosts, or cloud control-plane machines.
4. Treat MFA, VPN, SPA, Fail2Ban, and cloud security groups as layers, not replacements for SSH keys, least privilege, and host firewall.
5. Separate human accounts from automation accounts before requiring PAM-based MFA.

## Audit script

The skill ships `hardening-audit.sh` — a read-only inventory helper that collects SSH settings, listening ports, firewall state, enabled services, sysctl values, sudo config, automatic-update status, MAC enforcement, and audit-rule status. Run it first:

```bash
bash hardening-audit.sh 2>&1 | tee audit-$(hostname)-$(date +%Y%m%d).txt
```

## Host hardening

### SSH

Require named users, SSH keys, no root login, and no password login.

```conf
# /etc/ssh/sshd_config.d/10-hardening.conf
PermitRootLogin no
PubkeyAuthentication yes
PasswordAuthentication no
PermitEmptyPasswords no
AllowUsers admin deploy   # REPLACE with actual account names; omitting allows all users
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

**Only add if PAM MFA is already configured and tested:**

```conf
KbdInteractiveAuthentication yes
UsePAM yes
AuthenticationMethods publickey,keyboard-interactive
```

Snippets: [`snippets/ssh-bootstrap.sh`](snippets/ssh-bootstrap.sh) · [`snippets/sshd_config`](snippets/sshd_config) · [`snippets/sudoers-admins`](snippets/sudoers-admins)

For MFA options, bastion/ProxyJump, and Fail2Ban configuration: [`guide/ssh.md`](guide/ssh.md)

### Firewall

Default deny inbound. Open only required public services.

```bash
ufw default deny incoming && ufw default allow outgoing
ufw allow OpenSSH && ufw allow 80/tcp && ufw allow 443/tcp
ufw status verbose
```

Always inventory live listeners first:

```bash
ss -plnt && ss -plnu
systemctl list-unit-files --type=service --state=enabled
```

With nftables: [`snippets/nftables-minimal.nft`](snippets/nftables-minimal.nft)
With iptables: [`snippets/iptables-minimal.sh`](snippets/iptables-minimal.sh)

### Attack surface

Remove or mask unneeded services: telnet/rsh/rexec, cups, avahi, rpcbind/NFS, web panels, Docker TCP API.

```bash
systemctl disable --now cups avahi-daemon rpcbind 2>/dev/null || true
systemctl mask cups.service 2>/dev/null || true
```

### Kernel and network sysctl

Do **not** apply blindly to routers, VPN hubs, Kubernetes nodes, or asymmetric-routing hosts.

```bash
cp sysctl-network.conf /etc/sysctl.d/60-network-hardening.conf
cp sysctl-kernel.conf /etc/sysctl.d/61-kernel-hardening.conf
sysctl -p /etc/sysctl.d/60-network-hardening.conf
sysctl -p /etc/sysctl.d/61-kernel-hardening.conf
```

Key caveats: `rp_filter=1` breaks asymmetric routing — use `=2` if drops occur on legitimate traffic. `kernel.kexec_load_disabled=1` is irreversible until reboot — skip on hosts that use kexec for live-patching.

Or use the consolidated file from the container skill: [`snippets/sysctl-hardening.conf`](snippets/sysctl-hardening.conf)

### MAC and privilege

Keep SELinux enforcing on RHEL/Fedora/Alma/Rocky. Keep AppArmor enforcing on Ubuntu/SUSE. Do not leave permissive/complain mode as a permanent workaround.

```sudoers
%admins ALL=(ALL:ALL) ALL
Defaults use_pty
Defaults logfile="/var/log/sudo.log"
```

### Detection and recovery

```bash
cp snippets/auditd-hardening.rules /etc/audit/rules.d/
auditctl -R /etc/audit/rules.d/auditd-hardening.rules
```

Minimum stack: journald, auditd, AIDE or Wazuh, remote log shipping, and tested encrypted off-site backups. Do not run auditd and osquery process-auditing mode simultaneously.

For observability details, alerting, incident response, and eBPF runtime detection: [`guide/observability.md`](guide/observability.md)

## Container and Docker checklist

- Do not expose Docker TCP API without mTLS. `/var/run/docker.sock` access is root-equivalent.
- Prefer rootless Docker/Podman or `userns-remap` where compatible with your storage driver.
- Use minimal base images (scratch, distroless, Alpine); multi-stage builds; `USER` in Dockerfile.
- Pin base image versions or digests; do not use `latest` in production.
- Run containers non-root, `no-new-privileges`, `readOnlyRootFilesystem: true`, drop `ALL` capabilities, set CPU/memory/PID limits.
- Avoid `--privileged`, host namespace mounts (`hostPID`, `hostIPC`, `hostNetwork`), and broad `hostPath`.
- Scan images and IaC in CI/CD; sign images and verify signatures before deployment.

Snippets: [`snippets/docker-daemon.json`](snippets/docker-daemon.json) · [`snippets/docker-run-hardened.sh`](snippets/docker-run-hardened.sh) · [`snippets/Dockerfile.go-distroless`](snippets/Dockerfile.go-distroless)

For Docker daemon deep-dive, user namespaces, capabilities, seccomp/AppArmor/SELinux: [`guide/containers.md`](guide/containers.md)

## Kubernetes checklist

- Use Pod Security Admission with `restricted` profile where possible.
- Require `runAsNonRoot`, `allowPrivilegeEscalation: false`, `seccompProfile: RuntimeDefault`, `readOnlyRootFilesystem: true`, `capabilities.drop: ["ALL"]`.
- Set CPU/memory requests and limits for all workloads.
- Disable service account token automounting unless needed (`automountServiceAccountToken: false`).
- Avoid `hostNetwork`, `hostPID`, `hostIPC`, privileged pods, and broad `hostPath` mounts.
- Use NetworkPolicies for namespace and workload segmentation; default-deny first.
- Keep RBAC narrow — no wildcard verbs/resources, no broad `cluster-admin`.
- Enable API server audit logging.
- Use admission policies (Kyverno, OPA Gatekeeper, `ValidatingAdmissionPolicy`) for image signatures and workload security.

Snippets: [`snippets/k8s-secure-pod.yaml`](snippets/k8s-secure-pod.yaml) · [`snippets/k8s-default-deny-networkpolicy.yaml`](snippets/k8s-default-deny-networkpolicy.yaml) · [`snippets/k8s-audit-policy.yaml`](snippets/k8s-audit-policy.yaml) · [`snippets/kyverno-require-signed-images.yaml`](snippets/kyverno-require-signed-images.yaml)

For RBAC review commands, NetworkPolicy patterns, sandbox runtimes, and CI/CD pipeline hardening: [`guide/kubernetes.md`](guide/kubernetes.md)

## Supply chain checklist

- Use multi-stage builds; copy only runtime artifacts into the final image.
- Pin base image digests; do not use `latest` in production.
- Generate SBOMs and scan for CVEs and secrets in CI before push.
- Sign images with Cosign/Sigstore; verify signatures via admission controller before deployment.

Snippet: [`snippets/supply-chain-ci.sh`](snippets/supply-chain-ci.sh) — Trivy scan + Syft SBOM + Cosign sign pipeline

For base image strategy, Hadolint rules, Trivy/Syft/Cosign details, and Sigstore signing flow: [`guide/supply-chain.md`](guide/supply-chain.md)

## Strong opinions

- Changing the SSH port is noise reduction, not hardening. Do it only after keys, root-login disablement, firewall, and monitoring are in place.
- Mounting `docker.sock` into a container is usually a design failure. If unavoidable, isolate heavily and document the root-equivalent risk explicitly.
- `--privileged` should be treated as a production exception requiring explicit approval.
- A public host without centralized logs and tested backup/restore is not production-ready.
- Container security without host hardening is incomplete; host hardening without supply-chain control is also incomplete.

## Review guidance for agents

When reviewing a hardening plan or setup:

1. Identify exposed entry points: public IPs, SSH, Docker socket, admin panels, Kubernetes API, cloud metadata endpoint.
2. Check identity boundaries: users, sudo, SSH keys, service accounts, RBAC, workload identity, secret access.
3. Check privilege boundaries: root, capabilities, privileged mode, host namespaces, host mounts, SELinux/AppArmor/seccomp, read-only filesystem.
4. Check supply chain: base image, digest pinning, SBOM, CVE scan, image signature, provenance.
5. Check detection and response: logs, auditd, runtime detection, alerts, backups, credential rotation, rebuild process.
6. Rank fixes by exploitability and blast-radius reduction. Do not bury SSH exposure, Docker socket exposure, privileged containers, or missing patching under low-impact recommendations.

## Agent checklist order

1. Assumptions: distro, role, public services, cloud/on-prem, container runtime, Kubernetes version, SSH access model.
2. Read-only inventory commands.
3. Critical fixes: SSH, firewall, exposed services, root/sudo, privileged containers.
4. Important hardening: MAC, sysctl, updates, logging, backups, container security context.
5. Supply chain: image scanning, SBOM, signing, admission verification.
6. Optional barriers: VPN before SSH, bastion/IAP/JIT, SPA/fwknop, sandbox runtimes.
7. Rollback notes and lockout warnings.
8. Verification commands.

## Verification commands

Host:

```bash
sshd -T | grep -E 'permitrootlogin|passwordauthentication|kbdinteractiveauthentication|authenticationmethods|allowusers|disableforwarding'
sshd -t
ss -lntup
systemctl list-unit-files --type=service --state=enabled
sudo nft list ruleset || sudo iptables -S
sysctl net.ipv4.tcp_syncookies kernel.randomize_va_space kernel.dmesg_restrict net.ipv6.conf.all.accept_redirects
auditctl -l
grep -rn 'NOPASSWD' /etc/sudoers /etc/sudoers.d/ 2>/dev/null
journalctl -u ssh -S today --no-pager
```

Docker:

```bash
docker info
docker inspect CONTAINER_ID | jq '.[0].HostConfig | {Privileged, CapAdd, CapDrop, ReadonlyRootfs, SecurityOpt, UsernsMode, PidsLimit}'
ls -l /var/run/docker.sock
```

Kubernetes:

```bash
kubectl get pods -A -o json | jq '.items[] | {ns:.metadata.namespace,name:.metadata.name,sc:.spec.securityContext,containers:[.spec.containers[].securityContext]}'
kubectl get networkpolicy -A
kubectl get clusterrolebinding,rolebinding -A -o wide
kubectl auth can-i --as system:serviceaccount:default:app-sa get secrets -n default
```

Supply chain:

```bash
trivy image registry.example.com/app:1.0.0
syft registry.example.com/app:1.0.0 -o table
cosign verify registry.example.com/app:1.0.0
```

For tool reference, attack pattern quick reference, and authoritative standards: [`guide/reference.md`](guide/reference.md)
