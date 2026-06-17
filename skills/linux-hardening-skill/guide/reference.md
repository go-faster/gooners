# Tool Reference, Attack Patterns, and Authoritative Standards

Reference for the [`linux-hardening`](../SKILL.md) skill.

## Tool reference

| Tool | Coverage | Best use |
|---|---|---|
| **Lynis** | Linux/Unix host audit | Quick agentless security audit and hardening hints. No compliance framework. |
| **OpenSCAP** | Compliance baselines / SCAP | CIS, DISA STIG, NIST CSF formal compliance validation and reporting. |
| **Trivy** | Container images, FS, IaC, K8s, SBOM | Universal scanner: CVEs, misconfigurations, secrets, SBOM-aware. CI/CD and registry gate. |
| **Clair** | Container image static analysis | Registry-side static CVE analysis. Not a runtime detector. |
| **Anchore** | SBOM management, vuln scanning, policy | Strong SBOM work and continuous monitoring for regulated/enterprise environments. |
| **Falco** | Runtime detection host/container/K8s | Detects abnormal behavior at runtime via kernel events. Does not block. |
| **Tetragon** | Runtime enforcement host/container/K8s | In-kernel enforcement; can terminate processes on policy violation. |
| **Wazuh / OSSEC** | HIDS/XDR/SIEM, FIM, rootkit detection | Host monitoring, file integrity monitoring, and threat detection. Needs rule tuning. |
| **Docker Bench for Security** | CIS Docker Benchmark | Quick CIS check of daemon.json, file permissions, and container configs. No license cost. |
| **Cosign / Sigstore** | Image signing and attestation | Keyless signing via OIDC; provenance attestation; Rekor transparency log. |
| **Kyverno / OPA Gatekeeper** | Kubernetes admission policy | Policy-as-code enforcement at the API server; verify images, workload security, RBAC. |
| **Hadolint** | Dockerfile linting | AST-based Dockerfile security rules + ShellCheck on RUN commands. |

## Attack pattern quick reference

| Attack | Primary controls |
|---|---|
| **SSH brute-force** | Key-only auth, `PermitRootLogin no`, `AllowGroups`, firewall allowlist, `pam_faillock`, Fail2Ban |
| **LFI/RCE in application** | Patch, unprivileged service user, egress restriction, Falco/auditd on shell/network anomalies, container sandboxing |
| **Privilege escalation** | `sudo` least privilege, `no-new-privileges`, `allowPrivilegeEscalation: false`, dropped capabilities, SELinux/AppArmor |
| **Container escape** | No `--privileged`, no host namespaces, seccomp/AppArmor/SELinux, user namespaces, rootless/sandbox runtime |
| **Supply chain compromise** | Digest pinning, SBOM, CVE scan, secret scan, image signing, admission signature verification |
| **Crypto-mining** | Falco runtime rules, Wazuh behavior detection, CPU/load/network alerts via Prometheus/Alertmanager |

## Authoritative standards

- NIST SP 800-123 — Guide to General Server Security
- NIST SP 800-190 — Application Container Security Guide
- NIST SP 800-61 Rev. 3 — Incident Response Recommendations and Considerations
- OpenSSH manual pages: `sshd_config(5)`, `ssh-keygen(1)`, `ssh_config(5)`
- Docker official security docs: daemon hardening, rootless mode, userns-remap, seccomp, AppArmor
- Kubernetes official security docs: Pod Security Standards, SecurityContext, RBAC, NetworkPolicy, Admission Controllers, Auditing
- OWASP Cheat Sheets: Docker Security, Secrets Management, CI/CD Security
- CIS Benchmarks for Linux, Docker, and Kubernetes
- CNCF Cloud Native Security Whitepaper v2
- NSA/CISA Kubernetes Hardening Guidance

## Compatibility caveats

- Examples target modern Linux with `systemd`, `OpenSSH`, `nftables`, Docker/containerd, and Kubernetes 1.25+.
- Debian/Ubuntu and RHEL-like systems differ in `sudo` group name (`sudo` vs `wheel`), PAM module packages for MFA, and `pam_faillock` configuration paths.
- `rp_filter=1` breaks asymmetric routing, multi-WAN, and some Kubernetes CNI plugins — use `=2` (loose mode) if you see dropped legitimate traffic.
- `userns-remap` and rootless mode require compatibility validation with your storage driver and volume workflows before production rollout.
- `PodSecurityPolicy` was removed in Kubernetes 1.25; use Pod Security Admission for new clusters.
- `kernel.kexec_load_disabled=1` is irreversible until reboot — skip on hosts that use kexec for live-patching.
