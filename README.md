# MyOGChat

<https://github.com/Myleshen/MyOGChat>

[![release](https://github.com/Myleshen/MyOGChat/actions/workflows/release.yml/badge.svg)](https://github.com/Myleshen/MyOGChat/actions/workflows/release.yml)

A personal, self-hosted ChatGPT-style web app (installable PWA) backed by an
[OpenCode Go](https://opencode.ai/docs/go/) subscription. One static Go binary
with no runtime dependencies, SQLite storage, ~24 MB RSS at idle — comfortable on
a Raspberry Pi 4B. Optional features need local services (Docker for the
sandbox/OCR, an HTTP STT/TTS service, ComfyUI for images) — see the per-feature
docs under [`docs/`](docs/).

## Features

- Streaming chat with markdown + syntax highlighting and conversation history
- **Projects** — named containers with a system-instructions prompt and an
  optional default model; a chat is filed in a project or left unfiled
- **Project memory** — durable facts learned from project chats, injected into
  every chat of the project ([`docs/6.2-project-memory.md`](docs/6.2-project-memory.md))
- **Agent mode** — opt-in per conversation for long jobs: a persistent 5 GiB
  sandbox workspace, a 60-minute / 60-step budget and a per-user queue
  ([`docs/5.12-agent-mode.md`](docs/5.12-agent-mode.md))
- **Two upstream dialects** — OpenAI-compatible `/chat/completions` (Hy, LongCat,
  Kimi, GLM, DeepSeek, MiMo, MiniMax, Step, …) and the Responses API `/responses`
  (GPT 6/5.6 Luna, Grok), with a reasoning-effort selector. The
  Anthropic (`/messages`) and Google (`generateContent`) models in the live
  catalog are not offered — those dialects are not implemented.
- Collapsible **thinking** blocks (duration + reasoning-token count) and a
  thinking badge on models that surface reasoning
- Per-message **cost** and **tokens/sec**, plus a conversation running total
- **Regenerate** and copy tools; auto-generated chat titles
- **Tools** — the model can call `web_search` (Tavily → Brave → Serper →
  DuckDuckGo fallback, keyless by default), `fetch_url`, Linkwarden, OpenStreetMap
  `maps_*`, and `run_bash` / `run_python` in a hardened Docker sandbox
- **Email (read-only)** — IMAP search/read across one or more mailboxes, with no
  send path ([`docs/3.2-imap.md`](docs/3.2-imap.md))
- **Calendar** — CalDAV read/write plus read-only ICS, exposed as
  `calendar_list`/`_create`/`_update`/`_delete`
  ([`docs/3.3-calendar.md`](docs/3.3-calendar.md))
- **Image generation** — `generate_image` tool and a prompt-enhance step against
  a self-hosted ComfyUI backend ([`docs/3.1-imagegen.md`](docs/3.1-imagegen.md))
- **Speech** — microphone transcription (whisper.cpp) and reply read-aloud
  (Piper) through persistent HTTP services
  ([`docs/3.5-3.6-speech.md`](docs/3.5-3.6-speech.md))
- **Light / dark / system** themes and a categorized **settings page** (Account,
  Appearance, AI & access, Integrations, Tools, Voice, Usage, Users, About)
- Installable **PWA** (mobile-first; add to home screen on Android/iOS)
- Configurable retention (default 30 days, max 60) and multi-user accounts
  (username/password; first run creates the admin), designed to sit behind a
  Cloudflare Tunnel

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `OPENCODE_API_KEY` | — | optional; shared OpenCode Go key (users can also BYOK in Settings) |
| `MYOGCHAT_PORT` | `8787` | listen port |
| `MYOGCHAT_DB` | `~/.local/share/myogchat/chat.db` | SQLite path |
| `MYOGCHAT_RETENTION_DAYS` | `30` | clamped to `[1, 60]` |
| `MYOGCHAT_AGENT_QUOTA_BYTES` | `5368709120` | per-user agent workspace quota (5 GiB); `0` disables |

Users sign in with a username/password; there is no shared token
(`MYOGCHAT_AUTH_TOKEN` is obsolete).

## Build & run locally

Requires Go 1.27+ (pinned in `.mise.toml`; `mise exec -- go …` works).

```sh
OPENCODE_API_KEY=… go run .
# http://localhost:8787
```

## Deploy

The target host needs **no Go toolchain**. Releases are cut by pushing a
`vMAJOR.MINOR.PATCH` tag; GitHub Actions runs the tests, builds the static
`linux/arm64` binary, builds and pushes both sandbox images to GHCR, and
publishes them on a GitHub Release with `SHA256SUMS` and `images.txt`. The Pi
installs nothing but those artifacts, and pulls the sandbox images **by digest**
(never by mutable tag) so a deploy is reproducible.

```sh
# 1. on the workstation: tag a commit on main and push it
git tag v1.2.3 && git push origin v1.2.3

# 2. install that release on the target (no checkout, no Go on the Pi)
deploy/deploy-release.sh pi v1.2.3
```

Prerequisites on the target (once):

```sh
cp deploy/env.example ~/.config/myogchat/env   # then fill it in
chmod 600 ~/.config/myogchat/env
# GHCR images are private by default: log docker in once (PAT with read:packages)
ssh pi 'docker login ghcr.io -u <github-user>'
```

### Offline / fallback build

When GitHub is unreachable or you need to test an uncommitted tree, build
locally and scp the binary. Same service, no release artifacts:

```sh
deploy/deploy.sh pi@raspberrypi.local   # cross-compile arm64 + install
```

Enable lingering once (needs sudo **on the target**) so the service starts at
boot instead of only while you are logged in:

```sh
ssh pi@raspberrypi.local sudo loginctl enable-linger pi
```

Logs: `journalctl --user -u myogchat -f`.

## Docs

- [`DESIGN.md`](DESIGN.md) — architecture and design
- [`API_CONTRACT.md`](API_CONTRACT.md) — exact REST/SSE contract
- [`AGENTS.md`](AGENTS.md) — notes for AI coding agents working on this repo

## Notes

- Requests must carry a per-conversation `x-opencode-session` and a non-generic
  User-Agent, per OpenCode Go's usage requirements — the backend sets both.
- Upstream base is `https://opencode.ai/inference/go/openai/v1`. Each user can
  store a personal key in Settings → API key (it overrides the server key);
  keys from the old OpenCode console are no longer accepted.
- Auto-titles use a cheap non-reasoning Go model (`hy3`) on your own key — a
  fraction of a cent per chat, and they fall back to the prompt text if the
  call fails.
- Prompt-training models are intentionally excluded (`muse-spark-1.2-contributor`,
  `muse-spark-1.3-contributor`, `iceman-contributor-free` — `go.training: true`).

## License

[MIT](LICENSE) © 2026 Myleshen Praburam.
