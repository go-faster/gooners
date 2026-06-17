# SSH Hardening — Deep Dive

Reference for the [`linux-hardening`](../SKILL.md) skill. See [`../snippets/ssh-bootstrap.sh`](../snippets/ssh-bootstrap.sh) and [`../snippets/sshd_config`](../snippets/sshd_config) for ready-to-use configs.

## Bootstrap sequence

Configure SSH in two stages to avoid lockout: create and test a named admin account with key-based login in a second active session, then disable passwords and root login.

`ssh-bootstrap.sh` automates this: creates the admin user, installs the public key, and reloads sshd in one step. Run as root with `ADMIN_USER`, `ADMIN_GROUP`, and `PUBKEY_FILE` set.

## MFA options

Two practical patterns:

**1. Hardware-backed security keys** — `ed25519-sk` or `ecdsa-sk`. No per-host secret; resident keys stored on hardware token. Best for high-sensitivity environments.

**2. TOTP via PAM** — `publickey,keyboard-interactive:pam` requires both factors in sequence.

```conf
# /etc/ssh/sshd_config.d/10-hardening.conf
KbdInteractiveAuthentication yes
UsePAM yes
AuthenticationMethods publickey,keyboard-interactive
```

PAM fragment:

```pam
# /etc/pam.d/sshd
auth required pam_google_authenticator.so
```

For fleets, OpenSSH CA certificates (`TrustedUserCAKeys`) are operationally simpler: revocation happens at the CA level rather than touching individual `authorized_keys` files.

## Bastion / ProxyJump

Use a bastion whenever a server does not need direct SSH exposure to the Internet.

```conf
# ~/.ssh/config
Host bastion
    HostName bastion.example.net
    User alice
    IdentityFile ~/.ssh/id_ed25519

Host app-01
    HostName 10.10.20.11
    User alice
    ProxyJump bastion
    IdentityFile ~/.ssh/id_ed25519
```

## Fail2Ban

Reduces brute-force noise; not a substitute for key-only auth or network restrictions.

```ini
# /etc/fail2ban/jail.local
[DEFAULT]
bantime  = 1h
findtime = 10m
maxretry = 5
backend  = systemd
banaction = nftables-multiport

[sshd]
enabled = true
mode    = aggressive
port    = 22
```

## pam_faillock

Locks local accounts after repeated failures. Apply carefully on remote servers — misconfiguration can lock out break-glass access.

Configure via `/etc/security/faillock.conf`.

## SFTP-only chroot

`ChrootDirectory` is useful for SFTP-only accounts. Chroot paths must be owned by root and not writable by group or others.

```conf
Match Group sftp-only
    ChrootDirectory /srv/sftp/%u
    ForceCommand internal-sftp
    X11Forwarding no
    AllowTcpForwarding no
    PermitTTY no
```

## Port change

Changing the SSH port reduces scanner noise but is not meaningful hardening. Do it only after keys, root-login disablement, firewall, and monitoring are in place.
