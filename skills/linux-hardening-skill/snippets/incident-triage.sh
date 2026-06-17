#!/usr/bin/env bash
# Read-only triage collector. Produces a tarball at $OUT.tar.gz.
# Run as root on a suspected host BEFORE making any changes.
# Does not modify system state.
set -euo pipefail

OUT="${OUT:-/tmp/triage-$(hostname)-$(date -u +%Y%m%dT%H%M%SZ)}"
mkdir -p "$OUT"

uname -a > "$OUT/uname.txt"
date -u > "$OUT/date-utc.txt"
who -a > "$OUT/who.txt" || true
last -Faiwx > "$OUT/last.txt" || true
ps auxwwf > "$OUT/ps.txt" || true
ss -tulpn > "$OUT/listeners.txt" || true
ip a > "$OUT/ip-a.txt" || true
ip route > "$OUT/ip-route.txt" || true
systemctl list-unit-files --type=service --state=enabled > "$OUT/enabled-services.txt" || true
journalctl --since "24 hours ago" > "$OUT/journal-24h.txt" || true
journalctl -u ssh -u sshd --since "24 hours ago" > "$OUT/ssh-journal-24h.txt" || true
auditctl -l > "$OUT/audit-rules.txt" 2>/dev/null || true
ausearch -ts today > "$OUT/audit-today.txt" 2>/dev/null || true

docker ps --no-trunc > "$OUT/docker-ps.txt" 2>/dev/null || true
docker images --digests > "$OUT/docker-images.txt" 2>/dev/null || true
kubectl get pods -A -o wide > "$OUT/k8s-pods.txt" 2>/dev/null || true
kubectl get events -A --sort-by=.lastTimestamp > "$OUT/k8s-events.txt" 2>/dev/null || true

tar -C "$(dirname "$OUT")" -czf "$OUT.tar.gz" "$(basename "$OUT")"
echo "Triage saved to: $OUT.tar.gz"
