# Supply Chain Security — Deep Dive

Reference for the [`linux-hardening`](../SKILL.md) skill. See [`../snippets/supply-chain-ci.sh`](../snippets/supply-chain-ci.sh) for the full CI/CD pipeline script.

## Base image strategy

The base image determines the initial attack surface:

| Base image | Characteristics | Best use |
|---|---|---|
| `scratch` | Empty; no shell, libc, package manager, or OS tools | Static Go/Rust binaries with `CGO_ENABLED=0` |
| Distroless | Minimal runtime files, CA certs, users, libraries; no shell/package manager | Go, Java, Python, Node.js, and most production apps |
| Alpine | Small Linux with musl, BusyBox, and `apk` | Lightweight images where shell/package manager tradeoffs are acceptable |
| Hardened vendor images | Backported fixes, SBOMs, compliance support | Regulated environments needing formal support and SLA |

Use multi-stage builds to keep compilers, package managers, source code, and build tools out of the runtime image.

See [`../snippets/Dockerfile.go-distroless`](../snippets/Dockerfile.go-distroless) for a complete Go multi-stage example.

## Dockerfile linting with Hadolint

Hadolint converts Dockerfiles to an AST and applies security rules, also running ShellCheck on embedded `RUN` commands.

| Rule | Severity | What it prevents |
|---|---|---|
| DL3000 | Error | Relative `WORKDIR` paths |
| DL3004 | Error | `sudo` in Dockerfiles — use `gosu` or `USER` instead |
| DL3007 | Warning | `latest` in `FROM` — use pinned versions or digests |
| DL3008/3013/3018 | Warning | Unpinned package versions (`apt-get`, `pip`, `apk`) |
| DL3020 | Error | `ADD` with implicit remote fetch/extract — use `COPY` |
| DL3059 | Info | Multiple `RUN` layers that leave temp files in image history |

Integrate Hadolint as a pre-commit hook and CI step.

## CI/CD pipeline

`snippets/supply-chain-ci.sh` runs this pipeline in order:

1. `trivy config` — IaC/Dockerfile misconfiguration scan
2. `trivy fs` — filesystem CVE, secret, and misconfiguration scan
3. `docker build` — produce the image
4. `trivy image` — CVE scan; fails on HIGH/CRITICAL findings
5. `syft` — SBOM generation in SPDX JSON format
6. `cosign sign` — keyless image signature via OIDC/Sigstore
7. `cosign attest` — attach the SBOM as a signed attestation

Minimum pipeline gate:
```
commit → tests → SAST/IaC scan → build → SBOM → image scan → sign → push by digest → admission verify → deploy
```

## Sigstore keyless signing

The CI runner authenticates via OIDC (e.g., GitHub Actions OIDC), obtains a short-lived certificate from Fulcio, signs the image digest, and records the signature in the Rekor transparency log. The private key is ephemeral — it never exists on disk after the operation.

Admission controllers (Kyverno, Sigstore Policy Controller) verify these Rekor entries before allowing pods to start.

See [`../snippets/kyverno-require-signed-images.yaml`](../snippets/kyverno-require-signed-images.yaml) for a Kyverno `ClusterPolicy` that enforces keyless Cosign signatures via OIDC issuer matching.

## CI/CD infrastructure hardening

Pipeline compromise often yields supply-chain impact across many deployments:

- Use short-lived, machine-specific identities for CI runners (OIDC tokens, workload identity federation).
- Store secrets in a vault, not in CI environment variables.
- Protect release branches and tag signing.
- Do not allow pipelines to push unsigned images to production registries.
- Scan IaC, Dockerfiles, and code for secrets before build.
