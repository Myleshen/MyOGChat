# MyOGChat — Phase 2.x & Phase 3 scoping (finalized)

> **Status: scoped & approved, NOT started.** Each item below has a finalized
> approach, dependency list, effort estimate, and open questions. Full research
> reports are archived in `docs/research/`.
>
> Owner decisions: build **2.10, 2.11, 2.12, 2.13, 3.1, 3.2, 3.3, 3.4**.
> **3.1 image generation** is now **in** — self-hosted on the Proxmox GTX 1650 Ti
> (ComfyUI in an LXC), pending a one-time timing gate.

## Build plan (batches, sequential merges off `main`)

| Batch | Items | Theme |
|---|---|---|
| **3** | 2.10 · 2.12 · 2.13 | quick wins + tools polish |
| **4** | 2.11 | usage & quotas (touches chat + settings) |
| **5** | 3.2 · 3.3 · 3.4 | personal tools (mail, calendar, maps) |
| **6** | 3.1 | self-hosted image gen (Proxmox ComfyUI + OpenAI shim) |

Land order within a batch matters only for shared-file conflicts
(`internal/server/server.go` routes, `internal/store/store.go` schema,
`internal/server/integrations.go`, `static/app.js`). Recommended: 2.10 → 2.11;
CalDAV (3.3) before Google REST; OSM default before Google (3.4).

## Cross-cutting decisions

- **New Go deps (pure-Go, CGO-free, permissive licenses):**
  `github.com/emersion/go-imap/v2` + `go-message` + `go-sasl` (IMAP),
  `github.com/emersion/go-webdav` + `go-ical` (CalDAV). Maps and vision-OCR need
  **no new deps** (stdlib `net/http`).
- **Secrets at rest:** reuse `internal/secret` (AES-256-GCM) for every new
  credential (mailbox, calendar, Serper, Google Maps, image shim token). Never
  return secrets to the SPA — expose `connected`/`hint` only.
- **Per-user, not server-wide:** mailboxes and calendars are per-user (the app is
  multi-user). Tools register per-request only when the user has connected an
  account (same gating as Linkwarden).
- **UI:** new Settings sections follow the self-contained block pattern
  (`#ocr-section` / `#tools-section`).

---

## 2.10 Persist tool activity — small (~0.5 day)

- **Migration:** `messages` gains `tools TEXT` (JSON array
  `{name,status,summary}`), added in `internal/store/migrate()` (mirrors
  `messages.attachments`).
- **Store:** new `store.ToolActivity`; `Message.Tools []ToolActivity`; update
  `messageCols`, `scanMessage`, `InsertMessage` (placeholders 14→15).
- **Server:** `streamReply` (`internal/server/chat.go`) collects
  `upstream.EventTool` into a slice via a `mergeToolEvent` helper mirroring the
  SPA's `updateToolActivity` (running→done collapse); set on the assistant
  message before insert.
- **API/UI:** none needed — `GET /api/conversations/{id}` already marshals
  `[]store.Message`, and `buildToolsBlock(m.tools)` already renders it. Just bump
  `sw.js` cache.
- **Tests:** store round-trip; server includes tools.

## 2.11 Usage & quota accounting — medium (~2–3 days)

- **No new table:** aggregate assistant `messages` joined to `conversations` for
  `user_id`; day buckets via `strftime(..., unixepoch)`. Add `idx_messages_created`.
- **Store:** `UsageTotals` / `UsageRow` / `UserUsage` +
  `UserUsageTotals` / `UserUsageByDay` / `UserUsageByModel` / `AllUsersUsage`.
- **API:** new `internal/server/usage.go` — `GET /api/me/usage?range=7d|30d|all`
  and admin `GET /api/users/usage`.
- **Quotas:** global settings `daily_token_limit` / `monthly_cost_limit` (0 = off)
  + optional per-user override columns on `users`. A `checkQuota` helper called in
  `handlePostMessage` / `handleRegenerate` **after `resolveKey`, before persisting
  the user message**, returns `429` with a clear message; **fail-open** on DB
  error; skip queries entirely when limits are 0.
- **UI:** self-contained `#usage-section` (own usage for all; all-users table +
  limit controls for admins).
- **Caveat:** the retention janitor caps history — daily quotas always exact;
  monthly exact only when retention ≥ the window (documented; a `usage_daily`
  rollup is future work).

## 2.12 Search providers — Serper + Brave `llm/context` — small (~1 day)

- **Serper.dev:** a 4th `SearchProvider` (`serperProvider`).
  `POST https://google.serper.dev/search`, header `X-API-KEY`,
  body `{q,num,gl,hl,tbs,page,type}`; response `organic[]` = `{title,link,snippet,
  position}` (**`link`, not `url`**). 10 results = 1 credit, >10 doubles it;
  2,500 free queries. Chain: **Tavily → Brave → Serper → DDG** (`maxSearchChain` 3→4).
  **Per-user key (BYOK-style, owner request):** each user adds their own in
  **Settings → Integrations** (encrypted with `internal/secret`), resolved per
  request like the OpenCode key (own key → optional server env `SERPER_API_KEY`).
  Key: sign up at serper.dev → dashboard → API key (2,500 free one-time queries,
  no card). Optional — Brave/DDG already work.
- **UI:** per-user **Serper key** field + provider list in Settings → Integrations
  (visible to all users, not just admins).
- **Brave `llm/context`:** a **mode flag on the existing Brave provider**
  (`MYOGCHAT_BRAVE_MODE=web|context`, DB `brave_mode`) — not a separate provider
  (shares key/host/identity). `GET|POST /res/v1/llm/context`, response
  `grounding.generic[] = {url,title,snippets[]}` flattened into `Result.Snippet`.
  Model-facing `web_search` stays unchanged.
- **Tests:** httptest per provider; settings encryption/precedence; chain order.
- **Pitfalls:** credit doubling at `num>10`, oversized `llm/context` payloads.

## 2.13 Vision-model OCR — medium (~1.5–2 days)

- **Finding:** the Go gateway's `/models` exposes no modality metadata; the real
  source is models.dev + the docs. Image-input models include `gpt-6-luna` /
  `gpt-5.6-luna` (responses; text/image/pdf), `grok-4.7/4.6` (responses),
  `deepseek-v4.1-flash` and `deepseek-v4-flash-vision-exp` (chat). Note the Lunas
  **400 on `/chat/completions`** (responses-only).
- **Request shapes (confirmed):**
  - chat → `content[]` with `{"type":"image_url","image_url":{"url":"data:image/png;base64,…","detail":"high"}}`
  - responses → `input[]` with `{"type":"input_image","image_url":"data:…","detail":"high"}`
  Both accept data URIs.
- **Design:** `internal/ocr.VisionModel` implements the existing `Engine`; a single
  **non-streaming** `upstream.VisionOnce` owns both wire shapes (never the agent
  loop). A new `ocr.KeyedEngine` lets `Server.ocrFunc(ctx, u)` bind the per-request
  key from `resolveKey` (BYOK → group → shared). Config
  `MYOGCHAT_OCR_ENGINE=tesseract|vision|auto` — default **`tesseract`**, vision
  opt-in, `auto` decided at startup via a Docker preflight. Recommended model:
  `deepseek-v4-flash-vision-exp` (chat); `gpt-6-luna` as the cheap-input alt.
- **Tests/pitfalls:** httptest both shapes; `FallbackEngine` on `ErrUnavailable`;
  base64 size, JPEG/PNG/GIF/WebP only, short timeouts, single-shot enforcement,
  `extract.ErrOCR` mapping.

## 3.1 Image generation — IN (self-hosted on the Proxmox GPU)

- **External providers** (OpenAI Images, Replicate/fal.ai) are **out** — owner
  isn't getting one. OpenCode Go has no image-output model. **Instead we
  self-host** on the Proxmox box.
- **GPU (confirmed):** NVIDIA **GTX 1650 Ti Mobile** (`TU117`, **Turing**, CC 7.5,
  **4 GB GDDR6**), driver **615.71.09**. Turing is fully supported by **current
  CUDA 13 / PyTorch cu128** (the earlier "pin cu126 / CUDA 12.x max" advice was a
  **Pascal** artifact — ignore it). **FP16 = 2× FP32** here, so `--force-fp16`
  both halves VRAM and speeds things up.
- **Verdict: GO.** Fits **SD-Turbo** (~8–12 s @512²), **LCM-LoRA** (~6–12 s),
  **SD 1.5/2.1** (~18–35 s). SDXL/SD3/Flux do **not** fit 4 GB. Default:
  **SD-Turbo**.
- **Runtime:** **ComfyUI** (`--force-fp16 --reserve-vram 0.5`), in an **LXC with
  device passthrough** (host driver already loaded → no VFIO; bind `/dev/nvidia*`
  incl. `nvidia-uvm`/`nvidia-caps`, match userspace 615.71.09). VFIO VM is the
  isolated alternative.
- **Integration:** a tiny **OpenAI-compatible shim** (`POST /v1/images/generations`,
  bearer token, LAN-only; don't tunnel it) in front of ComfyUI. App side:
  `POST /api/images`, `GET /api/images/{id}` (bytes), `DELETE`, a new **binary
  `images` table** (owner-scoped BLOB; `attachments` is text-only), settings via
  `internal/secret`, janitor wiring, and a chat "generate image" UI.
- **Gate: PASSED (2026-10-10)** — SD-Turbo 512² = **~4.5–5 s** warm on **CT 101**
  (`comfyui`, `comfyui.local:8188`, full clone of the dev template, GPU passed
  through via `/dev/nvidia*` + userspace 615.71.09). ⚠️ **Must run ComfyUI with
  `--force-fp32`** — fp16 produced NaN/black images on this GPU (and fp32 is also
  ~2× faster here). GPU stays ~55 °C, ~2.5 GB VRAM. Quality: good (SD-Turbo, 4
  steps, `dpmpp_2m`/`karras`).

## 3.2 IMAP email — medium (~2–4 days)

- **Auth: app-specific passwords, NOT OAuth2** (Gmail mail scope is restricted →
  7-day refresh tokens in Testing mode; iCloud has no third-party OAuth).
  - Gmail `imap.gmail.com:993` TLS / `smtp.gmail.com:465|587`; 2SV required.
  - iCloud `imap.mail.me.com:993` / `smtp.mail.me.com:587` STARTTLS; app-specific password.
  - Zoho (owner: **Pro**, confirmed IMAP/SMTP) — India region:
    `imappro.zoho.in:993` (custom domain) / `smtppro.zoho.in:465|587`; personal
    `@zohomail.in` → `imap.zoho.in`/`smtp.zoho.in`. Region-aware presets
    (`.in`/`.com`/`.eu`); app-specific password.
- **Libs:** `emersion/go-imap/v2` + `go-message` + `go-sasl` (no XOAUTH2 type — use
  OAUTHBEARER or a ~15-line custom client if ever needed) + stdlib `net/smtp`.
- **Design (READ-ONLY by construction):** per-user encrypted credential blob
  (`auth_type`, host/port/TLS, password) — **no `allow_send`/`append_sent`**;
  per-call connect with timeouts; normalized `Message`; tools are **only**
  **`email_folders` / `email_search` / `email_read`** — **no `email_send`, no SMTP
  client in v1**.
- **Read-only enforcement (code-level, layered):**
  1. **No write path exists** — the IMAP wrapper exposes only read methods; no
     `APPEND`/`STORE`/`COPY`/`MOVE`/`EXPUNGE`/`CREATE`/`DELETE`.
  2. **Open read-only** — `EXAMINE` (go-imap v2 `SelectOptions{ReadOnly:true}`)
     instead of `SELECT` (no `\Seen`/`\Recent` side effects).
  3. **Fetch with `BODY.PEEK[]`** so reading never marks a message seen.
  4. **Transport command allowlist** — wrap the IMAP `net.Conn` in a filter that
     parses client commands and **rejects anything not on a read-only allowlist**
     (`CAPABILITY, NOOP, LOGIN, AUTHENTICATE, LIST, LSUB, STATUS, EXAMINE, SELECT,
     FETCH, SEARCH, UID, IDLE, CLOSE, LOGOUT`). Even a tool-layer bug cannot mutate
     the mailbox — **this is the key guardrail** (app passwords are full-access, so
     scoping must be ours).
  5. **Bodies are untrusted data** (prompt-injection): never instructions; with no
     write tools the blast radius is nil.
  6. **Outbound-only** (993/TLS verified); credentials never returned to the SPA.
- **Pitfalls:** prompt injection from mail bodies (treat as untrusted; gate send);
  Gmail auto-saves to Sent (iCloud/Zoho need `APPEND`); Gmail folders `[Gmail]/…`
  (use SPECIAL-USE); app passwords revoked on password change; cap attachments 5 MB
  / body 100 KB; force TLS verification.

## 3.3 Calendar — medium (~4–6 days)

- **Split by auth:** Apple/iCloud + Zoho are **CalDAV-only** (Basic: app-specific
  password; **read/write**). **Google → read-only via the per-calendar "secret
  address" ICS URL** (owner decision: no OAuth/consent, no GCP project — the URL
  *is* the credential, unauthenticated, resettable). Trade-off: **read-only** and
  lags ~12–24 h (no push/incremental sync). The service-account route (read/write,
  no consent screen) stays documented in `docs/research/3.3-google-calendar.md`
  as a future upgrade.
  - iCloud `caldav.icloud.com` → `.well-known/caldav` → `pNN-caldav.icloud.com` (cache the partition).
  - Zoho `calendar.zoho.com/.well-known/caldav` (region variants).
  - Google → the per-calendar **secret ICS URL** (`.../ical/<id>/private-<hash>/basic.ics`).
- **Libs:** `emersion/go-webdav` (CalDAV) + `go-ical` (MIT, pure Go).
- **Design:** per-user encrypted creds in a new `calendar_accounts` table with a
  **`kind` discriminator** (`caldav` | `ics`) + encrypted `credential` blob
  (CalDAV password / ICS URL) + `calendar_id` (mirrors `users.api_key_enc`). Tools
  **`calendar_list` / `calendar_create` / `calendar_update` / `calendar_delete`**
  (write ops only where the backend supports them — ICS is read-only);
  normalized `Event{uid, calendar_id, title, start, end, tz, all_day, recurrence,
  attendees, reminders, etag}`.
- **Pitfalls:** a service account **cannot invite attendees**
  (`forbiddenForServiceAccounts`) without domain-wide delegation, and sees only
  calendars explicitly shared with it; secret-ICS is read-only and lags
  (~12–24 h); server-side recurrence expansion (`singleEvents=true` /
  `<C:expand>`); always send ETags on CalDAV writes; all-day `DATE` vs timed
  `DATE-TIME`/`VTIMEZONE`; ICS folding/escaping. v1 = whole-series edits only.
- **Bonus (zero-OAuth):** a Google **Apps Script Web App** can read/write the
  owner's own calendar entirely inside Google's cloud — noted as an alternative.

## 3.4 Maps — medium (~3–4 days)

- **OSM-only (owner decision — no Google Maps billing).** A `GeoProvider`
  abstraction (mirrors `SearchProvider`) with a single **keyless `osmProvider`**:
  Nominatim (geocode/reverse), OSRM (directions), Photon/Overpass (places).
  No API key, no billing, no signup.
- **Tools:** `maps_geocode` / `maps_reverse` / `maps_directions` / `maps_places`.
  No new deps (stdlib + JSON; request OSRM `geojson` to skip polyline decoding).
- **Two policy must-dos:** a global **1 req/s limiter** for the OSM stack and a
  **SQLite result cache** (TTL 7–30 days; also satisfies Google's 30-day cap).
- **Config:** env + DB `maps_provider` (only `osm` for now) + `maps_region` /
  `maps_units`, in `GET/PATCH /api/settings`.
- **Pitfalls:** lon/lat ordering differs per endpoint (centralize + test); OSM
  ODbL attribution ("© OpenStreetMap contributors"); Nominatim forbids
  autocomplete and the OSRM demo server is not for production (self-host later if
  needed).

---

## 3.5 Speech-to-text (STT) — scoped (~2–3 d)

- **Ollama does NOT serve STT** (LLMs + embeddings only; no audio endpoints).
- **Pi 4B default: whisper.cpp `base.en`** (MIT; ~1.5–2× real-time, ~0.4 GB;
  ships an OpenAI-like HTTP server + arm64 Docker image). Streaming only via Vosk.
- **Proxmox GPU option:** faster-whisper `large-v3` int8 (~3 GB) / `distil-large-v3`.
- Design: `internal/speech` mirroring `internal/ocr` but with **persistent HTTP
  services** (models stay warm); `POST /api/stt` (multipart audio → text) + a PWA
  mic button (`MediaRecorder`). Keep the Go client OpenAI-compatible so the
  Proxmox shim is a drop-in. Full report: `docs/research/3.5-3.6-tts-stt.md`.

## 3.6 Text-to-speech (TTS) — scoped (~2–3 d)

- **Ollama does NOT do TTS.**
- **Pi 4B default: Piper** (real-time with `low`/`x_low` voices; ONNX; HTTP server
  mode). ⚠️ the maintained repo is **GPL-3.0** (old `rhasspy/piper` was MIT,
  archived). **Kokoro-82M** (Apache-2.0) is higher quality but slower-than-real-time
  on the Pi → GPU only. XTTS is non-commercial → skip.
- Design: `POST /api/tts` (text → WAV) + a speaker button on messages; Web Speech
  API as a zero-backend day-one fallback.
- **Hardware:** STT + TTS + embeddings fit in 4 GB, but the Pi **CPU** is the
  bottleneck — run sequentially, not concurrently; offload to the Proxmox GPU for
  quality. Effort ~4–6 d Pi-only, ~8–12 d with GPU offload.

---

## Resolved decisions

- **Zoho** — owner is on **Pro**; IMAP/SMTP confirmed (India-region endpoints).
- **Google Maps** — **dropped**; 3.4 is **OSM-only** (keyless, no billing).
- **Google Calendar** — **read-only via the secret ICS URL** (no OAuth/consent/
  GCP project). Apple/Zoho use CalDAV (read/write).
- **Serper** — optional; key from serper.dev (2,500 free one-time queries).

## Still open

1. **Serper key** — get one, or ship without it?

*(Image-gen gate: **PASSED** — see §3.1.)*
