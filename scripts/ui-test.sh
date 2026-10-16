#!/usr/bin/env bash
# MyOGChat UI/UX battery (agent-browser).
#
# Two passes over a mobile viewport:
#   Phase A — a normal test user: shell, keyboard, sidebar, projects, attachments,
#             settings page (#/settings: account + key + groups), themes, send.
#   Phase B — an admin: admin badge + the admin-only Users panel + Integrations.
#
# Run against an isolated instance with scripts/ui-test-run.sh (dedicated test DB).
#
# Usage: scripts/ui-test.sh [BASE_URL] [TEST_USER] [TEST_PASS]
# Env:   ADMIN_USER, ADMIN_PASS, SHOTS, DEVICE
set -uo pipefail

BASE_URL="${1:-http://127.0.0.1:8791}"
TEST_USER="${2:-uitest}"
TEST_PASS="${3:-uitestpass123}"
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASS="${ADMIN_PASS:-adminpass1}"
SHOTS="${SHOTS:-/tmp/opencode/ui}"
DEVICE="${DEVICE:-Pixel 7}"
UPLOAD_FILE="${UPLOAD_FILE:-/tmp/opencode/uitest-file.txt}"

mkdir -p "$SHOTS"
printf 'hello from the ui test\nline two\n' > "$UPLOAD_FILE"
export AGENT_BROWSER_SESSION="${AGENT_BROWSER_SESSION:-myogchat-uitest}"
FAILS=0; PASSES=0

ab() { agent-browser "$@" 2>&1; }
assert() {
  local name="$1" expr="$2" out
  out=$(ab eval "String(($expr))" | tail -1 | tr -d '\r' | sed -E 's/^[[:space:]]*"//; s/"[[:space:]]*$//')
  if [ "$out" = "true" ]; then echo "  PASS  $name"; PASSES=$((PASSES+1));
  else echo "  FAIL  $name  (got: ${out:-<empty>})"; FAILS=$((FAILS+1)); fi
}
shot() { ab screenshot "$SHOTS/$1" >/dev/null 2>&1; }
# wait_for <js-expr> [timeout-seconds] — poll until the expression is true.
wait_for() {
  local expr="$1" t="${2:-8}" i=0 out
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
  wait_for "!document.getElementById('app').hidden" 10
}
# Best-effort cleanup helpers (fire-and-forget DELETE by name).
cleanup() { ab eval "(function(){fetch('$1').then(function(r){return r.json();}).then(function(xs){var x=xs.filter(function(o){return o.name==='$2';})[0];if(x)fetch('$1/'+x.id,{method:'DELETE'});});return 'ok';})()" >/dev/null 2>&1; }

echo "== MyOGChat UI test =="; echo "base=$BASE_URL device=$DEVICE user=$TEST_USER"

ab close --all >/dev/null 2>&1 || true
ab open "$BASE_URL" >/dev/null 2>&1
ab set device "$DEVICE" >/dev/null 2>&1
ab reload >/dev/null 2>&1; sleep 1

echo "-- boot --"
assert "login screen shown" "!document.getElementById('login-view').hidden && !document.getElementById('login-form').hidden"
shot 01-boot.png
login "$TEST_USER" "$TEST_PASS"

echo "-- app shell --"
assert "app shell visible" "!document.getElementById('app').hidden"
assert "no horizontal overflow" "document.documentElement.scrollWidth <= window.innerWidth + 1"
assert "non-admin: no admin badge" "document.getElementById('user-admin-badge').hidden"
shot 02-app.png

echo "-- mobile keyboard --"
ab set viewport 412 915 >/dev/null 2>&1; ab click "textarea#input" >/dev/null 2>&1 || true
ab set viewport 412 470 >/dev/null 2>&1; sleep 1
assert "composer pinned with keyboard" "document.querySelector('.composer').getBoundingClientRect().bottom <= window.innerHeight + 1"
assert "page did not scroll" "window.scrollY === 0"
shot 03-keyboard.png
ab set viewport 412 915 >/dev/null 2>&1; sleep 1

echo "-- sidebar --"
ab click "#menu-btn" >/dev/null 2>&1; sleep 1
assert "sidebar opens" "document.getElementById('sidebar').classList.contains('open')"
shot 04-sidebar.png
ab click "#backdrop" >/dev/null 2>&1 || ab press Escape >/dev/null 2>&1; sleep 1
assert "sidebar closes" "!document.getElementById('sidebar').classList.contains('open')"

echo "-- projects --"
ab click "#menu-btn" >/dev/null 2>&1; sleep 1
ab click "#new-project-btn" >/dev/null 2>&1; sleep 1
assert "project editor opens" "!document.getElementById('project-modal').hidden"
ab fill "#project-name" "UI Test Project" >/dev/null 2>&1
ab click "#project-form button[type=submit]" >/dev/null 2>&1; sleep 1
assert "project created & listed" "document.getElementById('projects-list').textContent.includes('UI Test Project')"
shot 05-project.png
cleanup "/api/projects" "UI Test Project"
ab press Escape >/dev/null 2>&1; sleep 1
ab click "#backdrop" >/dev/null 2>&1 || true; sleep 1

echo "-- attachments (2.3) --"
ab upload "#file-input" "$UPLOAD_FILE" >/dev/null 2>&1 || true
sleep 2
assert "attachment chip shown" "document.getElementById('attachments-bar').textContent.length > 0 && !document.getElementById('attachments-bar').hidden"
shot 06-attachment.png

echo "-- settings page (#/settings) --"
ab press Escape >/dev/null 2>&1; sleep 1
ab click "#settings-btn" >/dev/null 2>&1; sleep 1
assert "settings page opens" "!document.getElementById('settings-page').hidden && location.hash === '#/settings'"
assert "settings fits viewport" "(function(){var r=document.getElementById('settings-page').getBoundingClientRect();return r.top >= -1 && r.bottom <= window.innerHeight + 1;})()"
assert "settings: no horizontal overflow" "document.getElementById('settings-page').scrollWidth <= window.innerWidth + 1 && document.documentElement.scrollWidth <= window.innerWidth + 1"
assert "account section present" "document.getElementById('account-username').textContent.length > 0"
assert "key section present" "!document.getElementById('key-section').hidden"
assert "groups section present" "!!document.getElementById('groups-section')"
assert "non-admin: users section hidden" "document.getElementById('users-section').hidden"
assert "about section present" "document.getElementById('app-version').textContent.length > 0"
assert "web search section present (all users)" "!!document.getElementById('my-search-section')"
assert "calendar section present (all users)" "!!document.getElementById('calendar-section')"
assert "email section present (all users)" "!!document.getElementById('email-section')"
assert "settings category nav present" "!!document.querySelector('.settings-nav-item[data-cat=account]')"
ab click ".settings-nav-item[data-cat=ai]" >/dev/null 2>&1; sleep 1
assert "category switch shows key + groups" "!document.getElementById('key-section').classList.contains('cat-hidden') && !document.getElementById('groups-section').classList.contains('cat-hidden')"
shot 07a-settings-top.png
ab fill "#group-name" "UI Test Group" >/dev/null 2>&1
ab click "#group-form button[type=submit]" >/dev/null 2>&1; sleep 1
assert "group created & listed" "document.getElementById('groups-list').textContent.includes('UI Test Group')"
ab scrollintoview "#groups-section" >/dev/null 2>&1; sleep 1
shot 07-settings.png
cleanup "/api/groups" "UI Test Group"
ab eval "document.querySelector('.settings-nav-item[data-cat=integrations]').click();'ok'" >/dev/null 2>&1; sleep 1
assert "integrations category shows web search" "!document.getElementById('my-search-section').classList.contains('cat-hidden')"
# In-page back affordance returns to the previous view.
ab click "#settings-back" >/dev/null 2>&1; sleep 1
assert "back button returns to chat" "document.getElementById('settings-page').hidden && !document.getElementById('app').hidden"
# Browser/Android back button works too (hash routing).
ab click "#settings-btn" >/dev/null 2>&1; sleep 1
ab eval "history.back()" >/dev/null 2>&1; sleep 1
assert "browser back closes settings" "document.getElementById('settings-page').hidden"

echo "-- theme --"
ab eval "document.documentElement.setAttribute('data-theme','light')" >/dev/null 2>&1
assert "light theme applies" "document.documentElement.getAttribute('data-theme')==='light'"
shot 08-light.png
ab eval "document.documentElement.setAttribute('data-theme','dark')" >/dev/null 2>&1
assert "dark theme applies" "document.documentElement.getAttribute('data-theme')==='dark'"

echo "-- send a message (with the attached file) --"
ab fill "#input" "Summarise the attached file in one line." >/dev/null 2>&1
ab click "#send-btn" >/dev/null 2>&1 || true
sleep 12
assert "assistant message rendered" "document.querySelectorAll('.msg').length >= 2"
assert "user message shows attachment" "document.querySelector('.messages').textContent.includes('file')"
assert "composer visible after send" "document.querySelector('.composer').getBoundingClientRect().bottom <= window.innerHeight + 1"
shot 09-sent.png

echo "-- phase 5: search + pins --"
assert "chat search input present" "!!document.getElementById('search-input')"
assert "conversation row has pin toggle" "!!document.querySelector('.convo-item .convo-pin')"
ab eval "(function(){var b=document.querySelector('.convo-item .convo-pin');if(b){b.click();return 'ok';}return 'none';})()" >/dev/null 2>&1
sleep 1
assert "pin toggles pressed state" "!!document.querySelector('.convo-item .convo-pin[aria-pressed=\"true\"]')"
shot 10-phase5-pin.png

echo "-- phase 5: export menu (a11y) --"
ab click "#export-btn" >/dev/null 2>&1; sleep 1
assert "export menu opens" "!!document.querySelector('.menu') && document.querySelectorAll('.menu-item').length >= 2"
assert "export anchor aria-expanded true" "document.getElementById('export-btn').getAttribute('aria-expanded')==='true'"
assert "export menu focuses first item" "document.activeElement && document.activeElement.classList.contains('menu-item')"
ab press ArrowDown >/dev/null 2>&1; sleep 0.3
assert "arrow key moves menu focus" "document.querySelectorAll('.menu-item')[1] === document.activeElement"
shot 10a-phase5-export.png
ab press Escape >/dev/null 2>&1; sleep 1
assert "export menu closes on Esc" "!document.querySelector('.menu') && document.getElementById('export-btn').getAttribute('aria-expanded')==='false'"

echo "-- phase 5: edit & resend --"
# Editing is refused while a reply is generating (5.7); wait for the previous
# send to settle so this section is not racy.
wait_for "document.getElementById('stop-btn').hidden" 120 || true
sleep 1
ab eval "(function(){var b=null;document.querySelectorAll('.msg-actions .msg-action').forEach(function(x){if(x.textContent.trim()==='Edit')b=x;});if(b){b.click();return 'ok';}return 'none';})()" >/dev/null 2>&1
sleep 1
assert "inline editor opens" "!!document.querySelector('.msg-edit-input')"
# Focus the Save button, then Esc: cancel must work even when the textarea is
# not focused (the inline-edit hardening).
ab eval "(function(){var s=document.querySelector('.msg-edit-save');if(s){s.focus();return 'ok';}return 'none';})()" >/dev/null 2>&1
ab press Escape >/dev/null 2>&1; sleep 1
assert "Esc cancels editor (focus on Save)" "!document.querySelector('.msg-edit-input')"
ab eval "(function(){var b=null;document.querySelectorAll('.msg-actions .msg-action').forEach(function(x){if(x.textContent.trim()==='Edit')b=x;});if(b){b.click();return 'ok';}return 'none';})()" >/dev/null 2>&1
sleep 1
ab eval "(function(){var t=document.querySelector('.msg-edit-input');if(t){t.value='Reply with exactly: EDITED-OK';return 'ok';}return 'none';})()" >/dev/null 2>&1
ab click ".msg-edit-save" >/dev/null 2>&1 || true
wait_for "!document.getElementById('stop-btn').hidden" 8 || true
wait_for "document.getElementById('stop-btn').hidden" 120 || true
sleep 1
assert "edited message resent" "document.querySelector('.messages').textContent.includes('EDITED-OK')"
assert "reply after edit" "document.querySelectorAll('.msg').length >= 2"
shot 10b-phase5-edit.png

echo "-- phase 5: paste image chip --"
ab eval "(function(){try{var b64='iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==';var bin=atob(b64);var arr=new Uint8Array(bin.length);for(var i=0;i<bin.length;i++)arr[i]=bin.charCodeAt(i);var f=new File([arr],'pasted.png',{type:'image/png'});var dt=new DataTransfer();dt.items.add(f);document.getElementById('input').dispatchEvent(new ClipboardEvent('paste',{clipboardData:dt,bubbles:true,cancelable:true}));return 'ok';}catch(e){return 'err:'+e.message;}})()" >/dev/null 2>&1
sleep 2
assert "pasted image becomes a chip" "document.getElementById('attachments-bar').textContent.includes('pasted.png')"
shot 10c-phase5-paste.png
ab eval "(function(){document.querySelectorAll('.attachment-remove').forEach(function(b){b.click();});return 'ok';})()" >/dev/null 2>&1
sleep 1

echo "-- phase 5: overflow with a chat open --"
assert "no horizontal overflow with chat open" "document.documentElement.scrollWidth <= window.innerWidth + 1 && document.getElementById('messages').scrollWidth <= document.getElementById('messages').clientWidth + 1"

if [ "${SKIP_ADMIN:-0}" != "1" ]; then
echo "-- admin pass --"
ab click "#menu-btn" >/dev/null 2>&1; sleep 1
ab click "#logout-btn" >/dev/null 2>&1; sleep 1
ab reload >/dev/null 2>&1; sleep 1   # clean login screen (dismiss any leftover drawer/backdrop)
assert "logged out" "!document.getElementById('login-view').hidden && !document.getElementById('login-form').hidden"
login "$ADMIN_USER" "$ADMIN_PASS"
assert "admin signed in" "!document.getElementById('app').hidden"
assert "admin badge shown" "!document.getElementById('user-admin-badge').hidden"
ab press Escape >/dev/null 2>&1; sleep 1
ab click "#settings-btn" >/dev/null 2>&1; sleep 1
assert "settings page opens (admin)" "!document.getElementById('settings-page').hidden"
assert "users section visible for admin" "!document.getElementById('users-section').hidden"
assert "share-server-key toggle present" "!document.getElementById('share-key-row').hidden"
assert "integrations section visible for admin" "!document.getElementById('integrations-section').hidden"
assert "web search block visible for admin" "!!document.getElementById('web-search-block') && !document.getElementById('web-search-block').hidden"
assert "code sandbox section visible for admin" "!document.getElementById('tools-section').hidden"
assert "ocr section visible for admin" "!document.getElementById('ocr-section').hidden"
assert "usage section present" "!!document.getElementById('usage-section')"
assert "voice section visible for admin" "!document.getElementById('voice-section').hidden"
assert "backup row visible for admin" "!document.getElementById('backup-row').hidden && !!document.getElementById('download-backup')"
ab eval "document.querySelector('.settings-nav-item[data-cat=users]').click();'ok'" >/dev/null 2>&1; sleep 1
ab scrollintoview "#users-section" >/dev/null 2>&1; sleep 1
shot 10-admin.png
ab click "#settings-back" >/dev/null 2>&1; sleep 1
fi

echo "-- result --"
echo "  passed=$PASSES failed=$FAILS"
[ "$FAILS" -eq 0 ] && echo "  UI TESTS OK" || echo "  UI TESTS FAILED"
ab close --all >/dev/null 2>&1 || true
exit $((FAILS > 0 ? 1 : 0))
