# MyOGChat — Design

Personal ChatGPT-style web app (PWA) backed by an **OpenCode Go** subscription.
Single-user, self-hosted on a Raspberry Pi 4B (Ubuntu, aarch64), reachable via LAN
and Cloudflare Tunnel.

## Goals

- Chat UI with streaming, markdown/code highlighting, conversation history
- Uses the owner's OpenCode Go API key for all LLM inference
- Minimal RAM/CPU footprint (target: single static Go binary, <50 MB RSS)
- SQLite storage with configurable retention (default 30 days, hard max 60)
- Installable PWA (mobile-first; primary usage is Android phone/tablet)

## Non-goals (MVP)

- Multi-user accounts, file uploads, RAG, tools/function-calling, voice
- React Native / native apps (PWA covers mobile)
- Anthropic `/messages` dialect models (Claude Haiku, MiniMax, Qwen) — excluded

## Architecture

```
browser (PWA) ──▶ Go binary :8787 ──▶ https://opencode.ai/zen/go/v1
                   │  embedded SPA      ├─ /chat/completions  (chat dialect)
                   │  SQLite (WAL)      └─ /responses         (responses dialect)
                   └─ hourly retention janitor
```

- **Backend**: Go, stdlib `net/http` only. SQLite via `modernc.org/sqlite`
  (pure Go, no cgo → trivial `GOARCH=arm64` cross-compile). Frontend embedded
  via `embed.FS`. No web framework, no external router.
- **Frontend**: vanilla JS SPA in `static/`, no build step. marked.js +
  highlight.js vendored in `static/vendor/`. Mobile-first CSS.
- **Upstream requirements** (OpenCode Go ToS): every request sends
  `x-opencode-session: <conversation UUID>` and `User-Agent: myogchat/1.0`.

## Configuration (env)

| Var | Default | Notes |
|---|---|---|
| `OPENCODE_API_KEY` | — | required, OpenCode Go key |
| `MYOGCHAT_PORT` | `8787` | listen port |
| `MYOGCHAT_DB` | `~/.local/share/myogchat/chat.db` | SQLite path |
| `MYOGCHAT_RETENTION_DAYS` | `30` | clamped to [1, 60] |

Authentication is multi-user (see API_CONTRACT §9): the first run shows a
*Create admin account* screen; there is no shared-token env var. Sessions are
signed with a `session_secret` generated into the database on first run.

## Retention

Hourly janitor deletes conversations whose `updated_at` (last activity) is older
than the retention window. Messages cascade-delete.

## Deployment

Cross-compile on dev machine: `GOOS=linux GOARCH=arm64 go build`.
Deploy to Pi via ssh: binary → `~/.local/bin/myogchat`, systemd unit
`deploy/myogchat.service`, secrets in `~/.config/myogchat/env` (chmod 600).
Cloudflare Tunnel public hostname (added manually by owner in CF dashboard)
points to `http://raspberrypi.local:8787`.

## v2 additions

- **Projects** (`projects` table): named containers with a system `instructions`
  prompt and optional `default_model`. Chats belong to 0 or 1 project; the
  instructions are injected into every call (system message for chat models,
  `instructions` field for responses models). Deleting a project unfiles its
  chats. See API_CONTRACT §8.
- **Thinking**: collapsible reasoning block with duration + reasoning-token
  count; `thinking` badge on models that surface reasoning.
- **Token speed**: tokens/sec per assistant message (`duration_ms` is measured
  from first token to completion).
- **Regenerate**: replace the trailing assistant reply.
- **Settings** (browser-local): theme (system/light/dark), thinking display,
  token-speed display, Enter-to-send, sidebar collapse.

## Verified upstream behavior (probed 2026-10-08)

- `GET /zen/go/v1/models` — works with Bearer key, lists all models.
- `POST /chat/completions` — streams `chat.completion.chunk` SSE;
  `reasoning_content` delta present on reasoning models (Kimi, DeepSeek);
  `stream_options:{include_usage:true}` yields final usage chunk.
  **Fails without `x-opencode-session`** (`MissingSessionID` error).
- `POST /responses` — works with `store:false`, accepts `input` as string or
  `[{role,content}]` array, `reasoning:{effort}` accepted
  (none|low|medium|high|xhigh|max). Reasoning content is **encrypted**
  (never visible). Streams standard OpenAI Responses SSE:
  `response.output_text.delta` events, `response.completed` carries `usage`
  (`input_tokens`, `output_tokens`, `input_tokens_details.cached_tokens`).
