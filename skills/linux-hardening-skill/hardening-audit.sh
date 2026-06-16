#!/usr/bin/env bash
set -euo pipefail

# Read-only Linux public-IP hardening audit helper.
# It prints inventory and risk hints. It does not change system state.

section() { printf '\n\033[1m== %s ==\033[0m\n' "$1"; }
run() {
  echo "+ $*"
  "$@" 2>&1 || true
}

section "System"
run uname -a
if command -v lsb_release >/dev/null 2>&1; then run lsb_release -a; fi
if [ -r /etc/os-release ]; then run grep -E '^(PRETTY_NAME|ID|VERSION_ID)=' /etc/os-release; fi

section "SSH effective settings"
if command -v sshd >/dev/null 2>&1; then
  run sshd -t
  run sshd -T | grep -E '^(permitrootlogin|pubkeyauthentication|passwordauthentication|permitemptypasswords|kbdinteractiveauthentication|authenticationmethods|allowusers|logingracetime|maxauthtries|maxstartups|disableforwarding|allowtcpforwarding|x11forwarding|allowagentforwarding|permittunnel|clientaliveinterval|clientalivecountmax) '
else
  echo "sshd not found"
fi

section "Listening sockets"
run ss -plnt
run ss -plnu

section "Firewall state"
if command -v ufw >/dev/null 2>&1; then run ufw status verbose; fi
if command -v firewall-cmd >/dev/null 2>&1; then run firewall-cmd --list-all; fi
if command -v nft >/dev/null 2>&1; then run nft list ruleset; fi

section "Enabled services"
run systemctl list-unit-files --type=service --state=enabled

section "High-risk service hints"
for svc in cups cups-browsed avahi-daemon rpcbind nfs-server cockpit docker lxd libvirtd; do
  if systemctl list-unit-files "${svc}.service" 2>/dev/null | grep -q "${svc}\.service"; then
    state=$(systemctl is-enabled "${svc}.service" 2>/dev/null || true)
    active=$(systemctl is-active "${svc}.service" 2>/dev/null || true)
    printf '%-18s enabled=%-10s active=%s\n' "$svc" "${state:-unknown}" "${active:-unknown}"
  fi
done

section "Sysctl hardening sample"
for key in \
  net.ipv4.tcp_syncookies \
  net.ipv4.conf.all.accept_redirects \
  net.ipv4.conf.all.send_redirects \
  net.ipv4.conf.all.accept_source_route \
  net.ipv4.conf.all.rp_filter \
  net.ipv6.conf.all.accept_redirects \
  net.ipv6.conf.all.accept_source_route \
  net.ipv6.conf.all.disable_ipv6 \
  kernel.dmesg_restrict \
  kernel.kptr_restrict \
  kernel.randomize_va_space \
  kernel.unprivileged_bpf_disabled \
  fs.protected_hardlinks \
  fs.protected_symlinks \
  fs.protected_fifos \
  fs.protected_regular; do
  run sysctl "$key"
done

section "Sudo configuration"
run grep -rn 'NOPASSWD' /etc/sudoers /etc/sudoers.d/ 2>/dev/null || echo "no NOPASSWD entries found"
run grep -rn 'ALL=(ALL' /etc/sudoers /etc/sudoers.d/ 2>/dev/null || true

section "Automatic updates"
for svc in unattended-upgrades dnf-automatic apt-daily-upgrade yum-cron; do
  if systemctl list-unit-files "${svc}.service" "${svc}.timer" 2>/dev/null | grep -qE "${svc}\.(service|timer)"; then
    state=$(systemctl is-enabled "${svc}.service" 2>/dev/null || systemctl is-enabled "${svc}.timer" 2>/dev/null || true)
    active=$(systemctl is-active "${svc}.service" 2>/dev/null || systemctl is-active "${svc}.timer" 2>/dev/null || true)
    printf '%-28s enabled=%-10s active=%s\n' "$svc" "${state:-unknown}" "${active:-unknown}"
  fi
done

section "MAC status"
if command -v getenforce >/dev/null 2>&1; then run getenforce; fi
if command -v aa-status >/dev/null 2>&1; then run aa-status; fi

section "Logging and audit"
run systemctl is-active auditd
if command -v auditctl >/dev/null 2>&1; then run auditctl -l; fi
run journalctl -u ssh -S today --no-pager
run journalctl -u sshd -S today --no-pager

section "Container privilege hints"
if getent group docker >/dev/null 2>&1; then
  echo "docker group members: $(getent group docker | cut -d: -f4)"
  echo "Note: docker group membership is effectively root-equivalent on typical Docker hosts."
fi

section "Done"
echo "Review output for exposed management ports, password/root SSH access, missing firewall default-deny, permissive MAC, and unexpected enabled services."
