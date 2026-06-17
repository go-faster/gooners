#!/usr/bin/env bash
# Supply-chain security pipeline: scan → build → scan → SBOM → sign.
# Requires: trivy, syft, cosign (authenticated via OIDC in CI environment).
# Set IMAGE before running, e.g.:
#   IMAGE=registry.example.com/team/app:$(git rev-parse --short HEAD) bash supply-chain-ci.sh
set -euo pipefail

IMAGE="${IMAGE:?set IMAGE, e.g. registry.example.com/app:1.0.0}"

# 1. Scan Dockerfile and IaC for misconfigurations before building.
trivy config --exit-code 1 .

# 2. Scan source filesystem for vulnerabilities and embedded secrets.
trivy fs --scanners vuln,secret,misconfig --exit-code 1 .

# 3. Build the image.
docker build -t "$IMAGE" .

# 4. Scan the built image; fail on HIGH or CRITICAL CVEs.
trivy image --exit-code 1 --severity HIGH,CRITICAL "$IMAGE"

# 5. Generate SBOM in SPDX JSON format.
syft "$IMAGE" -o spdx-json > sbom.spdx.json

# 6. Sign the image with Sigstore keyless signing (OIDC-based, no long-lived key).
cosign sign "$IMAGE"

# 7. Attach the SBOM as a signed attestation.
cosign attest --predicate sbom.spdx.json --type spdx "$IMAGE"
