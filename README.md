# MyOGChat

<https://github.com/Myleshen/MyOGChat>

A personal, self-hosted ChatGPT-style web app (installable PWA) backed by an
[OpenCode Go](https://opencode.ai/docs/go/) subscription. One static Go binary,
no runtime dependencies, SQLite storage, ~3 MB RAM — comfortable on a
Raspberry Pi 4B.

## Features

- Streaming chat with markdown + syntax highlighting and conversation history
- **Projects** — named containers with a system-instructions prompt and an
  optional default model; a chat is filed in a project or left unfiled
- **Two upstream dialects** — OpenAI-compatible `/chat/completions` (Kimi, GLM,
  DeepSeek, MiMo, …) and the Responses API `/responses` (GPT 6 Luna, Grok), with
  a reasoning-effort selector
- Collapsible **thinking** blocks (duration + reasoning-token count) and a
  thinking badge on models that surface reasoning
- Per-message **cost** and **tokens/sec**, plus a conversation running total
- **Regenerate** and copy tools; auto-generated chat titles
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

Users sign in with a username/password; there is no shared token
(`MYOGCHAT_AUTH_TOKEN` is obsolete).

## Build & run locally

Requires Go 1.27+ (pinned in `.mise.toml`; `mise exec -- go …` works).

```sh
OPENCODE_API_KEY=… go run .
# http://localhost:8787
```

## Deploy

The target host needs **no Go toolchain** — the binary is cross-compiled on the
workstation and installed as a systemd *user* service.

```sh
cp deploy/env.example ~/.config/myogchat/env   # on the target; then fill it in
chmod 600 ~/.config/myogchat/env
deploy/deploy.sh pi@raspberrypi.local        # cross-compile arm64 + install
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
- Muse Spark models are intentionally excluded (they train on your prompts).
