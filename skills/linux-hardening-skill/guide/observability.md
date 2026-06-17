# Observability, Detection, and Incident Response — Deep Dive

Reference for the [`linux-hardening`](../SKILL.md) skill. See [`../snippets/auditd-hardening.rules`](../snippets/auditd-hardening.rules) and [`../snippets/incident-triage.sh`](../snippets/incident-triage.sh).

## Mandatory collection

Local logs alone are weak — an attacker with root can often tamper with them. Use local journald/syslog plus remote log forwarding to a central system.

Minimum host observability:

- journald/syslog for service events
- auditd for security-relevant syscalls and critical files (`snippets/auditd-hardening.rules` covers identity, SSH, sudo, privilege commands, Docker binaries/sockets)
- file integrity monitoring: AIDE or a HIDS/XDR agent (Wazuh/OSSEC)
- runtime detection for hosts and containers: Falco or Tetragon
- resource metrics with Prometheus Node Exporter and Alertmanager notifications

Do not run auditd and osquery process-auditing mode simultaneously.

Useful quick commands:

```bash
ss -tulpn
systemctl list-unit-files --type=service --state=enabled
journalctl -u ssh -u sshd -p warning --since "24 hours ago"
ausearch -k passwd_changes
aureport -au
```

## Container and cluster telemetry

Log to stdout/stderr and collect centrally. Configure Docker log rotation to prevent disk exhaustion:

```json
{
  "log-driver": "json-file",
  "log-opts": { "max-size": "10m", "max-file": "5" }
}
```

In Kubernetes, enable API server audit logging. See [`../snippets/k8s-audit-policy.yaml`](../snippets/k8s-audit-policy.yaml) for a policy that logs secrets/configmaps/serviceaccounts at Metadata level and RBAC mutations at RequestResponse level.

## First alerts to build

- Spike in SSH failures or Fail2Ban bans
- New listeners on unexpected ports
- Root logins or logins by accounts outside `AllowGroups`/`AllowUsers`
- Changes to `/etc/ssh`, `/etc/sudoers`, `/etc/passwd`, `/etc/shadow`, or Docker daemon configuration
- Shell spawned by a web process or package manager execution inside a container
- Writes to sensitive paths; unexpected use of `nc`, `curl`, `wget`, `chmod +s`, or `setcap`
- Sustained CPU spikes or suspicious outbound connections to mining/Stratum endpoints

## Incident response runbook

Order: **isolate → preserve → assess → eradicate → recover → rotate → review**

1. **Contain spread** — close security group/firewall access, remove from load balancer, disable risky SSH paths, cordon/drain the Kubernetes node, or isolate the container/host.
2. **Preserve evidence** — collect `journalctl`, `audit.log`, process lists, socket lists, disk/VM snapshots, Kubernetes audit logs. Use `snippets/incident-triage.sh` (read-only collector).
3. **Assess scope** — identify exposed users, secrets, tokens, kubeconfigs, registry credentials, SSH keys, and service accounts.
4. **Eradicate** — if host trust is lost, rebuild from a trusted image rather than "cleaning" in place.
5. **Recover** — restore from trusted backups or redeploy from known-good infrastructure-as-code.
6. **Rotate credentials** — all potentially exposed credentials: cloud keys, registry tokens, service account tokens, database passwords, SSH keys.
7. **Review** — fix the root cause, add detection coverage, update runbooks.

## eBPF runtime detection: Falco vs Tetragon

Preventive controls are not enough. Runtime detection catches suspicious behavior post-deployment.

Useful detection classes: shell spawned by web server; package manager execution inside a container; writes to `/etc`, `/usr/bin`, `/proc/sys`; unexpected outbound connections; privilege escalation attempts; crypto-mining patterns; cloud metadata endpoint access; unexpected Kubernetes API use by workloads.

**Falco** (CNCF Graduation) uses eBPF to intercept syscalls and emit alerts via a rules engine. Broad observability and telemetry collection; alerting only — does not block.

**Tetragon** (Cilium project) applies policies inside the kernel — filtering, aggregation, and enforcement happen synchronously with the syscall. Can send `SIGKILL` before a violation completes (e.g., block writes to `/etc/shadow`). Overhead ~1–2.5% CPU since only matched events cross the kernel–userspace boundary. Policies are Kubernetes CRDs, making GitOps integration straightforward.

Mature platforms combine both: Falco for broad monitoring; Tetragon for surgical enforcement of critical paths (privilege escalation, shell execution, writes to sensitive directories). Correlate eBPF events with SBOM-derived CVE data and misconfiguration findings in a CSPM platform for a full attack chain view.
