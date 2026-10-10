#!/usr/bin/env bash
# Provision the MyOGChat speech services (features 3.5 STT / 3.6 TTS).
#
#   bash deploy/speech-setup.sh
#
# Runs two *persistent* HTTP services (models stay warm) on 127.0.0.1 so the Go
# service — the only caller — can reach them over the loopback interface. They
# are never exposed publicly; the Cloudflare Tunnel only fronts MyOGChat.
#
#   * whisper.cpp `whisper-server` (STT) on http://127.0.0.1:8080
#       OpenAI-compatible POST /v1/audio/transcriptions
#   * Piper `piper.http_server` (TTS) on http://127.0.0.1:5000
#
# The owner can instead point MYOGCHAT_STT_URL / MYOGCHAT_TTS_URL at a Proxmox
# GPU shim (faster-whisper + Kokoro behind the same OpenAI-compatible shapes);
# this script is only the Pi-local default.
#
# Everything is overridable by env: MYOGCHAT_DOCKER, MYOGCHAT_STT_MODEL,
# MYOGCHAT_TTS_VOICE, MYOGCHAT_STT_PORT, MYOGCHAT_TTS_PORT, MYOGCHAT_SPEECH_DATA.
set -euo pipefail

DOCKER="${MYOGCHAT_DOCKER:-docker}"
WHISPER_IMAGE="${MYOGCHAT_WHISPER_IMAGE:-ghcr.io/ggml-org/whisper.cpp:main}"
PIPER_IMAGE="${MYOGCHAT_PIPER_IMAGE:-myogchat-piper}"
STT_MODEL="${MYOGCHAT_STT_MODEL:-base.en}"
TTS_VOICE="${MYOGCHAT_TTS_VOICE:-en_US-lessac-medium}"
STT_PORT="${MYOGCHAT_STT_PORT:-8080}"
TTS_PORT="${MYOGCHAT_TTS_PORT:-5000}"
DATA="${MYOGCHAT_SPEECH_DATA:-$HOME/.local/share/myogchat/speech}"
PIPER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/speech/piper"

if ! command -v "$DOCKER" >/dev/null 2>&1; then
  echo "docker not found ($DOCKER) — install Docker or set MYOGCHAT_DOCKER" >&2
  exit 1
fi
mkdir -p "$DATA/whisper" "$DATA/voices"

echo "== [1/4] whisper.cpp model ($STT_MODEL) =="
MODEL_FILE="$DATA/whisper/ggml-${STT_MODEL}.bin"
if [ -f "$MODEL_FILE" ]; then
  echo "   already present: $MODEL_FILE"
else
  echo "   downloading to $MODEL_FILE (this can take a while)"
  curl -fL --retry 3 -o "$MODEL_FILE.part" \
    "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-${STT_MODEL}.bin"
  mv "$MODEL_FILE.part" "$MODEL_FILE"
fi

echo "== [2/4] Piper voice ($TTS_VOICE) =="
VOICE_BASE="https://huggingface.co/rhasspy/piper-voices/resolve/main"
# Voice ids look like en_US-lessac-medium → en / en_US / lessac / medium.
LANG="${TTS_VOICE%%_*}"
LOCALE="${TTS_VOICE%%-*}"
REST="${TTS_VOICE#*-}"
VOICE_NAME="${REST%-*}"
QUALITY="${REST##*-}"
VOICE_DIR="$DATA/voices"
if [ -f "$VOICE_DIR/${TTS_VOICE}.onnx" ]; then
  echo "   already present: $VOICE_DIR/${TTS_VOICE}.onnx"
else
  echo "   downloading $TTS_VOICE"
  for ext in onnx onnx.json; do
    curl -fL --retry 3 -o "$VOICE_DIR/${TTS_VOICE}.${ext}.part" \
      "$VOICE_BASE/${LANG}/${LOCALE}/${VOICE_NAME}/${QUALITY}/${TTS_VOICE}.${ext}" || true
    [ -f "$VOICE_DIR/${TTS_VOICE}.${ext}.part" ] && mv "$VOICE_DIR/${TTS_VOICE}.${ext}.part" "$VOICE_DIR/${TTS_VOICE}.${ext}"
  done
  if [ ! -f "$VOICE_DIR/${TTS_VOICE}.onnx" ]; then
    echo "   !! could not fetch the voice automatically." >&2
    echo "      Download ${TTS_VOICE}.onnx and ${TTS_VOICE}.onnx.json into $VOICE_DIR" >&2
    echo "      from https://huggingface.co/rhasspy/piper-voices then re-run." >&2
  fi
fi

echo "== [3/4] STT: whisper-server on 127.0.0.1:$STT_PORT =="
"$DOCKER" rm -f myogchat-whisper >/dev/null 2>&1 || true
"$DOCKER" run -d --restart unless-stopped --name myogchat-whisper \
  -p "127.0.0.1:${STT_PORT}:8080" \
  -v "$DATA/whisper:/models:ro" \
  "$WHISPER_IMAGE" \
  --host 0.0.0.0 --port 8080 -m "/models/ggml-${STT_MODEL}.bin"

echo "== [4/4] TTS: Piper on 127.0.0.1:$TTS_PORT =="
"$DOCKER" build -t "$PIPER_IMAGE" "$PIPER_DIR"
"$DOCKER" rm -f myogchat-piper >/dev/null 2>&1 || true
"$DOCKER" run -d --restart unless-stopped --name myogchat-piper \
  -p "127.0.0.1:${TTS_PORT}:5000" \
  -v "$VOICE_DIR:/voices:ro" \
  "$PIPER_IMAGE" --host 0.0.0.0 --port 5000 --model "/voices/${TTS_VOICE}.onnx"

cat <<EOF
OK — speech services started.

  STT: http://127.0.0.1:${STT_PORT}   (whisper.cpp, model ${STT_MODEL})
  TTS: http://127.0.0.1:${TTS_PORT}   (Piper, voice ${TTS_VOICE})

Set these in ~/.config/myogchat/env (defaults already match) and restart myogchat:
  MYOGCHAT_STT_URL=http://127.0.0.1:${STT_PORT}
  MYOGCHAT_TTS_URL=http://127.0.0.1:${TTS_PORT}
  MYOGCHAT_TTS_VOICE=${TTS_VOICE}

Note: the whisper.cpp image's server entrypoint/binary may differ by tag; if the
container exits, check \`docker logs myogchat-whisper\` and adjust the command.
EOF
