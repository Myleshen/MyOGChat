#!/usr/bin/env bash
# Install a published MyOGChat release on the Raspberry Pi.
#
# Runs *on the Pi* (deploy/deploy-release.sh pipes it over ssh). Needs: bash,
# curl, coreutils (sha256sum), docker, systemctl --user. No Go toolchain, no
# repository checkout, no sudo.
#
# Usage: install-release.sh <tag>        e.g. install-release.sh v1.2.3
#
# Pulls the previewed sandbox images *by digest* (see images.txt) and the
# checksum-verified arm64 binary, then restarts the systemd user service.
set -euo pipefail

TAG="${1:?usage: install-release.sh <tag>   (e.g. v1.2.3)}"
REPO=Myleshen/MyOGChat
REL="https://github.com/$REPO/releases/download/$TAG"
BIN_DIR="$HOME/softwares/MyOGChat"

TMP="$(mktemp -d "${TMPDIR:-/tmp}/myogchat-release.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
cd "$TMP"

echo "==> downloading $TAG"
for f in myogchat-linux-arm64 SHA256SUMS images.txt; do
  curl -fL --retry 3 -sS -o "$f" "$REL/$f"
done

# Only the binary line is verified; an empty match fails the pipeline (pipefail).
echo "==> verifying checksum"
grep -E ' [* ]myogchat-linux-arm64$' SHA256SUMS | sha256sum -c -

echo "==> pulling sandbox images by digest"
while read -r ref; do
  [ -n "$ref" ] || continue
  case "$ref" in
    */myogchat-sandbox-python@sha256:*) local_name=myogchat-sandbox-python ;;
    */myogchat-sandbox-bash@sha256:*) local_name=myogchat-sandbox-bash ;;
    *) echo "error: unexpected image reference in images.txt: $ref" >&2; exit 1 ;;
  esac
  docker pull "$ref"
  prev=$(docker image inspect -f '{{.Id}}' "$local_name:latest" 2>/dev/null || true)
  # The app runs images by their local names (MYOGCHAT_SANDBOX_IMAGE_*).
  docker tag "$ref" "$local_name:latest"
  # Drop the digest reference and the superseded build, or every release leaves
  # another ~900 MB of sandbox images behind.
  docker rmi "$ref" >/dev/null 2>&1 || true
  [ -n "$prev" ] && docker rmi "$prev" >/dev/null 2>&1 || true
done < images.txt

echo "==> installing binary to $BIN_DIR/myogchat"
install -d "$BIN_DIR"
install -m 755 ./myogchat-linux-arm64 "$BIN_DIR/myogchat.new"
mv -f "$BIN_DIR/myogchat.new" "$BIN_DIR/myogchat"

echo "==> restarting myogchat"
systemctl --user restart myogchat

echo "==> waiting for http://127.0.0.1:8787/"
code=""
for _ in $(seq 1 30); do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 http://127.0.0.1:8787/ || true)
  [ "$code" = 200 ] && break
  sleep 1
done
if [ "$code" != 200 ]; then
  echo "error: service did not return HTTP 200 (last: ${code:-none})" >&2
  echo "check: journalctl --user -u myogchat -n 50 --no-pager" >&2
  exit 1
fi

echo "==> installed $TAG"
echo "binary sha256: $(sha256sum "$BIN_DIR/myogchat" | cut -d' ' -f1)"
cat images.txt
