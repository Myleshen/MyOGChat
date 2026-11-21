#!/usr/bin/env bash
# Replace docker's plaintext ~/.docker/config.json credentials with an
# encrypted password-store backend (docker-credential-pass).
# One-time, idempotent, needs sudo only for apt.
set -euo pipefail

if ! command -v docker-credential-pass >/dev/null; then
  sudo apt-get update -qq
  sudo apt-get install -y golang-docker-credential-helpers pass
fi

if ! gpg --list-secret-keys --with-colons 2>/dev/null | grep -q '^fpr:'; then
  gpg --batch --passphrase '' --quick-gen-key 'Pi Docker <docker@rpi>' default default never
fi
FPR=$(gpg --list-secret-keys --with-colons | awk -F: '/^fpr:/{print $10; exit}')
[ -n "$FPR" ] || { echo "no gpg key" >&2; exit 1; }
pass init "$FPR" >/dev/null

python3 - <<'PY'
import base64, json, os, subprocess
p = os.path.expanduser("~/.docker/config.json")
d = json.load(open(p))
entry = d.get("auths", {}).get("ghcr.io")
if entry and entry.get("auth"):
    user, token = base64.b64decode(entry["auth"]).decode().split(":", 1)
    payload = json.dumps({"ServerURL": "ghcr.io", "Username": user, "Secret": token})
    subprocess.run(["pass", "insert", "-efm", "docker-credential-helpers/ghcr.io"],
                   input=payload.encode(), check=True)
    del d["auths"]["ghcr.io"]
    print("moved ghcr.io token into pass")
else:
    print("no plaintext ghcr.io token found")
d["credsStore"] = "pass"
os.makedirs(os.path.dirname(p), exist_ok=True)
with open(p, "w") as f:
    json.dump(d, f, indent=2)
os.chmod(p, 0o600)
PY

echo "--- verifying"
echo ghcr.io | docker-credential-pass get >/dev/null && echo "credential helper: OK"
python3 -c "import json,os;d=json.load(open(os.path.expanduser('~/.docker/config.json')));print('credsStore:',d.get('credsStore'));print('plaintext auths remaining:',list(d.get('auths',{})))"
