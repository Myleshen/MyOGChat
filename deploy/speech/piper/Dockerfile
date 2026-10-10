# Minimal Piper TTS image for MyOGChat (feature 3.6).
#
# Built by deploy/speech-setup.sh and tagged `myogchat-piper` by default. It runs
# the Piper HTTP server, which the Go service calls over HTTP (MYOGCHAT_TTS_URL).
#
# Piper is GPL-3.0 (it embeds espeak-ng). For a self-hosted, non-distributed
# personal app that is fine; keeping it in a separate container also keeps it at
# arm's length from the MyOGChat binary's license scope. The voice files carry
# their own licenses — the default `en_US-lessac-medium` is CC-BY.
FROM python:3.12-slim

RUN pip install --no-cache-dir "piper-tts[http]"

# Voices are bind-mounted read-only at /voices by the setup script.
EXPOSE 5000
ENTRYPOINT ["python", "-m", "piper.http_server"]
CMD ["--host", "0.0.0.0", "--port", "5000", "--model", "/voices/en_US-lessac-medium.onnx"]
