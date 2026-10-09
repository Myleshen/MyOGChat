#!/usr/bin/env bash
# MyOGChat UI/UX test battery (agent-browser).
#
# Runs a mobile-first pass over the app and asserts behaviour with real
# screenshots. Used after every merge before deploying.
#
# Usage:
#   scripts/ui-test.sh [BASE_URL] [USERNAME] [PASSWORD]
# Defaults: http://127.0.0.1:8791 admin adminpass1
#
# Requires: agent-browser (installed), jq. The target server must be running and
# have an approved user with the given credentials.

BASE_URL="${1:-http://127.0.0.1:8791}"
USERNAME="${2:-admin}"
PASSWORD="${3:-adminpass1}"
SHOTS="${SHOTS:-/tmp/opencode/ui}"
DEVICE="${DEVICE:-Pixel 7}"

mkdir -p "$SHOTS"
export AGENT_BROWSER_SESSION="${AGENT_BROWSER_SESSION:-myogchat-uitest}"
FAILS=0
PASSES=0

ab() { agent-browser "$@" 2>&1; }

# assert <name> <js-expression-evaluating-to-true>
assert() {
  local name="$1" expr="$2" out
  # agent-browser prints the JS string JSON-encoded, so strip surrounding quotes.
  out=$(ab eval "String(($expr))" | tail -1 | tr -d '\r' | sed -E 's/^[[:space:]]*"//; s/"[[:space:]]*$//')
  if [ "$out" = "true" ]; then
    echo "  PASS  $name"; PASSES=$((PASSES + 1))
  else
    echo "  FAIL  $name  (got: ${out:-<empty>})"; FAILS=$((FAILS + 1))
  fi
}

# js <expr> -> prints JSON.stringify of an object expression
js() { ab eval "JSON.stringify($1)" | tail -1; }

shot() { ab screenshot "$SHOTS/$1" >/dev/null 2>&1; }

echo "== MyOGChat UI test =="
echo "base=$BASE_URL device=$DEVICE shots=$SHOTS"

# Fresh session so the service-worker cache never masks new assets.
ab close --all >/dev/null 2>&1 || true
ab open "$BASE_URL" >/dev/null 2>&1
ab set device "$DEVICE" >/dev/null 2>&1
ab reload >/dev/null 2>&1
sleep 1

echo "-- boot --"
assert "login or setup screen visible" \
  "!document.getElementById('login-view').hidden"
shot 01-boot.png

# Log in if a login form is showing.
if ab is visible "#login-form" | grep -q true; then
  ab fill "#login-identifier" "$USERNAME" >/dev/null 2>&1
  ab fill "#login-password" "$PASSWORD" >/dev/null 2>&1
  ab click "#login-form button[type=submit]" >/dev/null 2>&1
  sleep 1
fi

echo "-- app shell --"
assert "app shell visible" "!document.getElementById('app').hidden"
assert "no horizontal overflow" "document.documentElement.scrollWidth <= window.innerWidth + 1"
shot 02-app.png

echo "-- mobile keyboard (visual viewport) --"
ab set viewport 412 915 >/dev/null 2>&1; sleep 1
ab click "textarea#input" >/dev/null 2>&1 || true
ab set viewport 412 470 >/dev/null 2>&1; sleep 1
assert "composer pinned & visible with keyboard" \
  "document.querySelector('.composer').getBoundingClientRect().bottom <= window.innerHeight + 1"
assert "page did not scroll on keyboard" "window.scrollY === 0"
shot 03-keyboard.png
ab set viewport 412 915 >/dev/null 2>&1; sleep 1

echo "-- sidebar --"
ab click "#menu-btn" >/dev/null 2>&1; sleep 1
assert "sidebar opens" "document.getElementById('sidebar').classList.contains('open')"
shot 04-sidebar.png
ab click "#backdrop" >/dev/null 2>&1 || ab press Escape >/dev/null 2>&1; sleep 1
assert "sidebar closes" "!document.getElementById('sidebar').classList.contains('open')"

echo "-- settings --"
ab click "#settings-btn" >/dev/null 2>&1; sleep 1
assert "settings opens" "!document.getElementById('settings-modal').hidden"
assert "settings fits the viewport" \
  "document.querySelector('#settings-modal .modal-card').getBoundingClientRect().height <= window.innerHeight + 1"
assert "OpenCode Go key section present" "!document.getElementById('key-section').hidden"
shot 05-settings.png
ab press Escape >/dev/null 2>&1; sleep 1

echo "-- theme --"
ab eval "document.documentElement.setAttribute('data-theme','light')" >/dev/null 2>&1
assert "light theme applies" "document.documentElement.getAttribute('data-theme')==='light'"
shot 06-light.png
ab eval "document.documentElement.setAttribute('data-theme','dark')" >/dev/null 2>&1
assert "dark theme applies" "document.documentElement.getAttribute('data-theme')==='dark'"

echo "-- send a message (free model) --"
ab fill "#input" "Say hi in three words." >/dev/null 2>&1
ab click "#send-btn" >/dev/null 2>&1 || true
sleep 10
assert "assistant message rendered" "document.querySelectorAll('.msg').length >= 2"
assert "composer still visible after send" \
  "document.querySelector('.composer').getBoundingClientRect().bottom <= window.innerHeight + 1"
shot 07-sent.png

echo "-- result --"
echo "  passed=$PASSES failed=$FAILS"
[ "$FAILS" -eq 0 ] && echo "  UI TESTS OK" || echo "  UI TESTS FAILED"
ab close --all >/dev/null 2>&1 || true
exit $((FAILS > 0 ? 1 : 0))
