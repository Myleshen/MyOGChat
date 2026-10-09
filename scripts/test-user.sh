#!/usr/bin/env bash
# Add/remove/list users directly in a MyOGChat database — no admin credentials
# and no running server needed (handy for throwaway test accounts).
#
# Usage:
#   scripts/test-user.sh add --username uitest --email uitest@example.com --password uitestpass123
#   scripts/test-user.sh rm  --username uitest
#   scripts/test-user.sh list
#
# Env:
#   MYOGCHAT_DB   target database (default: ~/.local/share/myogchat/chat.db)
#   MYOGCHAT_BIN  prebuilt binary (default: `mise exec -- go run .`)
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
if [ -n "${MYOGCHAT_BIN:-}" ]; then
  exec "$MYOGCHAT_BIN" user "$@"
fi
exec mise exec -- go run . user "$@"
