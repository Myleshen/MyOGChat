# MyOGChat — agent notes

Personal ChatGPT-style PWA backed by an OpenCode Go subscription.

**New here? Read `HANDOFF.md` first** (live state, status, testing, next steps).
Then `DESIGN.md` and `API_CONTRACT.md` — the contract is the source of truth.

## Layout

- `main.go` — config, startup, retention janitor, graceful shutdown
- `internal/models` — static model registry + cost calculation (tests here)
- `internal/store` — SQLite persistence (modernc.org/sqlite, WAL)
- `internal/upstream` — chat-completions + responses dialect adapters
- `internal/auth` — shared-token cookie auth
- `internal/server` — HTTP routes, SSE streaming, embedded SPA
- `static/` — vanilla-JS PWA (no build step), embedded via `//go:embed all:static`
- `deploy/` — systemd user unit + deploy script

## Rules

- Go stdlib `net/http` only. Single external dep: `modernc.org/sqlite` (pure Go).
- Must build with `CGO_ENABLED=0` and cross-compile to `linux/arm64`.
- Frontend has no build step; vendor libs live in `static/vendor/`.
- Never commit secrets. Runtime config comes from env / `~/.config/myogchat/env`.
- Upstream calls must send `x-opencode-session` (conversation UUID) and
  `User-Agent: myogchat/1.0` — required by OpenCode Go.

## Commands

```sh
mise exec -- go build ./...        # build
mise exec -- go test ./...         # tests (cost + peak-hour pricing)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 mise exec -- go build -o /tmp/opencode/myogchat-arm64 .
deploy/deploy.sh                   # cross-compile + install on the Pi
```

Local run:

```sh
OPENCODE_API_KEY=... MYOGCHAT_AUTH_TOKEN=... MYOGCHAT_PORT=8787 go run .
```

## Deployment

Runs on a Raspberry Pi 4B (Ubuntu, aarch64) as a **systemd user service**.
Logs: `ssh pi@raspberrypi.local journalctl --user -u myogchat -f`.
After first deploy, enable lingering once: `sudo loginctl enable-linger pi`.
Internet access is via the Cloudflare Tunnel (public hostname → `http://raspberrypi.local:8787`).

## v2 features

- **Projects**: named containers with a system `instructions` prompt + optional
  `default_model`; chats belong to 0/1 project. Instructions are injected every
  turn (chat dialect → leading `system` message; responses dialect →
  `instructions` field). Deleting a project unfiles its chats.
- **Thinking**: collapsible reasoning block with duration + reasoning-token
  count; `thinking` badge on models that surface reasoning. Cost/TPS fields:
  `messages.duration_ms`, `messages.reasoning_tokens`.
- **Regenerate**: `POST /api/conversations/{id}/regenerate` replaces the
  trailing assistant reply.
- **Settings** (browser-local, `myogchat.*` keys): theme, thinking display,
  token speed, Enter-to-send, sidebar collapse.
- Schema changes ship as **additive migrations** in `internal/store.migrate()`
  (safe on existing databases); covered by `store_test.go`.

## v3: users & authentication

- Multi-user accounts: `users` table with username/email, PBKDF2 password hashes
  (`crypto/pbkdf2`, stdlib — no new dependency), and a `status`
  (`active` | `pending` | `disabled`).
- **First run**: no users → the SPA shows a *Create admin account* screen
  (`POST /api/auth/setup`), which also claims any pre-auth (ownerless) chats.
- **Self-registration is approval-gated**: `POST /api/auth/register` creates a
  `pending` account with **no session**; an admin approves it in the Users panel.
- Only admins manage users (`/api/users`) and settings (`/api/settings`); the
  last active admin cannot be demoted, disabled, or deleted (lockout guard).
- Sessions are signed cookies keyed by a DB-backed `session_secret` (generated
  on first run). `MYOGCHAT_AUTH_TOKEN` is no longer used.
- All conversations/projects are scoped to their owner (`user_id`); cross-user
  access returns 404.
