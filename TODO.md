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
- No usage/quota accounting per user.

---

## Phase 2 — streamlined plan (ordered)

> Ordered by dependency + value. 2.1 is the foundation for 2.2.

### 2.1 Per-user API key (BYOK) + key settings ← starting now

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

### 2.2 Groups & shared keys

- `groups(id, name, owner_id, share_key, created_at)` +
  `group_members(group_id, user_id, role)`.
- Resolution becomes: own key → group key (owner's key if the group allows
  sharing) → server key → error. Owner/admin manages members.
- UI: group switcher, member management, "use my key for this group" toggle.

### 2.3 Files & PDF attachments

- Attach files to a message; extract text and fold into the prompt with a token
  budget.
- Text/code inline; **PDF** needs a text extractor. **Decision needed:** pure-Go
  lib (e.g. `ledongthuc/pdf`) — breaks the "single dependency" rule — or shell
  out to `pdftotext` (must exist on the Pi). Images → 2.5 (OCR).
- Limits: per-file size, total attachment size, and a clear token-budget
  indicator.

### 2.4 Tools: web search (+ extensible tool loop)

- Requires an **agentic tool-calling loop**: model requests a tool → we execute
  → feed results back → repeat (bounded iterations). Chat dialect uses `tools`
  (function calling); Responses dialect uses `tools` too — both need parsing +
  execution + re-send.
- **Web search** via Tavily (`TAVILY_API_KEY` is already in the owner's env).
- Later tools reuse the same loop (calculator, fetch-url, python via 2.6).

### 2.5 OCR for images

- Extract text from images (Tesseract binary or a Go binding) → context. Depends
  on the file pipeline (2.3). Also enables image→text for pasted screenshots.

### 2.6 Python sandbox (multi-purpose app)

- Run user/model code in a sandbox on the Pi (Docker container or `nsjail`):
  CPU/mem limits, no network by default, ephemeral FS. Heaviest item — needs a
  resource and security design; keep last.

### 2.7 Bash sandbox (lightweight tool runtime)
- Run small shell commands in a sandbox (resource-limited, **no network by
  default**, ephemeral working dir) so the model can do quick file/text/CLI tasks
  without a full Python env. Shares the 2.4 tool loop; use Docker / `nsjail` / `bwrap`.

### 2.8 URL summarization + Linkwarden integration
- **Summarize a URL**: fetch → extract readable text → summarize (a `fetch_url`
  tool in the 2.4 loop).
- **Linkwarden**: connect to the owner's instance (URL + token in settings) to
  search saved links and save new ones directly from a chat.

### 2.9 Full settings page (not a popup)
- Promote settings from the modal to a dedicated, mobile-first **page** (e.g.
  `#/settings`) with sections: Account, API key, Users (admin), Integrations
  (Linkwarden), Tools, About. Keep it PWA-fast — the PWA remains the main goal.

### Cross-cutting decisions to settle

- **Dependency policy:** 2.3 (PDF) and 2.5 (OCR) likely need either a new Go
  dependency or an external binary on the Pi — decide per item.
- **Usage accounting:** per-user token/cost tracking would help quota fairness
  once keys are shared (natural follow-on to 2.1/2.2).
- **Secrets at rest:** 2.1 introduces encrypted key storage — reuse that pattern
  for Linkwarden tokens etc.

## Parallelization plan (git worktrees)

Each workstream develops on its own **git worktree + branch off `main`**; the owner
merges sequentially and resolves the small additive conflicts in shared files
(`internal/server/server.go` routes, `internal/store/store.go` schema,
`static/app.js`).

- **Batch 1 (parallel):** 2.2 Groups · 2.3 Files/PDF · 2.4+2.8 Tools & web/URL/Linkwarden
- **Batch 2 (after 2.4):** 2.7 Bash sandbox · 2.6 Python sandbox · 2.9 Settings page

---

## Phase 3 (broad / exploratory — not planned yet)

- Image generation
- Personal tools: IMAP email, calendar, maps, etc.
