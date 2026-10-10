# MyOGChat — roadmap

## Phase 1 — done (current state)

**Core app**

- Single Go binary (`net/http` + `modernc.org/sqlite`), embedded PWA, SQLite
  (WAL), ~4 MB RAM on a Raspberry Pi 4B. Cross-compiled `linux/arm64` on the
  workstation.
- Two upstream dialects against OpenCode Go: OpenAI-compatible
  `/chat/completions` (Kimi, GLM, DeepSeek, MiMo, …) and the Responses API
  `/responses` (GPT 6 Luna, Grok) with a reasoning-effort selector.
  Per-conversation `x-opencode-session` + custom User-Agent (required by
  OpenCode Go).
- Streaming SSE chat; markdown + syntax highlighting; collapsible **thinking**
  (duration + reasoning tokens); per-message **cost** and **tokens/sec**;
  conversation running total; **regenerate** + copy; auto-generated titles.
- **Projects** with a system-instructions prompt + optional default model
  (instructions injected per dialect); "Unfiled" chats.
- **Themes** light/dark/system; settings panel (thinking display, token speed,
  Enter-to-send, sidebar collapse).

**Users & auth**

- Multi-user accounts: PBKDF2-HMAC-SHA256 password hashes (stdlib, no extra
  dep), first-run **admin setup**, **approval-gated self-registration** (pending
  until an admin approves), admin user management, last-admin lockout guard.
- Per-user data isolation (conversations/projects scoped by `user_id`;
  cross-user access → 404). Sessions are signed cookies keyed by a DB-backed
  `session_secret`.

**Mobile**

- Layout pinned to the **visual viewport** (`interactive-widget=resizes-content`
  - `visualViewport` sync) so the composer stays above the on-screen keyboard
    with no page jump. Installable PWA (cache v5).

**Deploy / ops**

- systemd **user** service on the Pi (`~/softwares/MyOGChat`); public via
  Cloudflare Tunnel at `https://chat.example.com`; secrets in
  `~/.config/myogchat/env`.
- Retention janitor (default 30 days, max 60).
- Tests: `models` (cost/peak/tier), `store` (migration, scoping, regenerate),
  `auth` (hashing/validation).

**Known limitations**

- One shared `OPENCODE_API_KEY` for everyone (per-user keys = Phase 2.1).
- Text only — no tools, no file/PDF attachments.
- Responses-dialect reasoning is encrypted upstream (no visible thinking).
- ~~No usage/quota accounting per user.~~ **Shipped** in 2.11 (usage view +
  optional admin quotas); retention caps the history the aggregates cover.

---

## Phase 2 — streamlined plan (ordered)

> Ordered by dependency + value. 2.1 is the foundation for 2.2.

### 2.1 Per-user API key (BYOK) + key settings — ✅ done (deployed)

**Goal:** each user can add their own OpenCode Go key; fall back to the server
key when sharing is allowed.

- Store the key **encrypted at rest** (AES-256-GCM, key from a DB-stored
  `encryption_key`); never return it in full (expose `has_key` + last 4 chars).
- **Resolution order** per request: user's own key → server key (if
  `share_server_key`, default on) → clear error prompting the user to add a key.
- Endpoints: `PUT /api/me/key` (set/replace/clear), key validated with a
  lightweight `/models` call before saving; `GET /api/auth/me` gains `has_key` /
  `key_hint`; `GET|PATCH /api/settings` gains `share_server_key` (admin).
- `OPENCODE_API_KEY` becomes **optional** at startup (a pure-BYOK deployment).
- UI: Settings → **OpenCode Go key** (status, add/replace/remove, which key is
  in use); admin toggle "share the server key with users".

### 2.2 Groups & shared keys — ✅ done (deployed)

- `groups(id, name, owner_id, share_key, created_at)` +
  `group_members(group_id, user_id, role)`.
- Resolution becomes: own key → group key (owner's key if the group allows
  sharing) → server key → error. Owner/admin manages members.
- UI: group switcher, member management, "use my key for this group" toggle.

### 2.3 Files & PDF attachments — ✅ done (deployed)

- Attach files to a message; extract text and fold into the prompt with a token
  budget.
- Text/code inline; **PDF** needs a text extractor. **Decision needed:** pure-Go
  lib (e.g. `ledongthuc/pdf`) — breaks the "single dependency" rule — or shell
  out to `pdftotext` (must exist on the Pi). Images → 2.5 (OCR).
- Limits: per-file size, total attachment size, and a clear token-budget
  indicator.

### 2.4 Tools: web search (+ extensible tool loop) — ✅ done (deployed)

- Requires an **agentic tool-calling loop**: model requests a tool → we execute
  → feed results back → repeat (bounded iterations). Chat dialect uses `tools`
  (function calling); Responses dialect uses `tools` too — both need parsing +
  execution + re-send.
- **Web search** is now provider-agnostic: a pluggable layer with a fallback
  chain **Tavily → Brave → Serper → DuckDuckGo** (`docs/2.4-search-providers.md`;
  Serper + Brave `llm/context` added in 2.12). Tavily (`TAVILY_API_KEY`), Brave
  (`BRAVE_API_KEY`) and Serper (per-user key, optional `SERPER_API_KEY`) are
  keyed; DuckDuckGo is keyless best-effort, so `web_search` works with zero
  configuration. Provider is selectable via `MYOGCHAT_SEARCH_PROVIDER` / the
  `search_provider` DB setting (Brave key settable from the admin UI, encrypted
  at rest).
- Later tools reuse the same loop (calculator, fetch-url, python via 2.6).

### 2.5 OCR for images — ✅ done (deployed)

- Extract text from images → context. Depends on the file pipeline (2.3).
- Runs **Tesseract in an ephemeral, hardened Docker container** (stdin→stdout),
  so the Pi needs no host `tesseract` (and no sudo). Image built by
  `deploy/ocr-setup.sh` from `deploy/ocr/Dockerfile` (`alpine:3.20` +
  `tesseract-ocr` + `tesseract-ocr-data-eng`).
- `internal/extract` delegates images to an injected `ocr.OCRFunc` (nil → the
  old `ErrUnsupported` fallback). Env `MYOGCHAT_OCR*` + DB setting `ocr_enabled`
  (admin toggle in Settings). See `docs/2.5-ocr.md`.

### 2.6 Python sandbox (multi-purpose app) — ✅ done

- `run_python` runs a script in a hardened, ephemeral **Docker** container
  (`python:3.14-slim`): CPU/mem/pids limits, read-only rootfs + writable `/work`
  tmpfs, non-root user, all caps dropped, dedicated bridge network, per-call
  timeout with forced cleanup. Code goes in over stdin. Outbound network is
  available (owner's choice); optional LAN-egress hardening via
  `sudo deploy/sandbox-setup.sh --harden`. See `docs/2.6-2.7-sandbox.md`.

### 2.7 Bash sandbox (lightweight tool runtime) — ✅ done
- `run_bash` runs a script in the same sandbox engine (`bash:5.3`), sharing the
  2.4 tool loop and the `internal/sandbox` runner. Files do not persist between
  calls. See `docs/2.6-2.7-sandbox.md`.

### 2.8 URL summarization + Linkwarden integration — ✅ done (deployed)
- **Summarize a URL**: fetch → extract readable text → summarize (a `fetch_url`
  tool in the 2.4 loop).
- **Linkwarden**: connect to the owner's instance (URL + token in settings) to
  search saved links and save new ones directly from a chat.

### 2.9 Full settings page (not a popup) — ✅ done (deployed)
- Settings is now a dedicated, mobile-first **page** at the hash route
  `#/settings` (vanilla JS, no build step) with sections: **Account**,
  **Appearance**, **OpenCode Go key**, **Groups**, **Integrations** (admin),
  **Users** (admin), **About**. The old modal is gone — the gear button
  navigates to the route and the in-page back button / browser (Android) back
  button return to the previous view. See `docs/2.9-settings-page.md`.

> **Status: Phase 2 is complete and deployed** (`https://chat.example.com`).
> Host provisioning done on the Pi: `deploy/sandbox-setup.sh` (network +
> `bash:5.3`/`python:3.14-slim`) and `deploy/ocr-setup.sh` (`myogchat-ocr`).
> Optional hardening still pending owner action:
> `sudo deploy/sandbox-setup.sh --harden` (blocks sandbox LAN egress).

### Cross-cutting decisions (settled)

- **Dependency policy:** PDF uses pure-Go `github.com/ledongthuc/pdf`; OCR and the
  code sandbox run **Docker** containers on the Pi (no new Go deps, no host
  binaries). See `docs/2.5-ocr.md`, `docs/2.6-2.7-sandbox.md`.
- **Secrets at rest:** encrypted with `internal/secret` (per-user keys, group
  keys, Linkwarden token, Brave key).
- **Usage accounting:** shipped (2.11) — per-user token/cost tracking plus
  optional admin quotas (natural follow-on to 2.1/2.2). See
  `docs/2.10-2.11-tool-activity-usage.md`.

## Parallelization plan (git worktrees)

Each workstream develops on its own **git worktree + branch off `main`**; the owner
merges sequentially and resolves the small additive conflicts in shared files
(`internal/server/server.go` routes, `internal/store/store.go` schema,
`static/app.js`).

- **Batch 1 (parallel, done):** 2.2 Groups · 2.3 Files/PDF · 2.4+2.8 Tools & web/URL/Linkwarden
- **Batch 2 (parallel, done):** 2.6 Python sandbox · 2.7 Bash sandbox · 2.5 OCR · 2.9 Settings page
  · multi-provider web search (2.4 extension)
- **Batch 3 (in progress):** 2.10 tool activity ✅ · 2.11 usage/quotas ✅ ·
  2.12 Serper + Brave llm/context · 2.13 vision-model OCR ✅

---

## Phase 2.x — polish & extensions

> **Finalized scopes live in [`docs/SCOPING.md`](SCOPING.md);** full research
> reports in [`docs/research/`](research/). Owner approved building
> **2.10 · 2.11 · 2.12 · 2.13 · 3.2 · 3.3 · 3.4**; **3.1 is de-scoped** (a
> self-host path is documented).

### 2.10 Persist tool activity — ✅ shipped
- **Problem:** the inline tool block (`web_search · done · …`) was rendered live
  from SSE `tool` events but never stored, so it disappeared on reload/re-render.
- **Shipped:** `messages.tools TEXT` (JSON array of `{name,status,summary}`),
  mirroring `messages.attachments` (2.3). `streamReply` collects `EventTool`
  events via `mergeToolEvent` and saves them with the assistant message; the
  SPA's existing `buildToolsBlock(m.tools)` renders them after reload.
- **Files:** `internal/store/store.go`, `internal/server/chat.go`,
  `static/app.js` (comment), `static/sw.js` (cache bump), tests, docs.

### 2.11 Usage & quota accounting — ✅ shipped
- **Goal:** per-user token/cost visibility, plus optional admin-set caps.
- **Shipped:** usage is an aggregate over assistant `messages` joined to
  `conversations` (no new table; `idx_messages_created` added).
  `GET /api/me/usage?range=7d|30d|all` → totals by day/model; admin
  `GET /api/users/usage`. **Quotas:** admin settings `daily_token_limit` /
  `monthly_cost_limit` (0 = off) enforced in `handlePostMessage` /
  `handleRegenerate` after `resolveKey`, before the user message is persisted,
  returning `429` (fail-open on DB error); optional per-user overrides.
- **UI:** a self-contained **Usage** section in Settings (own usage; admins see
  all users + the quota controls).
- **Files:** `internal/server/usage.go`, `internal/store/store.go`,
  `internal/server/{chat,users,server}.go`, `main.go`, `static/*`, tests, docs.
  Full notes: `docs/2.10-2.11-tool-activity-usage.md`.

### 2.12 Search providers — Serper + Brave `llm/context` — ✅ done (branch `feat/search-providers2`)
- The `SearchProvider` interface (`internal/tools/search.go`) makes this small.
- **Serper.dev** (`POST https://google.serper.dev/search`, header `X-API-KEY`,
  response `organic[].{title,link,snippet}` — `link`, not `url`) added as the 4th
  provider; chain is **Tavily → Brave → Serper → DDG** (`maxSearchChain` 3→4).
  `num` is capped at 10 to stay at 1 credit.
- **Serper key is per-user** (owner request): each user sets their own key in
  Settings → Web search (`GET/PUT /api/me/serper`, encrypted with
  `internal/secret` in `users.serper_key_enc`), resolved per request: own key →
  optional server `SERPER_API_KEY`. Visible to all users, not admin-only.
- **Brave `llm/context`** is a **mode flag on the existing Brave provider**
  (`MYOGCHAT_BRAVE_MODE=web|context`, DB `brave_mode`), not a separate provider;
  `grounding.generic[].snippets[]` are joined into `Result.Snippet`. The
  model-facing `web_search` tool is unchanged.
- **UI:** per-user Serper key field + provider list (`#my-search-section`) +
  Serper option and Brave mode in the admin Web search block.
- **Files:** `internal/tools/search.go`, `internal/server/integrations.go`,
  `internal/server/{server,users,chat}.go`, `internal/store/store.go`, `main.go`,
  `static/{index.html,app.js,sw.js}`, `docs/2.4-search-providers.md`,
  `API_CONTRACT.md`. **Effort:** small–medium.

### 2.13 Vision-model OCR (alternative engine) — ✅ done (branch `feat/vision-ocr`)
- **Finding:** OpenCode Go exposes vision **input** models — `gpt-6-luna`
  (responses dialect; input = text, image, pdf) and `deepseek-v4-flash-vision-exp`
  (chat). They can transcribe images, so OCR can optionally run through a vision
  model instead of (or in addition to) Tesseract.
- **Shipped:** `internal/ocr.VisionModel` implements the existing `Engine`; a
  single **non-streaming** `upstream.VisionOnce` owns both wire shapes (chat
  `image_url` object, responses `input_image` flat). A `ocr.KeyedEngine` lets
  `Server.ocrFunc(ctx, u)` bind the per-request key (BYOK → group → shared).
  `FallbackEngine` retries on `ErrUnavailable`; `MYOGCHAT_OCR_ENGINE=tesseract|
  vision|auto` (default `tesseract`; `auto` = Docker preflight at startup).
  Model registry gains `vision bool`; images downscaled past
  `MYOGCHAT_OCR_MAX_IMAGE_DIM`. See `docs/2.13-vision-ocr.md`.
- **Files:** `internal/ocr` (`vision.go`, `select.go`, `downscale.go`),
  `internal/upstream/vision.go`, `internal/server/ocr.go`, `internal/models`,
  `main.go`, docs. **Effort:** medium.

---

## Phase 3 (scoped — exploratory, not scheduled)

### 3.1 Image generation — IN (self-hosted on the Proxmox GPU)
- **External providers out** (owner isn't getting one; OpenCode Go has no
  image-output model) → **self-host** on the Proxmox box.
- **GPU:** GTX 1650 Ti Mobile (Turing, CC 7.5, 4 GB), driver 615.71.09 — fully
  supported by **current CUDA 13 / PyTorch cu128** (the earlier "Pascal/CUDA
  12.x" caveat does not apply). **Verdict: GO** — SD-Turbo ~8–12 s @512², SD1.5
  ~18–35 s; SDXL/Flux don't fit 4 GB.
- **Runtime:** **ComfyUI** (`--force-fp16`) in an **LXC with device passthrough**;
  front it with a tiny **OpenAI-compatible shim** (`POST /v1/images/generations`).
- **App side:** new **binary `images` table** + `POST /api/images` +
  `GET /api/images/{id}` + a chat "generate image" UI.
- **Gate:** time one SD-Turbo 512² image; **< 15 s warm → proceed**.
- **See `docs/SCOPING.md` §3.1 and `docs/research/3.1-imagegen-recheck.md`.**

### 3.2 IMAP email — ✅ done (read-only)
- **Read/search** mail via **app-specific passwords** (Gmail/iCloud/**Zoho Pro
  (India)**), per-user encrypted creds (`internal/secret`), implicit TLS with
  cert verification. Tools are only **`email_folders` / `email_search` /
  `email_read`** — **no `email_send`, no SMTP client**.
- **Read-only by construction** (layered): no write path; `EXAMINE`
  (`SelectOptions{ReadOnly:true}`); `BODY.PEEK[]`; and a **transport command
  allowlist** that rejects any command outside the read-only set (`UID` narrowed
  to `FETCH`/`SEARCH`). See `docs/3.2-imap.md`.
- `mail_accounts` table + per-user Settings section (provider preset, email, app
  password, custom host/port, test/save/disconnect, tools toggle). Caps: 100 KB
  body, 5 MB attachment (metadata only), 50 search results.
- Files: `internal/mail/` (read-only client + allowlist), `internal/tools/email.go`,
  `internal/server/email.go`, `internal/store` (schema + CRUD), `static/`
  (`#email-section`), `docs/3.2-imap.md`, `API_CONTRACT.md` §11.

### 3.3 Calendar — ✅ implemented (branch `feat/calendar`)
- **Apple/iCloud + Zoho → CalDAV** (read/write, app-specific password).
  **Google → read-only via the secret ICS URL** (no OAuth/consent; owner decision).
  `calendar_list` / `calendar_create` / `calendar_update` / `calendar_delete`
  tools (registered per-request only when the user connected a calendar) + a
  self-contained Settings section. New `calendar_accounts` table (`kind`
  discriminator + encrypted credential). Pure-Go `emersion/go-webdav` +
  `go-ical`. See `docs/3.3-calendar.md` and `API_CONTRACT.md` §11.
- v1 = whole-series edits; ICS is read-only (write tools return a clear error).

### 3.4 Maps — ✅ done (branch `feat/maps`)
- **OSM-only (keyless)** — Nominatim (geocode/reverse) + OSRM (directions) +
  Photon/Overpass (places). Four `maps_*` tools reusing the 2.4 loop; global
  1 req/s limiter + a SQLite result cache (policy). See `docs/3.4-maps.md`.

### 3.5 Speech-to-text (STT) — ✅ done (implemented, pending deploy)
- **Ollama does NOT serve STT** (LLMs + embeddings only). Pi default:
  **whisper.cpp `base.en`** (MIT, ~1.5–2× real-time, HTTP server). GPU option:
  faster-whisper `large-v3` int8. Design: `internal/speech` + `POST /api/stt` +
  a PWA mic button (`MediaRecorder`). Persistent HTTP service (not one-shot
  Docker), OpenAI-compatible so a Proxmox shim is a drop-in.
  See `docs/3.5-3.6-speech.md` / `docs/SCOPING.md` §3.5 /
  `docs/research/3.5-3.6-tts-stt.md`.

### 3.6 Text-to-speech (TTS) — ✅ done (implemented, pending deploy)
- Pi default: **Piper** (real-time on the Pi; GPL-3.0 — note). GPU: Kokoro-82M
  (Apache-2.0). Design: `internal/speech` + `POST /api/tts` + a speaker button;
  Web Speech API fallback. See `docs/3.5-3.6-speech.md` / `docs/SCOPING.md` §3.6.
