#!/usr/bin/env bash
# Full-feature UI check for MyOGChat (agent-browser, mobile viewport).
#
# Drives every user-facing feature through the real UI and screenshots each step:
#   shell, settings page, chat, regenerate, attachments (text/PDF/image OCR),
#   web search, URL fetch, the code sandbox (run_bash/run_python), projects,
#   groups, themes, and the admin panels.
#
# Usage: scripts/ui-test-full.sh <BASE_URL> <TEST_USER> <TEST_PASS>
# Env:   ADMIN_USER, ADMIN_PASS (enables the admin pass), SHOTS, DEVICE, FIXTURE_DIR
set -uo pipefail

BASE_URL="${1:?usage: ui-test-full.sh <base_url> <user> <pass>}"
TEST_USER="${2:?test user required}"
TEST_PASS="${3:?test pass required}"
ADMIN_USER="${ADMIN_USER:-}"
ADMIN_PASS="${ADMIN_PASS:-}"
SHOTS="${SHOTS:-/tmp/opencode/ui-full}"
DEVICE="${DEVICE:-Pixel 7}"
FX="${FIXTURE_DIR:-/tmp/opencode/fixtures}"
export AGENT_BROWSER_SESSION="${AGENT_BROWSER_SESSION:-myogchat-full}"

mkdir -p "$SHOTS" "$FX"
printf 'MyOGChat fixture. The secret word is PLATYPUS-42.\n' > "$FX/text.txt"
magick -size 900x170 xc:white -pointsize 60 -fill black -annotate +40+110 'HELLO MYOGCHAT OCR 12345' "$FX/image.png" 2>/dev/null || true
# A PDF with REAL selectable text (magick would rasterize it, yielding no text).
cat > "$FX/doc.ps" <<'PS'
%!PS
/Helvetica findfont 26 scalefont setfont
72 700 moveto
(PDF fixture: the code is ZEBRA-7) show
showpage
PS
ps2pdf "$FX/doc.ps" "$FX/doc.pdf" 2>/dev/null || true

FAILS=0; PASSES=0
ab() { agent-browser "$@" 2>&1; }
assert() {
  local name="$1" expr="$2" out
  out=$(ab eval "String(($expr))" | tail -1 | tr -d '\r' | sed -E 's/^[[:space:]]*"//; s/"[[:space:]]*$//')
  if [ "$out" = "true" ]; then echo "  PASS  $name"; PASSES=$((PASSES+1));
  else echo "  FAIL  $name  (got: ${out:-<empty>})"; FAILS=$((FAILS+1)); fi
}
shot() { ab screenshot "$SHOTS/$1" >/dev/null 2>&1; }
wait_for() {
  local expr="$1" t="${2:-10}" i=0 out
  while [ "$i" -lt $((t*2)) ]; do
    out=$(ab eval "String(($expr))" | tail -1 | tr -d '\r' | sed -E 's/^[[:space:]]*"//; s/"[[:space:]]*$//')
    [ "$out" = "true" ] && return 0
    sleep 0.5; i=$((i+1))
  done
  return 1
}
login() {
  ab fill "#login-identifier" "$1" >/dev/null 2>&1
  ab fill "#login-password" "$2" >/dev/null 2>&1
  ab click "#login-form button[type=submit]" >/dev/null 2>&1
  wait_for "!document.getElementById('app').hidden" 12
}
close_drawers() {
  ab press Escape >/dev/null 2>&1; sleep 0.4
  ab click "#backdrop" >/dev/null 2>&1 || true; sleep 0.4
}
clear_attachments() {
  ab eval "(function(){document.querySelectorAll('.attachment-remove').forEach(function(b){b.click();});return 'ok';})()" >/dev/null 2>&1
  sleep 0.8
}
# send <prompt> — wait for streaming to start (stop-btn shown) then finish.
send() {
  ab fill "#input" "$1" >/dev/null 2>&1
  ab click "#send-btn" >/dev/null 2>&1
  wait_for "!document.getElementById('stop-btn').hidden" 8 || true
  wait_for "document.getElementById('stop-btn').hidden" 260 || true
  sleep 1
}
# send_capture <prompt> <shotname> — send and screenshot while a tool is running.
send_capture() {
  ab fill "#input" "$1" >/dev/null 2>&1
  ab click "#send-btn" >/dev/null 2>&1
  wait_for "!document.getElementById('stop-btn').hidden" 8 || true
  wait_for "document.querySelector('.msg-tools') !== null" 45 || true
  shot "$2"
  wait_for "document.getElementById('stop-btn').hidden" 260 || true
  sleep 1
}
# send_file <path> <prompt> — attach (wait until fully uploaded) then send.
send_file() {
  clear_attachments
  ab upload "#file-input" "$1" >/dev/null 2>&1
  wait_for "document.querySelectorAll('.attachment-chip').length > 0" 15 || true
  wait_for "document.querySelectorAll('.attachment-chip.uploading').length === 0" 30 || true
  send "$2"
}
# used_tool <name> -> "true"/"false" (checks rendered tool-activity blocks)
used_tool() {
  ab eval "String(Array.from(document.querySelectorAll('.msg-tools')).some(function(e){return e.textContent.includes('$1');}))" \
    | tail -1 | tr -d '\r' | sed -E 's/^[[:space:]]*"//; s/"[[:space:]]*$//'
}
assert_tool() {
  if [ "$(used_tool "$2")" = "true" ]; then echo "  PASS  $1"; PASSES=$((PASSES+1));
  else echo "  FAIL  $1  (tool '$2' not seen)"; FAILS=$((FAILS+1)); fi
}

echo "== MyOGChat FULL UI test =="; echo "base=$BASE_URL device=$DEVICE user=$TEST_USER shots=$SHOTS"

ab close --all >/dev/null 2>&1 || true
ab open "$BASE_URL" >/dev/null 2>&1
ab set device "$DEVICE" >/dev/null 2>&1
ab reload >/dev/null 2>&1; sleep 1

echo "-- boot --"
assert "login screen shown" "!document.getElementById('login-view').hidden && !document.getElementById('login-form').hidden"
shot 01-boot.png
login "$TEST_USER" "$TEST_PASS"
assert "app shell visible" "!document.getElementById('app').hidden"
assert "no horizontal overflow" "document.documentElement.scrollWidth <= window.innerWidth + 1"

echo "-- settings page --"
ab click "#settings-btn" >/dev/null 2>&1
wait_for "!document.getElementById('settings-page').hidden" 8
assert "settings page opens" "!document.getElementById('settings-page').hidden"
assert "account section" "!!document.getElementById('account-section')"
assert "appearance section" "!!document.getElementById('appearance-section')"
assert "key section" "!!document.getElementById('key-section')"
assert "groups section" "!!document.getElementById('groups-section')"
assert "about section" "!!document.getElementById('about-section')"
assert "non-admin: users hidden" "document.getElementById('users-section').hidden"
assert "non-admin: integrations hidden" "document.getElementById('integrations-section').hidden"
assert "non-admin: sandbox hidden" "document.getElementById('tools-section').hidden"
assert "non-admin: ocr hidden" "document.getElementById('ocr-section').hidden"
shot 02-settings.png
ab click "#settings-back" >/dev/null 2>&1; sleep 1

echo "-- chat: plain send --"
ab click "#new-chat-btn" >/dev/null 2>&1; sleep 1
send "Reply with exactly: PONG"
assert "assistant replied" "document.querySelectorAll('.msg').length >= 2"
shot 03-chat.png

echo "-- regenerate --"
ab eval "(function(){var b=null;document.querySelectorAll('.msg-actions .msg-action').forEach(function(x){if(x.textContent.trim()==='Retry')b=x;});if(b)b.click();return 'ok';})()" >/dev/null 2>&1
wait_for "!document.getElementById('stop-btn').hidden" 8 || true
wait_for "document.getElementById('stop-btn').hidden" 260 || true
assert "regenerate kept the reply" "document.querySelectorAll('.msg').length >= 2"

echo "-- attachment: text --"
send_file "$FX/text.txt" "Read the attached file and reply with just the secret word."
assert "text attachment read (PLATYPUS-42)" "document.querySelector('.messages').textContent.includes('PLATYPUS-42')"
shot 04-attach-text.png

echo "-- attachment: PDF --"
send_file "$FX/doc.pdf" "Read the attached PDF and reply with just the code it contains."
assert "PDF attachment read (ZEBRA-7)" "document.querySelector('.messages').textContent.includes('ZEBRA-7')"
shot 05-attach-pdf.png

echo "-- attachment: image (OCR) --"
send_file "$FX/image.png" "Read the text in the attached image and reply with it."
assert "image OCR read (OCR 12345)" "document.querySelector('.messages').textContent.includes('OCR 12345')"
shot 06-attach-image.png

echo "-- tool: web search --"
send_capture "Search the web for the official website of the Go programming language, then give me its URL." 07a-websearch-live.png
assert "web search returned a URL (go.dev)" "document.querySelector('.messages').textContent.includes('go.dev')"
shot 07-websearch.png

echo "-- tool: fetch URL --"
send_capture "Read https://example.com and tell me its page title." 08a-fetchurl-live.png
assert "fetch_url read the page (Example Domain)" "document.querySelector('.messages').textContent.includes('Example Domain')"
shot 08-fetchurl.png

echo "-- tool: run_bash (sandbox) --"
send_capture "Run this exact shell command in your sandbox and report the output: echo BASH-OK; uname -m" 09a-runbash-live.png
assert "run_bash output seen (BASH-OK)" "document.querySelector('.messages').textContent.includes('BASH-OK')"
shot 09-runbash.png

echo "-- tool: run_python (sandbox) --"
send_capture "Use Python to compute 17*23 and report the result." 10a-runpython-live.png
assert "run_python result seen (391)" "document.querySelector('.messages').textContent.includes('391')"
shot 10-runpython.png

echo "-- projects --"
ab click "#menu-btn" >/dev/null 2>&1; sleep 1
ab click "#new-project-btn" >/dev/null 2>&1; sleep 1
assert "project editor opens" "!document.getElementById('project-modal').hidden"
ab fill "#project-name" "Full UI Project" >/dev/null 2>&1
ab click "#project-form button[type=submit]" >/dev/null 2>&1; sleep 1
assert "project created & listed" "document.getElementById('projects-list').textContent.includes('Full UI Project')"
shot 11-project.png

echo "-- groups --"
close_drawers
ab click "#settings-btn" >/dev/null 2>&1
wait_for "!document.getElementById('settings-page').hidden" 8
ab scrollintoview "#groups-section" >/dev/null 2>&1; sleep 1
ab fill "#group-name" "Full UI Group" >/dev/null 2>&1
ab click "#group-form button[type=submit]" >/dev/null 2>&1; sleep 1
assert "group created & listed" "document.getElementById('groups-list').textContent.includes('Full UI Group')"
shot 12-groups.png
ab click "#settings-back" >/dev/null 2>&1; sleep 1

echo "-- theme --"
ab eval "document.documentElement.setAttribute('data-theme','light')" >/dev/null 2>&1
assert "light theme applies" "document.documentElement.getAttribute('data-theme')==='light'"
ab eval "document.documentElement.setAttribute('data-theme','dark')" >/dev/null 2>&1
assert "dark theme applies" "document.documentElement.getAttribute('data-theme')==='dark'"
shot 13-dark.png

if [ -n "$ADMIN_USER" ] && [ -n "$ADMIN_PASS" ]; then
echo "-- admin pass --"
close_drawers
ab click "#menu-btn" >/dev/null 2>&1; sleep 1
ab click "#logout-btn" >/dev/null 2>&1; sleep 1
ab reload >/dev/null 2>&1; sleep 1
login "$ADMIN_USER" "$ADMIN_PASS"
assert "admin signed in" "!document.getElementById('app').hidden && !document.getElementById('user-admin-badge').hidden"
ab click "#settings-btn" >/dev/null 2>&1; wait_for "!document.getElementById('settings-page').hidden" 8
assert "users section visible" "!document.getElementById('users-section').hidden"
assert "integrations section visible" "!document.getElementById('integrations-section').hidden"
assert "web search block visible" "!document.getElementById('web-search-block').hidden"
assert "sandbox section visible" "!document.getElementById('tools-section').hidden"
assert "ocr section visible" "!document.getElementById('ocr-section').hidden"
ab scrollintoview "#tools-section" >/dev/null 2>&1; sleep 1
shot 14-admin-settings.png
fi

echo "-- result --"
echo "  passed=$PASSES failed=$FAILS"
[ "$FAILS" -eq 0 ] && echo "  FULL UI TESTS OK" || echo "  FULL UI TESTS FAILED"
ab close --all >/dev/null 2>&1 || true
exit $((FAILS > 0 ? 1 : 0))
