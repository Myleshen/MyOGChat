#!/usr/bin/env bash
# Build the MyOGChat image-OCR container (feature 2.5).
#
# Run once on the Pi (and on any workstation you want to smoke-test with):
#     bash deploy/ocr-setup.sh
#
# The server then runs `tesseract` inside this image via `docker run` (see the
# MYOGCHAT_OCR_IMAGE / MYOGCHAT_DOCKER env vars). The Pi needs Docker installed
# and the service user in the `docker` group — the host tesseract is never
# required.
set -euo pipefail

IMAGE="${MYOGCHAT_OCR_IMAGE:-myogchat-ocr}"
DOCKER="${MYOGCHAT_DOCKER:-docker}"
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ocr"

if ! command -v "$DOCKER" >/dev/null 2>&1; then
  echo "docker not found ($DOCKER) — install Docker or set MYOGCHAT_DOCKER" >&2
  exit 1
fi

echo "== building $IMAGE from $DIR =="
"$DOCKER" build -t "$IMAGE" "$DIR"

echo "== verifying tesseract =="
"$DOCKER" run --rm "$IMAGE" tesseract --version

echo "OK — built $IMAGE. Restart myogchat if it was already running."
