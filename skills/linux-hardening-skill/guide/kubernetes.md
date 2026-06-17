# Kubernetes Hardening — Deep Dive

Reference for the [`linux-hardening`](../SKILL.md) skill. See the `snippets/k8s-*.yaml` and `snippets/kyverno-*.yaml` files for ready-to-use configs.

## Pod Security Admission

Use Pod Security Admission (PSA) and target the `restricted` profile. `PodSecurityPolicy` was removed in Kubernetes 1.25 — do not use it for new clusters.

Enable PSA on a namespace:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: production
  labels:
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/audit: restricted
    pod-security.kubernetes.io/warn: restricted
```

See [`../snippets/k8s-secure-pod.yaml`](../snippets/k8s-secure-pod.yaml) for a complete Pod spec meeting the `restricted` baseline: `runAsNonRoot`, `runAsUser: 10001`, `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, `capabilities.drop: ["ALL"]`, `seccompProfile: RuntimeDefault`, `automountServiceAccountToken: false`, and CPU/memory requests and limits.

## Admission controls

Use Kyverno, OPA Gatekeeper, or `ValidatingAdmissionPolicy` (1.26+ GA) to enforce cluster-wide:

- no privileged pods
- no host namespaces (`hostPID`, `hostIPC`, `hostNetwork`) unless explicitly approved
- non-root execution
- `RuntimeDefault` seccomp
- dropped capabilities
- resource requests/limits
- approved image registries
- signed image attestations
- service account token automounting disabled by default

See [`../snippets/kyverno-require-signed-images.yaml`](../snippets/kyverno-require-signed-images.yaml).

## RBAC review commands

```bash
kubectl get clusterrolebinding -o wide
kubectl get rolebinding -A -o wide
kubectl auth can-i --as system:serviceaccount:default:app-sa get secrets -n default
kubectl get pods -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{" automount="}{.spec.automountServiceAccountToken}{"\n"}{end}'
```

Keep RBAC narrow: no wildcard verbs/resources, no broad `cluster-admin`, separate deployment identities from runtime workload identities, disable `automountServiceAccountToken` by default.

## NetworkPolicy

Start with default-deny, then add explicit allow rules for each required communication path.

See [`../snippets/k8s-default-deny-networkpolicy.yaml`](../snippets/k8s-default-deny-networkpolicy.yaml) for a default-deny policy covering both Ingress and Egress in a namespace.

See [`../snippets/k8s-audit-policy.yaml`](../snippets/k8s-audit-policy.yaml) for a Kubernetes audit policy that logs secrets/configmaps/serviceaccounts at Metadata level and RBAC mutations at RequestResponse level.

## Sandbox runtimes

For multi-tenant or high-risk workloads, use `RuntimeClass`:

- **gVisor** (`runsc`) — runs container syscalls through a userspace kernel; significantly reduces kernel attack surface; adds latency.
- **Kata Containers** — runs each container in a lightweight VM; strongest isolation; higher overhead and compatibility tradeoffs.

Both have compatibility tradeoffs with some syscalls and storage drivers.

## Standards mapping

- **NIST SP 800-190** — frames container risk across images, registries, orchestrators, runtime, and host OS.
- **CIS Kubernetes Benchmark** — measurable controls for API server, kubelet, etcd, RBAC, and pod security.
- **NSA/CISA Kubernetes Hardening Guidance** — emphasizes pod security, service account token control, immutable filesystems, namespace isolation, RBAC minimization.
- **CNCF Cloud Native Security Whitepaper v2** — organizes security across Develop, Distribute, Deploy, and Runtime phases.
