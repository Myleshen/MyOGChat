#!/usr/bin/env bash
# Focused end-to-end check that each tool RUNS and its RESULT reaches the model.
#
# For each tool it starts a fresh conversation, sends a tool-eliciting prompt, and
# prints the SSE tool events plus the model's final answer. A correct answer that
# contains the tool's data proves the round-trip (guards the class of bug where a
# Params field is dropped on the agent path).
#
# Usage: scripts/tool-check.sh <BASE_URL> <USER> <PASS>
# Env:   MODEL (default glm-5.3-flash), OUT_DIR (default /tmp/opencode)
set -uo pipefail

BASE_URL="${1:?usage: tool-check.sh <base_url> <user> <pass>}"
USER_NAME="${2:?user required}"
USER_PASS="${3:?pass required}"
MODEL="${MODEL:-glm-5.3-flash}"
OUT_DIR="${OUT_DIR:-/tmp/opencode}"
COOKIES="$(mktemp)"
mkdir -p "$OUT_DIR"

curl -s -c "$COOKIES" -o /dev/null -X POST "$BASE_URL/api/auth/login" \
  -d "{\"identifier\":\"$USER_NAME\",\"password\":\"$USER_PASS\"}"

run() { # name prompt
  local name="$1" prompt="$2" cid sse
  cid=$(curl -s -b "$COOKIES" -X POST "$BASE_URL/api/conversations" -d "{\"model\":\"$MODEL\"}" | jq -r .id)
  sse="$OUT_DIR/tool-$name.sse"
  curl -sN -b "$COOKIES" --max-time 150 -X POST "$BASE_URL/api/conversations/$cid/messages" \
    -d "{\"content\":$(jq -Rn --arg p "$prompt" '$p')}" > "$sse" 2>/dev/null
  echo "=================== $name ==================="
  echo "-- tool events --"
  awk '/^event: tool/{getline; sub(/^data: /,""); print "   " $0}' "$sse"
  echo "-- assistant answer --"
  awk '/^event: text/{f=1;next} /^event:/{f=0} f && /^data: /{sub(/^data: /,""); print}' "$sse" \
    | jq -r '.delta' 2>/dev/null | tr -d '\n' | sed 's/^/   /'
  echo
  echo "-- errors --"
  awk '/^event: error/{getline; sub(/^data: /,""); print "   " $0}' "$sse"
  echo
}

run web_search "Search the web for the official website of the Go programming language and give me its exact URL."
run fetch_url "Read https://example.com and tell me its exact page title."
run run_bash "Run this exact shell command in your sandbox and report the exact output: echo TOOLCHK-BASH; uname -m"
run run_python "Use Python to compute 1234*5678 and report the exact number."
run linkwarden "Search my Linkwarden for anything about the Go programming language."

rm -f "$COOKIES"
