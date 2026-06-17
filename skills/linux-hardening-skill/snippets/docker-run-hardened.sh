#!/usr/bin/env bash
# Least-privilege docker run template.
# Set IMAGE and NAME before running.
# Add --cap-add NET_BIND_SERVICE if the app must bind ports below 1024.
set -euo pipefail

IMAGE="${IMAGE:-example/app:1.0.0}"
NAME="${NAME:-app}"

exec docker run --rm --name "$NAME" \
  --user 10001:10001 \
  --read-only \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=64m \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --security-opt seccomp=default \
  --pids-limit 256 \
  --memory 256m \
  --cpus 1 \
  --network bridge \
  "$IMAGE"
