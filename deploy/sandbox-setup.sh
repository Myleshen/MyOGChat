#!/usr/bin/env bash
# Provision the MyOGChat code sandbox (Phase 2.6 Python / 2.7 Bash).
#
#   deploy/sandbox-setup.sh            # part A: create network + pull images (no sudo)
#   sudo deploy/sandbox-setup.sh --harden   # part B: restrict sandbox egress (sudo)
#
# Part A (no sudo needed — the user is in the `docker` group):
#   * creates the dedicated user-defined bridge network (no published ports, so
#     there is no inbound path into a sandbox container), and
#   * pulls the Bash and Python images so `docker run --pull=never` works.
#
# Part B (needs sudo) adds DOCKER-USER iptables rules that DROP sandbox egress
# to RFC1918 / link-local / loopback / CGNAT ranges, leaving only the public
# internet reachable.
#
# IMPORTANT: without part B the sandbox containers can still reach your LAN
# (the owner's explicit choice is that the sandbox has outbound network access).
# Run `sudo deploy/sandbox-setup.sh --harden` to block LAN access.
set -euo pipefail

NETWORK="${MYOGCHAT_SANDBOX_NETWORK:-myogchat-sbx}"
BASH_IMAGE="${MYOGCHAT_SANDBOX_IMAGE_BASH:-bash:5.3}"
PYTHON_IMAGE="${MYOGCHAT_SANDBOX_IMAGE_PYTHON:-python:3.14-slim}"
DOCKER="${MYOGCHAT_DOCKER:-docker}"

echo "== [1/3] docker reachable =="
if ! "$DOCKER" info --format '{{.ServerVersion}}' >/dev/null 2>&1; then
  echo "cannot talk to the Docker daemon as $(id -un)." >&2
  echo "Is docker installed and is this user in the 'docker' group?" >&2
  exit 1
fi
"$DOCKER" info --format '   server {{.ServerVersion}}'

echo "== [2/3] network: $NETWORK =="
if "$DOCKER" network inspect "$NETWORK" >/dev/null 2>&1; then
  echo "   already exists"
else
  "$DOCKER" network create "$NETWORK" >/dev/null
  echo "   created"
fi

echo "== [3/3] images =="
for img in "$BASH_IMAGE" "$PYTHON_IMAGE"; do
  echo "   pulling $img"
  "$DOCKER" pull "$img"
done

SUBNET="$("$DOCKER" network inspect "$NETWORK" --format '{{range .IPAM.Config}}{{.Subnet}} {{end}}' 2>/dev/null | awk '{print $1}')"
echo
echo "sandbox network subnet: ${SUBNET:-<unknown>}"

# ---------------- part B: egress hardening (needs sudo) ----------------
if [ "${1:-}" != "--harden" ]; then
  cat <<EOF

Provisioning done. To also block sandbox access to your LAN, run:
  sudo MYOGCHAT_SANDBOX_NETWORK=$NETWORK bash deploy/sandbox-setup.sh --harden
EOF
  exit 0
fi

if [ "$(id -u)" -ne 0 ]; then
  echo "--harden must run as root (use sudo)." >&2
  exit 1
fi
if [ -z "$SUBNET" ]; then
  echo "could not determine the sandbox subnet; cannot add egress rules." >&2
  exit 1
fi

add4() {
  # Idempotent: skip when the exact rule already exists.
  if iptables -C DOCKER-USER "$@" 2>/dev/null; then
    return 0
  fi
  iptables -I DOCKER-USER "$@"
}

echo
echo "== hardening sandbox egress (subnet $SUBNET) =="
# Private, link-local, loopback and CGNAT destinations are dropped. Public
# internet is left alone. DOCKER-USER is evaluated before Docker's own
# forwarding rules, so this only affects forwarded (container) traffic.
for cidr in 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 169.254.0.0/16 127.0.0.0/8 100.64.0.0/10 224.0.0.0/4 255.255.255.255/32; do
  add4 -s "$SUBNET" -d "$cidr" -j DROP
  echo "   DROP $SUBNET -> $cidr"
done

# IPv6 link-local (best effort; user-defined networks are IPv4-only by default).
if command -v ip6tables >/dev/null 2>&1; then
  if ! ip6tables -C DOCKER-USER -s "$SUBNET" -d fe80::/10 -j DROP 2>/dev/null; then
    ip6tables -I DOCKER-USER -s "$SUBNET" -d fe80::/10 -j DROP 2>/dev/null || true
  fi
fi

echo
echo "Egress hardening applied. Sandbox containers can now reach the public"
echo "internet but not your LAN. Re-run with --harden after recreating the"
echo "network (subnet change) to refresh the rules."
