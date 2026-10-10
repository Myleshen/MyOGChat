# MyOGChat — Handoff

> **Read me first.** This is the entry point for a new agent/session picking up
> this repo. It captures the live state, how to work in the codebase, and exactly
> what's left. Deeper detail lives in the docs listed below.

## TL;DR

MyOGChat is a personal, self-hosted ChatGPT-style PWA backed by an **OpenCode Go**
subscription. Single Go binary (embedded frontend + SQLite), running on a
Raspberry Pi 4B, exposed via a Cloudflare Tunnel. Multi-user, mobile-first.

- **Live:** `https://chat.example.com` (also `http://raspberrypi.local:8787` on LAN)
- **Repo:** `github.com/Myleshen/MyOGChat` — `main` is the source of truth
- **Phase 1:** complete. **Phase 2:** complete (2.1–2.13). **Phase 3:** complete.
  **Phase 4:** complete — 4.1/4.2/4.3a/4.3b/4.4/4.5a/4.5b shipped (see `docs/PHASE4.md`)
  (see status below; plan of record `docs/PHASE4.md`).

## Live deployment

| Thing | Value |
|---|---|
| Host | Raspberry Pi 4B (Ubuntu, aarch64), SSH `pi@raspberrypi.local` (alias `rpi` on the owner's machine) |
| Service | systemd **user** service `myogchat` → `~/softwares/MyOGChat/myogchat` (symlinked from `~/.local/bin/myogchat`) |
| DB | `~/.local/share/myogchat/chat.db` (SQLite, WAL) |
| Env | `~/.config/myogchat/env` (chmod 600): `OPENCODE_API_KEY`, `MYOGCHAT_PORT=8787`, `MYOGCHAT_RETENTION_DAYS=30`. **`MYOGCHAT_AUTH_TOKEN` is obsolete** (multi-user auth replaced it). |
| Tunnel | Cloudflare Tunnel (docker container `cloudflaretunnel`, dashboard-managed) → `http://raspberrypi.local:8787`. Owner manages the CF dashboard. |
| Repo on Pi | `~/softwares/MyOGChat` (checkout) + `~/softwares/MyOGChat.git` (bare relay — see "Deploy") |

**Deployed commit:** `6a1352d` (Phase 2 batch: BYOK, groups, files/PDF, tools).

> The Pi has **no Go toolchain** and **no GitHub key**. Builds are cross-compiled
> on the workstation and shipped. The Pi checkout pulls from the local bare relay
> (`git pull local main`), not GitHub.

## Repo map

```
main.go                     entrypoint; config; retention janitor; `user` CLI subcommand
internal/
  auth/       password hashing (PBKDF2 stdlib), session cookies, users endpoints
  secret/     AES-256-GCM helper for encrypted secrets at rest
  store/      SQLite: users, settings, conversations, messages, projects, groups, attachments
  models/     static model registry + cost/peak/tier pricing + thinking flags
  upstream/   OpenCode Go clients: chat + responses dialects, SSE parsing, agentic tool loop (agent.go)
  server/     HTTP routes, SSE streaming, auth middleware, per-user scoping
  tools/      tool registry + web_search (Tavily/Brave/DDG chain), fetch_url, Linkwarden, maps_* (OSM: Nominatim/OSRM/Photon+Overpass), run_bash/run_python
  sandbox/    hardened ephemeral Docker code sandbox runner (2.6/2.7)
  extract/    text + PDF extraction for attachments (images → injected OCR)
  ocr/        image OCR via Docker Tesseract (feature 2.5)
  speech/     STT (whisper.cpp) + TTS (Piper) via persistent HTTP services (3.5/3.6)
static/       embedded PWA (vanilla JS, no build step); vendor/ has marked + highlight.js
scripts/      ui-test.sh, ui-test-run.sh, test-user.sh
deploy/       systemd unit + deploy.sh + sandbox-setup.sh + ocr-setup.sh + ocr/ (Dockerfile) + env.example
docs/         2.2-groups.md, 2.3-attachments.md, 2.4-tools.md, 2.5-ocr.md, 2.6-2.7-sandbox.md, 2.8-linkwarden.md, 2.9-settings-page.md, 2.13-vision-ocr.md, 3.4-maps.md, 3.5-3.6-speech.md, PHASE4.md, research/phase4/
```

## Docs index

- `DESIGN.md` — architecture, config, ops
- `API_CONTRACT.md` — **the contract** (REST + SSE shapes, §1 auth … §9 users, §2.x additions). Keep it in sync.
- `AGENTS.md` — concise agent notes (layout, rules, commands)
- `TODO.md` — roadmap with per-item status
- `README.md` — user-facing overview
- `docs/` — `2.2-groups.md`, `2.3-attachments.md`, `2.4-tools.md`,
  `2.4-search-providers.md`, `2.8-linkwarden.md`, `2.10-2.11-tool-activity-usage.md`,
  `2.13-vision-ocr.md`, `3.4-maps.md`, `3.5-3.6-speech.md`
- `docs/PHASE4.md` — Phase 4 plan of record; `docs/research/phase4/` — the 1..5
  research reports behind it

## Conventions & hard constraints

- Go **stdlib `net/http`** only. Dependencies: `modernc.org/sqlite` (pure Go) and
  `github.com/ledongthuc/pdf` (pure Go, for PDF text). Both CGO-free.
- Must build with `CGO_ENABLED=0` and cross-compile to `linux/arm64`.
- Frontend: vanilla JS, **no build step**, mobile-first; everything served from
  Go `embed.FS`. Bump the `static/sw.js` cache version on asset changes.
- DB changes are **additive migrations** (`CREATE TABLE IF NOT EXISTS` /
  `ALTER TABLE ADD COLUMN`) in `internal/store.migrate()`; never drop data.
- Upstream requests need `x-opencode-session` (conversation UUID) and a non-generic
  User-Agent (`myogchat/1.0`).
- **Never commit secrets.** Per-user keys / Linkwarden tokens are encrypted with
  `internal/secret` (key = DB `encryption_key` setting).

## Build / test / run / deploy

```sh
# build (Go is pinned via .mise.toml)
mise exec -- go build ./...
mise exec -- go vet ./...
mise exec -- go test ./...            # auth, secret, store, models, extract, tools, upstream

# run locally
OPENCODE_API_KEY=... MYOGCHAT_PORT=8787 MYOGCHAT_DB=/tmp/m.db mise exec -- go run .

# cross-compile + deploy to the Pi (scp .new + mv, git pull, restart)
mise exec -- bash deploy/deploy.sh pi@raspberrypi.local
# or manually:
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 mise exec -- go build -trimpath -ldflags="-s -w" -o /tmp/myogchat-arm64 .
scp /tmp/myogchat-arm64 pi@raspberrypi.local:~/softwares/MyOGChat/myogchat.new
ssh pi@raspberrypi.local 'cd ~/softwares/MyOGChat && mv -f myogchat.new myogchat && chmod 755 myogchat && git pull -q local main && systemctl --user restart myogchat'

# user management directly in the DB (no admin creds; for test accounts / resets)
scripts/test-user.sh add --username uitest --email uitest@example.com --password uitestpass123
scripts/test-user.sh rm  --username uitest
scripts/test-user.sh list
#   (same CLI on the Pi: ~/softwares/MyOGChat/myogchat user add|rm|list)
# per-user secrets, no login (encrypted with internal/secret):
#   myogchat user setkey --username <name> [--serper <key>] \
#     [--linkwarden-url <url>] [--linkwarden-token <token>]
```

## Testing (UI/UX is a gate — use agent-browser)

`agent-browser` (Vercel Labs CLI) is installed; the **agent-browser skill** is
registered. Prefer it for all UI testing.

```sh
# FULL isolated run: fresh test DB + seeded admin & test user + server + battery
scripts/ui-test-run.sh

# battery only (against any running instance)
scripts/ui-test.sh <BASE_URL> <TEST_USER> <TEST_PASS>
#   env: ADMIN_USER/ADMIN_PASS (admin pass), SKIP_ADMIN=1 (skip admin pass), SHOTS, DEVICE
```

- `ui-test.sh` is a two-pass mobile battery (Pixel 7): as a **normal user** —
  login, no-overflow, keyboard (composer pinned, no page scroll), sidebar,
  projects, attachments, settings (key + groups), themes, send; as **admin** —
  badge, Users panel, Integrations. Prints PASS/FAIL + saves screenshots to
  `/tmp/opencode/ui/`. Currently **28/28** locally, **22/22** on the tunnel (SKIP_ADMIN).
- The runner uses a **dedicated test DB** (`/tmp/opencode/myogchat-ui.db`) and
  seeds users directly into it — the real DB is never touched. It has a port guard.
- On the Pi: add a temp account via the CLI, test against the tunnel, then remove it.

## Phase status

**Phase 1 — done.** Core app, projects, themes, multi-user auth (PBKDF2,
approval-gated registration, admin, per-user isolation), mobile visual-viewport
fix, deploy.

**Phase 2 — 6 of 9 shipped:**

| # | Item | Status |
|---|---|---|
| 2.1 | Per-user keys (BYOK) | ✅ shipped |
| 2.2 | Groups & shared keys | ✅ shipped |
| 2.3 | Files & PDF attachments | ✅ shipped |
| 2.4 | Tools + web search | ✅ shipped (**web_search** is provider-agnostic: Tavily → Brave → Serper → DuckDuckGo fallback; DuckDuckGo needs no key. Serper + Brave `llm/context` added in 2.12) |
| 2.8 | URL summarization + Linkwarden | ✅ shipped (**Linkwarden is per-user** — each user sets their own instance URL + token in Settings; optional `LINKWARDEN_URL`/`LINKWARDEN_TOKEN` env fallback) |
| 2.5 | OCR for images | ✅ shipped (`DockerTesseract`; needs `deploy/ocr-setup.sh` on the host) |
| 2.6 | **Python sandbox** | ✅ shipped (`run_python`, hardened Docker sandbox; needs `deploy/sandbox-setup.sh` on the host) |
| 2.7 | **Bash sandbox** | ✅ shipped (`run_bash`, same engine) |
| 2.9 | **Full settings page (not a popup)** | ✅ shipped — `#/settings` page replaces the modal; see `docs/2.9-settings-page.md` |
| 2.10 | **Persist tool activity** | ✅ shipped — `messages.tools`; the inline tool block survives a reload |
| 2.11 | **Usage & quota accounting** | ✅ shipped — `/api/me/usage`, admin all-users view, optional quotas; see `docs/2.10-2.11-tool-activity-usage.md` |

All Phase 2 items (2.1–2.11) are implemented. Provision the host with
`deploy/sandbox-setup.sh` and `deploy/ocr-setup.sh`.
Parallelize with git worktrees off `main` (see "Parallel workstreams").

**Phase 3 — complete (deployed).** 3.1 image generation (self-hosted
ComfyUI/SD-Turbo on the Proxmox box; app side shipped — `docs/3.1-imagegen.md`),
3.2 IMAP read-only email (`docs/3.2-imap.md`), 3.3 calendar CalDAV/ICS
(`docs/3.3-calendar.md`), 3.4 keyless OSM maps (`docs/3.4-maps.md`), and
3.5/3.6 speech STT/TTS (`docs/3.5-3.6-speech.md`).

**Phase 4 — in progress** (plan of record `docs/PHASE4.md`; research reports in
`docs/research/phase4/`):

| # | Item | Status |
|---|---|---|
| 4.1 | Collapsible tool calls + full persistence | ✅ done — assistant messages persist tool args/output/error/duration/truncated in `messages.tools` (JSON, size-capped 16 KB args / 64 KB output / 4 KB error, UTF-8-safe); UI renders a collapsible `<details>` per message with per-tool Input/Output |
| 4.2 | Attachment chips in chat | ✅ done — sent user messages show file type-icon + name + size chips (metadata only; original bytes are **not** stored) |
| 4.3a | Image prompt enhancement | ✅ done — `POST /api/images/enhance` rewrites a brief into a rich SD-Turbo prompt (LLM via the caller's key, deterministic template fallback with `enhanced:false`); the SPA shows a preview & edit modal before generating |
| 4.3b | `generate_image` model tool | ✅ done — the model calls `generate_image` mid-conversation; the image is stored owner-scoped and attached to the assistant message (`messages.images`), so it renders in chat and survives a reload. Live-verified: prompt written by the model, PNG served via `GET /api/images/{id}` |
| 4.4 | Settings IA/UX overhaul | ✅ done — settings categorized (left nav rail ≥768px, horizontal chip bar on mobile) into Account / Appearance / AI & access / Integrations / Tools / Voice / Usage / Users / About; admin web-search block relabelled "Server web search (admin)"; per-user Serper under Integrations → Web search; UI battery 45/45 on mobile |
| 4.5a | Document extraction | ✅ done — `internal/extract` handles `.docx/.xlsx/.pptx` (OOXML), `.odt/.ods/.odp` (ODF), `.epub`, `.rtf` with the standard library only (no new deps) |
| 4.5b | Media transcription | ✅ done — audio/video attachments (`.mp3/.wav/.m4a/.ogg/.opus/.flac/.mp4/.mov/.webm/.mkv`) are transcribed via the configured speech-to-text service. **Asynchronous**: the upload returns `202 pending` and a bounded background worker fills in the transcript (so long clips never hit the tunnel timeout); the SPA polls until ready. Live-verified. |

## Known gaps / caveats (be honest)

- **web_search** now works with zero configuration (DuckDuckGo keyless fallback).
  Tavily/Brave keys (`TAVILY_API_KEY` / `BRAVE_API_KEY`, or the `brave_key` DB
  setting) and a per-user Serper key give higher-quality results; the provider is
  selectable in Settings → Integrations (`search_provider`). Serper + Brave
  `llm/context` (2.12) are on branch `feat/search-providers2` (not merged).
  `fetch_url` + Linkwarden are independent; **Linkwarden is now per-user**
  (`GET/PUT/DELETE /api/me/linkwarden`, `users.linkwarden_*` columns) with an
  optional `LINKWARDEN_URL`/`LINKWARDEN_TOKEN` env fallback. Operators can set a
  user's keys with `myogchat user setkey` (see `docs/2.8-linkwarden.md`).
- **Group key-sharing** is unit-tested (`internal/server/keys_test.go`) but a full
  live "member uses the owner's key" round-trip hasn't been done.
- **PDF** is unit-tested (+ a fixture) and the UI attach/send works; a real
  multi-page PDF hasn't been pushed through the UI yet.
- **Document extraction (4.5a)** now covers OOXML/ODF/EPUB/RTF entirely with the
  standard library; still **not** exercised against large/complex real-world
  files through the UI (unit-tested with fixtures).
- **Media transcription (4.5b)** is **synchronous**, so a long audio/video clip
  can exceed the Cloudflare Tunnel request timeout; async transcription is future
  work. Requires the configured speech-to-text service (`docs/3.5-3.6-speech.md`).
- **OCR (2.5)** is unit-tested (fake docker runner) and the extract path is
  covered; the live Docker smoke test was **not run** here (no Docker daemon /
  not in the `docker` group on the workstation). Build the image on the Pi with
  `bash deploy/ocr-setup.sh`, then `MYOGCHAT_OCR_LIVE=1 go test ./internal/ocr/
  -run LiveSmoke -v`.
- **Usage/quota accounting** (2.11) is implemented: per-user token/cost view
  (`GET /api/me/usage`), admin all-users view, and optional admin quotas
  (`daily_token_limit` / `monthly_cost_limit`, 0 = off) with per-user overrides.
  Note the retention janitor caps the history the aggregates cover (daily quotas
  exact; monthly exact only when retention ≥ the window). **Tool activity**
  (2.10) is now persisted on assistant messages (`messages.tools`) and survives a
  reload.

## Gotchas / tips

- **Service-worker cache** can mask new assets during testing: start a fresh
  browser session (`agent-browser close --all`) or hard-refresh. `sw.js` version
  is bumped per release (currently v8).
- **`pkill -f <pattern>`** can match your own shell command and kill it — use
  `pkill -x <name>` or a pattern that won't match the command line.
- **`/zen/go/v1/models` is public** (200 with no auth); only completions
  authenticate. Key validation uses a minimal free-model completion.
- **Split-horizon DNS**: on the owner's LAN, `chat.example.com` resolves to
  `raspberrypi.local` (the Pi), not Cloudflare. Off-LAN it goes through the tunnel.
- The **`rpi`** shell alias (`ssh pi@raspberrypi.local`) exists on the owner's
  machine; agents can use the IP directly.
- Retention janitor purges conversations (and now attachments) older than
  `MYOGCHAT_RETENTION_DAYS` (default 30, max 60).

## Parallel workstreams

For independent features, use git worktrees so agents don't collide:

```sh
git worktree add ~/dev/myogchat-wt/<name> -b feat/<name> main
# agent works there; then merge into main and resolve the additive conflicts in:
#   internal/server/server.go (routes), internal/store/store.go (schema),
#   static/app.js / index.html / style.css (UI sections)
```

After each merge: `go build/vet/test` + `node --check static/app.js` + run
`scripts/ui-test-run.sh`, then deploy and re-run the battery against the tunnel.

## Immediate next steps

1. (Optional) Add `TAVILY_API_KEY` to the Pi env to enable web search.
2. (Done) **2.5 OCR** — run `bash deploy/ocr-setup.sh` on the Pi (Docker) and restart.
3. (Done) **2.7 bash sandbox** + **2.6 python sandbox** — run
   `deploy/sandbox-setup.sh` on the Pi to create the network and pull
   `bash:5.3` / `python:3.14-slim`; optional `sudo … --harden` blocks LAN egress.
4. (Done) **2.9 settings page** — `#/settings` replaces the modal.
5. Keep `API_CONTRACT.md` / `TODO.md` current; add a `docs/2.x-*.md` per feature.
