# MyOGChat

<https://github.com/Myleshen/MyOGChat>

A personal, self-hosted ChatGPT-style web app (installable PWA) backed by an
[OpenCode Go](https://opencode.ai/docs/go/) subscription. Single Go binary, no
runtime dependencies, SQLite storage, ~3 MB RAM — built to run comfortably on a
Raspberry Pi 4B.

## Features

- Streaming chat with markdown + syntax highlighting, conversation history
- **Projects** — named containers with a system-instructions prompt and an
  optional default model; chats live in a project or stay unfiled
- **Two upstream dialects**: OpenAI-compatible `/chat/completions` (Kimi, GLM,
  DeepSeek, MiMo, …) and the Responses API `/responses` (GPT 6 Luna, Grok) with
  a reasoning-effort selector
- Collapsible **thinking** blocks (duration + reasoning-token count) and a
  thinking badge on models that surface reasoning
- Per-message **cost** and **tokens/sec**, conversation running total
- **Regenerate** / copy tools, auto-generated chat titles
- **Light / dark / system** themes and a settings panel
- Installable **PWA** (mobile-first; add to home screen on Android/iOS)
- Configurable retention (default 30 days, hard max 60)
- Shared-token login; designed to sit behind a Cloudflare Tunnel

## Requirements

- An OpenCode Go API key (subscription in the [OpenCode console](https://opencode.ai/console))
- Go 1.24+ to build (this repo pins Go via `.mise.toml`; `mise exec -- go …` works)

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `OPENCODE_API_KEY` | — | **required**, your OpenCode Go key |
| `MYOGCHAT_AUTH_TOKEN` | — | **required**, shared login token (e.g. `openssl rand -base64 24`) |
| `MYOGCHAT_PORT` | `8787` | listen port |
| `MYOGCHAT_DB` | `~/.local/share/myogchat/chat.db` | SQLite path |
| `MYOGCHAT_RETENTION_DAYS` | `30` | clamped to `[1, 60]` |

## Run locally

```sh
OPENCODE_API_KEY=sk-… MYOGCHAT_AUTH_TOKEN=$(openssl rand -base64 24) go run .
# open http://localhost:8787
```

## Deploy to a Raspberry Pi (aarch64)

```sh
cp deploy/env.example ~/.config/myogchat/env   # on the Pi, then fill it in
chmod 600 ~/.config/myogchat/env
deploy/deploy.sh pi@raspberrypi.local        # builds arm64 + installs the user service
```

Then enable lingering once so the service survives logout/reboot:

```sh
ssh pi@raspberrypi.local sudo loginctl enable-linger pi
```

Logs: `journalctl --user -u myogchat -f`. Details in [`DESIGN.md`](DESIGN.md)
and the exact API in [`API_CONTRACT.md`](API_CONTRACT.md).

## Notes

- Requires a client-side `x-opencode-session` header (per conversation) and a
  non-generic User-Agent, per OpenCode Go's usage requirements — both are set
  by the backend.
- The Muse Spark models are intentionally excluded (they train on your prompts).
