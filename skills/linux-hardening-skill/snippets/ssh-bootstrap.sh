#!/usr/bin/env bash
# Creates a named admin user, installs an SSH public key, and reloads sshd.
# Run as root or with sudo. Set variables before executing.
#
#   ADMIN_USER=alice ADMIN_GROUP=sshadmins PUBKEY_FILE=/tmp/id_ed25519.pub bash ssh-bootstrap.sh
#
# Only run this BEFORE disabling password auth. Keep a second SSH session open
# to verify key login works before reloading sshd.
set -euo pipefail

ADMIN_USER="${ADMIN_USER:-alice}"
ADMIN_GROUP="${ADMIN_GROUP:-sshadmins}"
PUBKEY_FILE="${PUBKEY_FILE:-/tmp/id_ed25519.pub}"

sudo groupadd --system "$ADMIN_GROUP" 2>/dev/null || true
sudo useradd -m -s /bin/bash "$ADMIN_USER" 2>/dev/null || true
sudo usermod -aG "$ADMIN_GROUP" "$ADMIN_USER"

if getent group sudo >/dev/null; then
  sudo usermod -aG sudo "$ADMIN_USER"
elif getent group wheel >/dev/null; then
  sudo usermod -aG wheel "$ADMIN_USER"
fi

sudo install -d -m 700 -o "$ADMIN_USER" -g "$ADMIN_USER" "/home/$ADMIN_USER/.ssh"
sudo install -m 600 -o "$ADMIN_USER" -g "$ADMIN_USER" "$PUBKEY_FILE" "/home/$ADMIN_USER/.ssh/authorized_keys"

sudo sshd -t
sudo systemctl reload sshd || sudo systemctl reload ssh
