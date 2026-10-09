#!/usr/bin/env bash
# Spin up an ISOLATED MyOGChat instance on a dedicated test SQLite DB, seed an
# admin + a throwaway test user directly in that DB, then run the agent-browser
# UI battery against it. Your real database is never touched.
#
# Usage: scripts/ui-test-run.sh
# Env:
#   MYOGCHAT_TEST_DB    (default /tmp/opencode/myogchat-ui.db)
#   MYOGCHAT_TEST_PORT  (default 8791)
#   OPENCODE_API_KEY    (default: read from ~/.local/share/opencode/auth.json)
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DB="${MYOGCHAT_TEST_DB:-/tmp/opencode/myogchat-ui.db}"
PORT="${MYOGCHAT_TEST_PORT:-8791}"
BIN="${MYOGCHAT_TEST_BIN:-/tmp/opencode/myogchat-uitest-bin}"
TEST_USER="uitest"
TEST_PASS="uitestpass123"
ADMIN_USER="admin"
ADMIN_PASS="adminpass1"

cd "$ROOT"

echo "== build =="
mise exec -- go build -o "$BIN" .

echo "== fresh test db: $DB =="
rm -f "$DB" "$DB-wal" "$DB-shm"
export MYOGCHAT_DB="$DB"

echo "== seed users directly in the test db =="
"$BIN" user add --username "$ADMIN_USER" --email admin@example.com --password "$ADMIN_PASS" --admin
"$BIN" user add --username "$TEST_USER" --email uitest@example.com --password "$TEST_PASS"
"$BIN" user list

if [ -z "${OPENCODE_API_KEY:-}" ]; then
  OPENCODE_API_KEY="$(jq -r '."opencode-go".key' ~/.local/share/opencode/auth.json 2>/dev/null || true)"
fi

echo "== start isolated server on :$PORT =="
OPENCODE_API_KEY="$OPENCODE_API_KEY" MYOGCHAT_PORT="$PORT" MYOGCHAT_DB="$DB" \
  "$BIN" >/tmp/opencode/uitest-server.log 2>&1 &
SRV=$!
trap 'kill "$SRV" 2>/dev/null || true' EXIT
sleep 2

echo "== run battery =="
ADMIN_USER="$ADMIN_USER" ADMIN_PASS="$ADMIN_PASS" \
  bash scripts/ui-test.sh "http://127.0.0.1:$PORT" "$TEST_USER" "$TEST_PASS"
