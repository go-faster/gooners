#!/usr/bin/env bash
# Legacy iptables deny-by-default baseline.
# Prefer nftables-minimal.nft on modern systems.
# Replace the SSH source IP with your bastion/admin range.
set -euo pipefail

iptables -P INPUT DROP
iptables -P FORWARD DROP
iptables -P OUTPUT ACCEPT

iptables -A INPUT -i lo -j ACCEPT
iptables -A INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
iptables -A INPUT -m conntrack --ctstate INVALID -j DROP
iptables -A INPUT -p icmp -j ACCEPT
iptables -A INPUT -p tcp -s 198.51.100.10/32 --dport 22 -m conntrack --ctstate NEW -j ACCEPT
iptables -A INPUT -p tcp -m multiport --dports 80,443 -m conntrack --ctstate NEW -j ACCEPT
