# MyOGChat — API & Implementation Contract

**This file is the single source of truth for backend and frontend. Follow it exactly.
If something is ambiguous, prefer the simpler interpretation that keeps this contract stable.**

Base URL: `/api`. All JSON. All times unix **milliseconds**. All money in
**micro-dollars** (1/1,000,000 of a USD) as integer `cost_micros`.

## 1. Auth

Shared-token login. Cookie-based, stateless.

- `POST /api/login` — body `{"token": "<secret>"}`. Constant-time compare against
  `MYOGCHAT_AUTH_TOKEN`. On success: `204` + `Set-Cookie`.
  On failure: `401 {"error":"invalid token"}`.
- `POST /api/logout` — clears cookie. `204`.
- `GET /api/me` — `204` if authed, else `401`. (Frontend gates on this.)

Cookie: name `myogchat`, value = `base64url(expiryUnix|hmacSHA256(expiryUnix, sha256(MYOGCHAT_AUTH_TOKEN)))`,
`HttpOnly`, `SameSite=Lax`, `Path=/`, 30-day expiry. Set `Secure` attribute only
when `X-Forwarded-Proto: https` or request TLS. Middleware protects every `/api/*`
route except `/api/login`. Static files (`/`, app assets) are public — the SPA
itself shows the login screen when `/api/me` returns 401.

## 2. Data model (SQLite)

```sql
CREATE TABLE IF NOT EXISTS conversations (
  id         TEXT PRIMARY KEY,          -- UUID v4; also sent as x-opencode-session
  title      TEXT NOT NULL DEFAULT '',
  model      TEXT NOT NULL,             -- model id, e.g. "kimi-k2.7-code"
  effort     TEXT,                      -- NULL, or none|low|medium|high|xhigh|max
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
  id                TEXT PRIMARY KEY,
  conversation_id   TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  role              TEXT NOT NULL,      -- 'user' | 'assistant'
  content           TEXT NOT NULL,
  reasoning         TEXT,               -- chat-dialect reasoning_content (may be NULL)
  model             TEXT,               -- set on assistant messages
  prompt_tokens     INTEGER,
  completion_tokens INTEGER,
  cached_tokens     INTEGER,
  cost_micros       INTEGER,
  created_at        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_convo ON messages(conversation_id, created_at);
CREATE INDEX IF NOT EXISTS idx_convos_updated ON conversations(updated_at DESC);
```

Enable `PRAGMA journal_mode=WAL` and `PRAGMA foreign_keys=ON`.

## 3. REST endpoints

### `GET /api/models`
Returns the **static registry** (§5) as JSON array, sorted by `output_per_m` ascending:
```json
[{"id":"kimi-k2.7-code","name":"Kimi K2.7 Code","dialect":"chat","effort":false,
  "input_per_m":0.95,"output_per_m":4.0,"cache_read_per_m":0.19}]
```
(Fields `tier_*` and peak-pricing internals are NOT exposed.)

### `GET /api/conversations`
`[{"id","title","model","effort","created_at","updated_at"}]`, `updated_at` DESC.

### `POST /api/conversations`
Body `{"model":"kimi-k2.7-code","effort":"medium"}`. `effort` only allowed for
responses-dialect models; reject otherwise with `400`. Generates UUIDv4 id.
Returns the created conversation object. `201`.

### `GET /api/conversations/{id}`
`{"id","title","model","effort","created_at","updated_at","messages":[...]}`
with messages ascending by `created_at` (full message rows as in §2, snake_case).
`404` if unknown.

### `PATCH /api/conversations/{id}`
Body may contain `title` (string, ≤200 chars), `model` (valid registry id),
`effort` (valid for the conversation's dialect). Updates `updated_at`. Returns
updated conversation (without messages).

### `DELETE /api/conversations/{id}` → `204`.

### `POST /api/conversations/{id}/messages` — chat (SSE)

Body: `{"content":"<user text>"}` (non-empty, ≤100 KB).

Server flow:
1. Validate conversation + content. Insert user message.
2. Load full history (user/assistant messages, `role`+`content` only).
3. Call upstream via the dialect adapter for the conversation's model, with headers
   `Authorization: Bearer $OPENCODE_API_KEY`, `x-opencode-session: <conversation id>`,
   `User-Agent: myogchat/1.0`. Abort upstream when client disconnects.
4. Stream normalized SSE to the client as upstream events arrive.
5. On completion: insert assistant message (content, reasoning, usage, cost_micros),
   bump conversation `updated_at`.
6. **Auto-title**: if conversation `title` is still empty after the first assistant
   reply, make ONE non-streaming chat-dialect call to `longcat-2.5-preview-free`:
   messages = `[{"role":"user","content":"Write a 3-6 word chat title for this conversation. Reply with the title only, no quotes, no punctuation at the end.\n\nUser: <first user msg, truncated to 500 chars>\nAssistant: <first assistant reply, truncated to 500 chars>"}]`,
   `max_tokens: 20`. Sanitize to one line ≤80 chars. On any failure use the first
   40 chars of the first user message. Persist title and include it in `done`.

**SSE wire format** (`Content-Type: text/event-stream`, `Cache-Control: no-cache`);
each event is `event: <type>\ndata: <json>\n\n`, plus `: keepalive\n\n` comment
every 15 s while waiting on upstream:

| event | data JSON | when |
|---|---|---|
| `meta` | `{"message_id":"…","model":"…"}` | first, before any upstream call output |
| `reasoning` | `{"delta":"…"}` | reasoning tokens (chat dialect only) |
| `text` | `{"delta":"…"}` | visible assistant text |
| `usage` | `{"prompt_tokens":N,"completion_tokens":N,"cached_tokens":N,"cost_micros":N}` | after final upstream usage |
| `done` | `{"message_id":"…","title":"…"}` | successful end; `title` only if newly set |
| `error` | `{"message":"…"}` | any failure (upstream HTTP status + first 200 chars of body, or local error); stream ends |

## 4. Upstream adapters (internal, `internal/upstream`)

Both adapters emit a common channel of:
```go
type Event struct {
    Kind  string // "reasoning" | "text" | "usage" | "error"
    Delta string
    Usage *Usage // prompt_tokens, completion_tokens, cached_tokens
    Err   error
}
```

**chat dialect** → `POST https://opencode.ai/zen/go/v1/chat/completions`:
`{"model":…, "messages":[{"role","content"}…], "stream":true,
"stream_options":{"include_usage":true}}`. Parse `chat.completion.chunk` lines:
`choices[0].delta.reasoning_content` → reasoning; `choices[0].delta.content` →
text; final chunk with empty `choices` carries `usage` (`prompt_tokens`,
`completion_tokens`, `prompt_tokens_details.cached_tokens`).

**responses dialect** → `POST https://opencode.ai/zen/go/v1/responses`:
`{"model":…, "input":[{"role","content"}…], "store":false, "stream":true,
"max_output_tokens":4096, "reasoning":{"effort":<effort or "medium">,
"summary":"auto"}}`. Parse SSE events: `response.output_text.delta` (`delta`
field) → text; `response.reasoning_summary_text.delta` → reasoning (may never
fire); `response.completed` → `response.usage` mapped as
`prompt_tokens=input_tokens`, `completion_tokens=output_tokens`,
`cached_tokens=input_tokens_details.cached_tokens`.

Both: 5-minute overall timeout, non-2xx → `error` event with status + body snippet.

## 5. Model registry (`internal/models`) — static, exactly these

`dialect` is `chat` unless noted. Prices = USD per 1M tokens
(input / output / cache-read). `tier_threshold`: when `prompt_tokens` exceed it,
use tier prices (tier_input/tier_output/tier_cache).

| id | name | in | out | cache | tier_threshold | tier in/out/cache |
|---|---|---|---|---|---|---|
| longcat-2.5-preview-free | LongCat 2.5 Free | 0 | 0 | 0 | — | — |
| mimo-v2.6-flash | MiMo V2.6 Flash | 0.14 | 0.28 | 0.0028 | — | — |
| mimo-v2.5 | MiMo V2.5 | 0.14 | 0.28 | 0.0028 | — | — |
| hy3 | Hy3 | 0.14 | 0.58 | 0.035 | — | — |
| glm-5.3-flash | GLM-5.3 Flash | 0.15 | 0.50 | 0.03 | — | — |
| space-bunny | Space Bunny | 0.15 | 0.60 | 0.03 | — | — |
| longcat-2.0 | LongCat 2.0 | 0.30 | 1.20 | 0.006 | — | — |
| mimo-v2.6-pro | MiMo V2.6 Pro | 0.435 | 0.87 | 0.003625 | — | — |
| mimo-v2.5-pro | MiMo V2.5 Pro | 0.435 | 0.87 | 0.003625 | — | — |
| deepseek-v4-pro | DeepSeek V4 Pro | 0.66 | 1.98 | 0.022 | peak: 1.32/3.96/0.044 | |
| deepseek-v4.1-flash | DeepSeek V4.1 Flash | 0.15 | 0.60 | 0.003 | peak: 0.30/1.20/0.006 | |
| deepseek-v4-flash | DeepSeek V4 Flash | 0.15 | 0.60 | 0.003 | peak: 0.30/1.20/0.006 | |
| deepseek-v4-flash-vision-exp | DeepSeek V4 Flash Vision | 0.15 | 0.60 | 0.003 | peak: 0.30/1.20/0.006 | |
| hy4-preview | Hy4 Preview | 0.834 | 2.501 | 0.042 | — | — |
| kimi-k2.7-code | Kimi K2.7 Code | 0.95 | 4.00 | 0.19 | — | — |
| kimi-k2.6 | Kimi K2.6 | 0.95 | 4.00 | 0.16 | — | — |
| glm-5.2 | GLM-5.2 | 1.40 | 4.40 | 0.26 | — | — |
| glm-5.3 | GLM-5.3 | 1.40 | 4.40 | 0.26 | — | — |
| kimi-k3 | Kimi K3 | 3.00 | 15.00 | 0.30 | — | — |
| **gpt-6-luna** | GPT 6 Luna | 0.10 | 0.50 | 0.01 | 272000 → 0.20/0.75/0.02 | responses, effort |
| **gpt-5.6-luna** | GPT 5.6 Luna | 0.20 | 1.20 | 0.02 | 272000 → 0.40/1.80/0.04 | responses, effort |
| **grok-4.7** | Grok 4.7 | 2.00 | 6.00 | 0.50 | 200000 → 4.00/12.00/1.00 | responses, effort |
| **grok-4.6** | Grok 4.6 | 2.00 | 6.00 | 0.50 | 200000 → 4.00/12.00/1.00 | responses, effort |

DeepSeek peak hours: 01:00–04:00 and 06:00–10:00 UTC, **Mon–Fri**; all other
times use base prices.

**Cost formula** (integer micros, `math.Round`):
`cost_micros = (prompt−cached)·in_rate + cached·cache_rate + completion·out_rate`
where rates are `price_per_m / 1e6 · 1e6` applied per token. Tier/peak selection
happens before applying rates.

## 6. Frontend contract (`static/`, vanilla JS, no build step)

- `index.html`, `app.js`, `style.css`, `manifest.webmanifest`, `sw.js`,
  `icons/` (icon.svg master; 192/512 PNG generated at integration),
  `vendor/marked.min.js`, `vendor/highlight.min.js`, `vendor/highlight-theme.css`
  (vendored from jsdelivr, pinned versions).
- **Mobile-first**: bottom composer with safe-area padding, sidebar as overlay
  drawer on narrow screens, min 44px touch targets, dark theme.
- Login screen → `POST /api/login`; on 401 from any `/api` call, show login.
- Conversation list (new / rename via prompt / delete via confirm), model picker
  (grouped: Free / Budget / Mid / Premium by `output_per_m` bands
  0 / ≤0.6 / ≤2 / >2), effort picker (none..max) **visible only for
  responses-dialect models**.
- Send message: `fetch` POST, read body with `ReadableStream`, parse SSE frames
  manually (EventSource cannot POST). Render `text` deltas as markdown
  (incremental re-render of the message is fine), `reasoning` into a collapsed
  `<details>Thinking…</details>` block above the text.
- After `usage`: show per-message cost as `$X.XXXX` (micros/1e6, 4 decimals;
  show `<$0.0001` for tiny nonzero, `Free` for 0-cost models).
- Conversation header shows running total cost (sum of message costs).
- PWA: `manifest.webmanifest` (name MyOGChat, standalone, theme `#0d1117`),
  `sw.js` caches only static shell (never `/api/*`), network-first for API.
- No frameworks, no npm, no build step. Everything must work when served
  from Go `embed.FS`.

## 7. Ops

- Janitor: on startup and hourly — `DELETE FROM conversations WHERE updated_at < cutoff`.
- Log lines to stdout (journald captures): startup config (redact secrets),
  request log (method, path, status, ms), upstream errors, janitor counts.
- Graceful shutdown on SIGTERM (close db, finish in-flight SSE within 5 s).

## 8. v2 additions — projects, tools, settings

This section extends the contract. Where it conflicts with earlier sections, **this section wins**.

### 8.1 Schema additions

```sql
CREATE TABLE IF NOT EXISTS projects (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL,
  instructions  TEXT NOT NULL DEFAULT '',
  default_model TEXT,                 -- nullable registry model id
  sort_order    INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
-- conversations gains: project_id TEXT REFERENCES projects(id) ON DELETE SET NULL
-- messages gains:      reasoning_tokens INTEGER, duration_ms INTEGER
```

The store must migrate an existing database: `CREATE TABLE IF NOT EXISTS` for
projects, and `ALTER TABLE … ADD COLUMN` for `conversations.project_id`,
`messages.reasoning_tokens`, `messages.duration_ms` when
`PRAGMA table_info(...)` shows them missing. Never drop existing data.

### 8.2 Projects API

- `GET /api/projects` → array sorted by `sort_order` ASC then `created_at` ASC:
  `{"id","name","instructions","default_model","sort_order","created_at","updated_at","conversation_count"}`
  (`conversation_count` = chats currently in the project).
- `POST /api/projects` `{"name" (required, ≤120), "instructions"?, "default_model"?}`
  → `201` project object (`conversation_count: 0`). `default_model` must be a
  registry id or absent.
- `PATCH /api/projects/{id}` `{"name"?, "instructions"?, "default_model"?, "sort_order"?}`
  → `200` updated project. `instructions` ≤ 8000 chars. `default_model: ""` or
  `null` clears it.
- `DELETE /api/projects/{id}` → `204`. Chats are **not** deleted; their
  `project_id` becomes NULL (explicit `UPDATE` before delete).

### 8.3 Conversations changes

- Conversation JSON gains `project_id` (nullable string).
- `POST /api/conversations` `{"model"?, "effort"?, "project_id"?}`: if
  `project_id` is present it must exist; if `model` is omitted the project's
  `default_model` is used (error if neither). `effort` rules unchanged.
- `PATCH /api/conversations/{id}` also accepts `project_id` (string to move,
  `""`/`null` to unfile).
- `GET /api/conversations` accepts optional `?project_id=<id|none>` filter;
  otherwise returns all.

### 8.4 Message JSON additions

Assistant messages gain `reasoning_tokens` (int|null) and `duration_ms`
(int|null — measured from first streamed token to the end of the stream, used
for tokens/sec).

### 8.5 Regenerate endpoint

`POST /api/conversations/{id}/regenerate` → same SSE wire format as
`/messages`. It deletes every assistant message after the last user message
(so the tail is a user message) and streams a fresh assistant reply from the
remaining history. `400` if the conversation has no user message. Overwrites
nothing else; `updated_at` is bumped.

### 8.6 Upstream changes

- `Params` gains `Instructions string`.
  - **chat dialect**: when non-empty, prepend `{"role":"system","content":Instructions}`
    to the messages array.
  - **responses dialect**: set top-level `"instructions": Instructions`.
  - Instructions come from the conversation's project (`projects.instructions`).
- `Usage` gains `ReasoningTokens`:
  - chat: `usage.completion_tokens_details.reasoning_tokens`
  - responses: `usage.output_tokens_details.reasoning_tokens`

### 8.7 Model registry

`Model` gains `thinking bool` (exposed in `/api/models`) — true when the model
**surfaces visible reasoning text** (chat dialect) or exposes a reasoning-effort
control (responses dialect). Models that reason internally without surfacing it
(e.g. Kimi K3, Kimi K2.6, MiMo, Hy) are `false`. The UI shows a "thinking" badge
for `true` models.

### 8.8 Frontend additions

LocalStorage keys (JSON or plain strings), namespaced `myogchat.`:

| key | values | default |
|---|---|---|
| `myogchat.theme` | `system` \| `light` \| `dark` | `system` |
| `myogchat.thinking` | `expanded` \| `collapsed` \| `hidden` | `collapsed` |
| `myogchat.showTps` | `0` \| `1` | `1` |
| `myogchat.enterToSend` | `0` \| `1` | `1` |
| `myogchat.sidebarCollapsed` | `0` \| `1` | `0` |

Required UI:
1. **Theme**: `<html data-theme="light|dark">` set from the setting; `system`
   follows `prefers-color-scheme` and reacts to changes. Ship a light and a dark
   syntax-highlight stylesheet; toggle the disabled one.
2. **Settings page**: a dedicated, full-screen mobile-first **page** at the hash
   route `#/settings` (not a modal), opened from the gear button. Sections:
   **Account** (username/email from `GET /api/auth/me` + log out), **Appearance**
   (theme, thinking display, token speed, Enter-to-send), **OpenCode Go key**
   (§2.1), **Groups** (§2.2), **Integrations** (admin, §2.4/2.8), **Users**
   (admin, §9.6), **About** (name + version). The page scrolls independently,
   uses the shared visual-viewport variables (`--app-h`/`--app-top`) so the
   on-screen keyboard never breaks layout, and never overflows horizontally.
   A header back affordance and the browser/Android back button both return to
   the previous view (hash-driven). Esc also closes it. There is **one** settings
   surface — the old modal is removed.
3. **Sidebar**: a collapse/expand control on desktop (icon); collapsed state
   persisted. On mobile it stays an overlay drawer. Sidebar shows a
   **Projects** section (list, "New project" button, per-project menu:
   rename / edit instructions / set default model / delete) where each project
   expands to show its chats, then an **Unfiled**/"Chats" section.
4. **Project editor**: dialog with name, instructions textarea, default model
   select. Creating a chat inside a project sends that `project_id`; the model
   picker defaults to the project's `default_model`.
5. **Move chat to project**: from the chat's row menu (move to project / unfile).
6. **Thinking**: when `reasoning` is present, render a collapsible block whose
   summary shows "Thought for {duration} · {reasoning_tokens} tokens" when
   available; honours the thinking-display pref. Model picker shows a
   "thinking" badge for models with `thinking: true`.
7. **Token speed**: footer shows `{completion_tokens/duration}s → N tok/s` per
   assistant message when `duration_ms > 0` and `showTps` is on; live estimate
   while streaming (chars/4 over elapsed).
8. **Retry**: on the last assistant message, a "Retry" action calls the
   regenerate endpoint. **Copy**: copy a message's raw text, and copy code
   blocks (already required).

### 8.9 Web search providers

`web_search` is provider-agnostic (tool name + `{"query"}` schema unchanged).
An ordered fallback chain **Tavily → Brave → DuckDuckGo** answers the query;
DuckDuckGo needs no key, so the chain is never empty and the tool is registered
whenever any provider is usable. See `docs/2.4-search-providers.md`.

Env vars (read once at startup): `TAVILY_API_KEY`, `BRAVE_API_KEY`,
`MYOGCHAT_SEARCH_PROVIDER` (`tavily`|`brave`|`ddg`|`auto`, default `auto`),
`MYOGCHAT_SEARCH_RESULTS` (default 5, clamped 1–20), `MYOGCHAT_SEARCH_REGION`
(default `us-en`), `MYOGCHAT_SEARCH_SAFESEARCH` (`off`|`moderate`|`strict`,
default `moderate`).

DB settings: `search_provider` (overrides the env default) and `brave_key`
(encrypted at rest with `internal/secret`, takes precedence over `BRAVE_API_KEY`).
Both are surfaced in `GET/PATCH /api/settings` (see §9.4).

### 8.10 OCR for images (2.5)

Image uploads to `POST /api/attachments` are OCR'd to text and stored in
`attachments.text` exactly like text/PDF attachments (2.3); the endpoint's
request/response shapes are unchanged. Text/code and PDF behavior is unchanged.

- OCR runs Tesseract in an ephemeral, hardened Docker container (stdin → stdout);
  the host needs no `tesseract` (see `docs/2.5-ocr.md`).
- Detection: MIME `image/*` or a known image extension (`.png .jpg .jpeg .gif
  .bmp .tif .tiff .webp .pbm .pgm .ppm .pnm`).
- With OCR enabled, an image returns recognized text (trimmed, capped at
  `extract.MaxTextChars`). With OCR disabled/unavailable the upload is rejected
  with the existing `400` "unsupported file type" message; a failed OCR run
  returns `400 "could not read text from the image: …"`.
- Config: `MYOGCHAT_OCR` (default on), `MYOGCHAT_OCR_IMAGE`,
  `MYOGCHAT_OCR_LANGS` (default `eng`), `MYOGCHAT_DOCKER`, `MYOGCHAT_OCR_TIMEOUT`
  (default 30s).
- Global admin setting `ocr_enabled` (default `true`) gates OCR on top of the
  env switch and is surfaced by `GET`/`PATCH /api/settings` (§9.4) as
  `ocr_enabled` / `ocr_available`.

## 9. Users & authentication (replaces §1)

Multi-user accounts with username/email/password. The shared-token login is
**removed**; `MYOGCHAT_AUTH_TOKEN` is no longer read.

### 9.1 Schema

```sql
CREATE TABLE IF NOT EXISTS users (
  id            TEXT PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
  email         TEXT NOT NULL UNIQUE COLLATE NOCASE,
  password_hash TEXT NOT NULL,
  is_admin      INTEGER NOT NULL DEFAULT 0,
  status        TEXT NOT NULL DEFAULT 'active',  -- active | pending | disabled
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
-- conversations and projects each gain: user_id TEXT REFERENCES users(id) ON DELETE CASCADE
```

Settings keys (created with defaults on first open):
- `session_secret` — 32 random bytes, base64; generated once, used to sign cookies.
- `allow_registration` — `"1"` or `"0"`; default `"1"` (self-registration open).

Migrations are additive (ALTER TABLE ADD COLUMN) per §8.1. **Existing
conversations/projects with NULL `user_id` are legacy**: the first admin created
by setup claims them (see §9.3).

### 9.2 Password hashing

PBKDF2-HMAC-SHA256 from the Go **standard library** (`crypto/pbkdf2`) — no new
dependency. Per-user 16-byte random salt, **600000** iterations, 32-byte key.
Stored string: `pbkdf2$sha256$600000$<salt-base64>$<hash-base64>` (standard
base64). Verification uses `subtle.ConstantTimeCompare`. Passwords must be
**≥ 8 characters**.

### 9.3 Auth endpoints

Public (no session required): `status`, `setup`, `register`, `login`.

- `GET /api/auth/status` → `200 {"needs_setup": bool, "registration_open": bool}`
  (`needs_setup` = no users exist).
- `POST /api/auth/setup` `{"username","email","password"}` → `201` + session
  cookie. **Only allowed while no users exist** (else `403`). Creates the first
  user as **admin** and claims all legacy rows (`conversations`/`projects`
  with NULL user_id) for it.
- `POST /api/auth/register` `{"username","email","password"}` → `202`
  `{"status":"pending","message":"…"}`. The account is created with
  `status = "pending"` and **no session cookie is set** — a self-registered user
  cannot log in until an admin approves them. `403` when `allow_registration` is
  `"0"`. `409` on duplicate username/email (also for setup).
- `POST /api/auth/login` `{"identifier","password"}` → `204` + cookie.
  `identifier` is the username **or** email (case-insensitive). `401` on failure.
  `403` when the account `status` is `pending` (message: "account pending
  approval") or `disabled`.
- `POST /api/auth/logout` → `204`, clears the cookie.
- `GET /api/auth/me` → `200 {"id","username","email","is_admin"}` or `401`.

Session cookie: name `myogchat`, value = `base64url(userID|expiryUnix|hex(hmacSHA256(secret, userID|expiry)))`,
`HttpOnly`, `SameSite=Lax`, `Path=/`, 30-day expiry, `Secure` when the request
is HTTPS / `X-Forwarded-Proto: https`.

Validation: username 3–32 chars of `[A-Za-z0-9_.-]`; email must contain `@` and a
`.`, ≤ 254 chars; all trimmed. Duplicates (case-insensitive) → `409`.

### 9.4 Admin endpoints (admin session required)

- `GET /api/users` → `[{"id","username","email","is_admin","status","created_at"}]`
  (pending users included so the admin can approve them).
- `POST /api/users` `{"username","email","password","is_admin"?}` → `201`
  (created `status = "active"`).
- `PATCH /api/users/{id}` `{"password"?, "is_admin"?, "status"?}` → `200`
  (`status` ∈ `active` | `pending` | `disabled`; setting `active` approves a
  pending registration).
- `DELETE /api/users/{id}` → `204` (cannot delete yourself → `400`)
- `GET /api/settings` → `200`:
  `{"allow_registration": bool, "pending_users": N, "share_server_key": bool,
   "server_key_present": bool, "tools_enabled": bool, "tavily_configured": bool,
   "brave_configured": bool, "search_provider": "auto"|"tavily"|"brave"|"ddg",
   "search_active": "tavily"|"brave"|"ddg", "linkwarden_url": string,
   "linkwarden_configured": bool, "linkwarden_token_hint": string,
   "sandbox_enabled": bool, "sandbox_available": bool,
   "ocr_enabled": bool, "ocr_available": bool}`
  (`share_server_key` §2.1; tools/Linkwarden §2.4/2.8; search §8.9; sandbox §10;
   OCR §8.10)
- `PATCH /api/settings` accepts any subset of
  `{"allow_registration"?: bool, "share_server_key"?: bool,
   "tools_enabled"?: bool, "linkwarden_url"?: string, "linkwarden_token"?: string,
   "search_provider"?: "auto"|"tavily"|"brave"|"ddg", "brave_key"?: string,
   "sandbox_enabled"?: bool, "ocr_enabled"?: bool}`
  → `200` (same shape as GET). Unknown `search_provider` values normalize to
  `auto`; `brave_key: ""` clears the stored (encrypted) key; omitted
  `linkwarden_token`/`brave_key` leave the stored value untouched.

Non-admin callers of any of these get `403`.

### 9.5 Data scoping

Every conversation and project belongs to a user (`user_id`). All
conversation/project queries filter by the session user; fetching or mutating a
row that is not the caller's returns `404`. New rows set `user_id` to the caller.
Admins do **not** see other users' chats (only user administration).

### 9.6 Frontend

- **Boot sequence**: `GET /api/auth/status` first.
  - `needs_setup: true` → show a **Create admin account** screen
    (username, email, password + confirm) → `POST /api/auth/setup` → enter app.
  - else `GET /api/auth/me` → `200` enter app, `401` show login screen.
- **Login screen**: identifier + password → `POST /api/auth/login`. When
  `registration_open`, show a **Create account** toggle/second form
  (username, email, password) → `POST /api/auth/register`. Registration returns
  `202 pending` — show "Your account is awaiting admin approval" and **do not**
  enter the app or store a session.
- A `403` from login whose body mentions approval shows the pending-approval
  message; a `403` for `disabled` shows "account disabled".
- **Sidebar bottom**: show the signed-in username and a Log out button.
- **Settings page**: reachable at the hash route `#/settings` (see §8.8.2). The
  account section shows the signed-in username/email (`GET /api/auth/me`) and a
  log out button; admin-only sections (Users, Integrations, share-server-key)
  are hidden for non-admins.
- **Settings → Users** (admins only): list users (with a **status** badge and a
  **Pending** section/count), add a user (username, email, password, admin
  checkbox), **Approve**/**Reject** pending registrations (`PATCH status=active`
  / `DELETE`), disable/enable users, delete a user, and a **"Allow new
  registrations"** toggle (`GET`/`PATCH /api/settings`).
- Any `/api` `401` returns to the login screen; `403` on admin panels shows a
  toast and is hidden for non-admins.

## 10. Code sandbox (2.6 Python / 2.7 Bash)

The model gets two tools — **`run_bash`** and **`run_python`** — that execute
model-generated code in a hardened, resource-limited, **ephemeral** Docker
container and return stdout/stderr. Full design: `docs/2.6-2.7-sandbox.md`.

- Each call is a fresh `docker run --rm` container; **nothing persists** between
  calls (read-only rootfs, writable `/work` tmpfs).
- The code is piped to the interpreter over **stdin** (`bash -s` / `python -`);
  it is never interpolated into the command line.
- Hardening: dedicated user-defined bridge network (no published ports → no
  inbound path), `--cap-drop ALL`, `--security-opt no-new-privileges`,
  `--user 65534:65534`, `--read-only`, tmpfs `/work` + `/tmp`,
  `--memory 256m --memory-swap 256m --cpus 1 --pids-limit 128
  --ulimit nofile=256:256`, `--stop-timeout 2`, `--pull never`.
- Outbound network is available (the owner's choice). Optional LAN-egress
  hardening is applied by `sudo deploy/sandbox-setup.sh --harden`
  (`DOCKER-USER` DROPs to RFC1918 / link-local / loopback / CGNAT).
- Per-call wall-clock timeout (default 30 s) enforced with `exec.CommandContext`
  plus an explicit `docker rm -f <name>` on timeout/cancel so containers cannot
  leak. Unique `--name` + label `myogchat-sandbox=1`.
- Registered per request only when `tools_enabled` **and** `sandbox_enabled`
  (default true) **and** Docker is reachable. Missing images/network return a
  clear error pointing at `deploy/sandbox-setup.sh`.
- Env config: `MYOGCHAT_SANDBOX` (hard on/off), `MYOGCHAT_DOCKER`,
  `MYOGCHAT_SANDBOX_NETWORK`, `MYOGCHAT_SANDBOX_IMAGE_BASH` (default `bash:5.3`),
  `MYOGCHAT_SANDBOX_IMAGE_PYTHON` (default `python:3.14-slim`),
  `MYOGCHAT_SANDBOX_TIMEOUT`, `MYOGCHAT_SANDBOX_MEMORY`, `MYOGCHAT_SANDBOX_CPUS`,
  `MYOGCHAT_SANDBOX_MAX_OUTPUT`.
- `GET`/`PATCH /api/settings` gain `sandbox_enabled` (admin) and
  `sandbox_available` (read-only status).

## 11. Calendar (3.3)

Per-user calendar connection: **Apple/iCloud + Zoho via CalDAV** (read/write,
HTTP Basic with an app-specific password) and **Google via the per-calendar
"secret address" ICS URL** (read-only, no OAuth). Full design:
`docs/3.3-calendar.md`.

### 11.1 Schema (additive)

```sql
CREATE TABLE IF NOT EXISTS calendar_accounts (
  id           TEXT PRIMARY KEY,
  user_id      TEXT NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
  kind         TEXT NOT NULL,      -- caldav | ics
  provider     TEXT,               -- apple | zoho | google | custom
  calendar_id  TEXT,               -- default CalDAV collection path / ICS label
  username     TEXT,               -- CalDAV username (not secret)
  credential   TEXT,               -- AES-256-GCM: CalDAV password / ICS URL
  endpoint     TEXT,               -- resolved CalDAV server URL
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);
```

One account per user. `credential` is encrypted with `internal/secret` and never
returned to the client.

### 11.2 Endpoints (session required, per-user)

- `GET /api/me/calendar` → `200 {"connected":false}` or
  `{"connected":true,"kind":"caldav"|"ics","provider":string,
   "calendar_id":string,"username":string,"endpoint":string,"can_write":bool}`.
- `PUT /api/me/calendar`:
  - CalDAV: `{"kind":"caldav","provider"?,"endpoint","username","password","calendar_id"?}`
  - ICS: `{"kind":"ics","provider"?:"google","url","calendar_id"?}`
  Validates by connecting to the provider; on failure `400` with the provider
  error. On success stores the (encrypted) credential and returns the status
  object (CalDAV responses also include `"calendars":[{id,name,writable}]`).
- `DELETE /api/me/calendar` → `204` (disconnect).

### 11.3 Tools (registered per request only when the user has a connected calendar)

| tool | parameters |
|---|---|
| `calendar_list` | `from` (req, RFC3339 or `YYYY-MM-DD`), `to` (req), `query?`, `calendar_id?`, `max?` (≤100) |
| `calendar_create` | `title` (req), `start` (req), `end` (req), `description?`, `location?`, `all_day?`, `timezone?`, `attendees?:string[]`, `recurrence?` (RRULE), `reminders_minutes?:int[]`, `calendar_id?` |
| `calendar_update` | `uid` (req) + any `calendar_create` field (partial; edits the whole series) |
| `calendar_delete` | `uid` (req), `calendar_id?` |

Write tools return a clear read-only error on an ICS backend. Recurrence is
expanded server-side on CalDAV (`<C:expand>`) and client-side for ICS feeds;
v1 edits whole series only.
