#!/usr/bin/env bash
# Build MyOGChat for the Raspberry Pi and install it as a systemd *user* service.
#
# Usage: deploy/deploy.sh [ssh-host]      (default: pi@raspberrypi.local)
#
# Requires the env file to already exist on the target at ~/.config/myogchat/env
# (chmod 600) with OPENCODE_API_KEY (and any optional keys). See env.example.
#
# After the first deploy, enable lingering once so the service survives logout
# and reboot:
#   ssh <host> sudo loginctl enable-linger "$USER"
set -euo pipefail

HOST="${1:-pi@raspberrypi.local}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$(mktemp -d)/myogchat"

# Use go from PATH, or fall back to the project-pinned mise toolchain.
if command -v go >/dev/null 2>&1; then
  GO=(go)
elif command -v mise >/dev/null 2>&1; then
  GO=(mise exec -- go)
else
  echo "error: neither go nor mise found on PATH" >&2
  exit 1
fi

echo "==> building linux/arm64"
(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 "${GO[@]}" build -trimpath -ldflags="-s -w" -o "$BIN" .)

echo "==> installing on $HOST"
ssh "$HOST" 'mkdir -p ~/.local/bin ~/.config/systemd/user'
scp -q "$BIN" "$HOST:~/.local/bin/myogchat.new"
scp -q "$ROOT/deploy/myogchat.service" "$HOST:~/.config/systemd/user/myogchat.service"
ssh "$HOST" '
  set -e
  mv ~/.local/bin/myogchat.new ~/.local/bin/myogchat
  chmod 755 ~/.local/bin/myogchat
  systemctl --user daemon-reload
  systemctl --user enable --now myogchat
  systemctl --user restart myogchat
  sleep 1
  systemctl --user --no-pager --lines=0 status myogchat || true
'
echo "==> done. Verify: curl -s -o /dev/null -w '%{http_code}\\n' http://${HOST#*@}:8787/api/models   (expect 401 without login)"
