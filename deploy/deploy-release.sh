#!/usr/bin/env bash
# Install a published MyOGChat release on the Pi. The Pi needs no checkout:
# deploy/install-release.sh is piped over ssh (all logic lives there).
#
# Usage: deploy/deploy-release.sh <ssh-target> <tag>
#   deploy/deploy-release.sh pi v1.2.3
set -euo pipefail

HOST="${1:?usage: deploy/deploy-release.sh <ssh-target> <tag>   (e.g. pi v1.2.3)}"
TAG="${2:?usage: deploy/deploy-release.sh <ssh-target> <tag>   (e.g. pi v1.2.3)}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> installing $TAG on $HOST"
ssh "$HOST" bash -s -- "$TAG" < "$ROOT/deploy/install-release.sh"
