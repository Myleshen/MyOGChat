# MyOGChat — Phase 2.x & Phase 3 scoping (finalized)

> **Status: scoped & approved, NOT started.** Each item below has a finalized
> approach, dependency list, effort estimate, and open questions. Full research
> reports are archived in `docs/research/`.
>
> Owner decisions: build **2.10, 2.11, 2.12, 2.13, 3.2, 3.3, 3.4**.
> **3.1 image generation** is de-scoped (no external provider) — a self-hosted
> path on the Proxmox box is documented below for a future go/no-go.

## Build plan (batches, sequential merges off `main`)

| Batch | Items | Theme |
|---|---|---|
| **3** | 2.10 · 2.12 · 2.13 | quick wins + tools polish |
| **4** | 2.11 | usage & quotas (touches chat + settings) |
| **5** | 3.2 · 3.3 · 3.4 | personal tools (mail, calendar, maps) |

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
  Config: `SERPER_API_KEY` env + encrypted DB `serper_key`.
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

## 3.1 Image generation — DE-SCOPED (self-host path documented)

- **External providers** (OpenAI Images, Replicate/fal.ai) are **out** — owner
  isn't getting one. OpenCode Go has no image-output model.
- **Self-host on the Proxmox box (`asus-rog`):** GPU is a **GTX 1650 Ti Mobile
  (Turing, CC 7.5, 4 GB)** — not a 1050. Driver `615.71.09` installed
  (`/dev/nvidia0`+`nvidiactl` present; no `nvidia-smi`/CUDA toolkit yet),
  `intel_iommu=on`, `vfio-pci` available, GPU in IOMMU group 2.
- **Verdict:** fits **SD 1.5 / SD 2.1 / SD-Turbo at 512²** (~3–8 s SD-Turbo,
  ~20–45 s SD1.5). **SDXL/SD3/Flux do not fit 4 GB.** Turing keeps current
  CUDA/PyTorch support and gets a 2× FP16 speedup (the report's "pin cu126 /
  Pascal limits" caveat does **not** apply).
- **Runtime:** **ComfyUI**, in an **LXC with device passthrough** (host driver
  shared; simplest) or a VFIO VM (isolated). Front it with a tiny
  **OpenAI-compatible shim** (`POST /v1/images/generations`, bearer token, LAN-only;
  don't tunnel it).
- **App side:** `POST /api/images` + a **binary store** — `attachments` is
  text-only today, so generated images need a new blob store + `GET /api/images/{id}`.
- **Gate:** time one SD-Turbo 512² image; **< 15 s → proceed**.

## 3.2 IMAP email — medium (~2–4 days)

- **Auth: app-specific passwords, NOT OAuth2** (Gmail mail scope is restricted →
  7-day refresh tokens in Testing mode; iCloud has no third-party OAuth).
  - Gmail `imap.gmail.com:993` TLS / `smtp.gmail.com:465|587`; 2SV required.
  - iCloud `imap.mail.me.com:993` / `smtp.mail.me.com:587` STARTTLS; app-specific password.
  - Zoho `imap.zoho.com:993` / `smtp.zoho.com:465|587`; app password — **⚠️ verify the
    plan includes IMAP/SMTP (free plans no longer do)**.
- **Libs:** `emersion/go-imap/v2` + `go-message` + `go-sasl` (no XOAUTH2 type — use
  OAUTHBEARER or a ~15-line custom client if ever needed) + stdlib `net/smtp`.
- **Design:** per-user encrypted credential blob (`auth_type`, host/port/TLS,
  password, `allow_send`, `append_sent`); per-call connect with timeouts;
  normalized `Message`; tools **`email_folders` / `email_search` / `email_read` /
  `email_send`** (send gated).
- **Pitfalls:** prompt injection from mail bodies (treat as untrusted; gate send);
  Gmail auto-saves to Sent (iCloud/Zoho need `APPEND`); Gmail folders `[Gmail]/…`
  (use SPECIAL-USE); app passwords revoked on password change; cap attachments 5 MB
  / body 100 KB; force TLS verification.

## 3.3 Calendar — medium (~4–6 days)

- **Split by auth:** Apple/iCloud + Zoho are **CalDAV-only** (Basic: app-specific
  password); Google is better via **REST v3** (OAuth2 only — Basic rejected since
  2025-03-14).
  - iCloud `caldav.icloud.com` → `.well-known/caldav` → `pNN-caldav.icloud.com` (cache the partition).
  - Zoho `calendar.zoho.com/.well-known/caldav` (region variants).
  - Google `www.googleapis.com/calendar/v3` with hand-rolled OAuth2 refresh
    (avoid `google.golang.org/api` — drags in grpc/protobuf/OTel).
- **Libs:** `emersion/go-webdav` (CalDAV) + `go-ical` (MIT, pure Go).
- **Design:** per-user encrypted creds in a new `calendar_accounts` table
  (mirrors `users.api_key_enc`); tools **`calendar_list` / `calendar_create` /
  `calendar_update` / `calendar_delete`**; normalized `Event{uid, calendar_id,
  title, start, end, tz, all_day, recurrence, attendees, reminders, etag}`.
- **Pitfalls:** Google Testing-mode refresh tokens expire in 7 days (publish the
  consent screen "In production"); server-side recurrence expansion
  (`singleEvents=true` / `<C:expand>`); always send ETags on CalDAV writes; all-day
  `DATE` vs timed `DATE-TIME`/`VTIMEZONE`; ICS folding/escaping. v1 = whole-series
  edits only.

## 3.4 Maps — medium (~3–4 days)

- **`GeoProvider` abstraction** (mirrors `SearchProvider`): `Geocode` / `Reverse` /
  `Directions` / `Places`, with:
  - **`osmProvider` (keyless, default):** Nominatim (geocode/reverse), OSRM
    (directions), Photon/Overpass (places).
  - **`googleProvider` (keyed):** Geocoding, Routes `computeRoutes`, Places Text
    Search (New).
  - `auto` = Google when a key is set, else OSM (never empty).
- **Tools:** `maps_geocode` / `maps_reverse` / `maps_directions` / `maps_places`.
  No new deps (stdlib + JSON; request OSRM `geojson` to skip polyline decoding).
- **Two policy must-dos:** a global **1 req/s limiter** for the OSM stack and a
  **SQLite result cache** (TTL 7–30 days; also satisfies Google's 30-day cap).
- **Config:** env + DB `maps_provider` / `google_maps_key` (encrypted) +
  `maps_region` / `maps_units`, in `GET/PATCH /api/settings`.
- **Pitfalls:** Google requires billing + key restrictions (per-SKU free tiers —
  the pooled $200 credit was retired Mar 2025); Places/Routes "New" need
  `X-Goog-FieldMask`; lon/lat ordering differs per provider (centralize + test);
  OSM ODbL attribution; transit is Google-only; IP-restricting a key behind the
  Cloudflare Tunnel is awkward (restrict to APIs + low quota).

---

## Open questions / decisions needed before building

1. **Zoho plan** — does it include IMAP/SMTP (free plans no longer do)?
2. **Google Calendar** — willing to create an OAuth client + publish the consent
   screen ("In production") for a personal app?
3. **Google Maps** — willing to enable billing + create a restricted key? (OSM
   works with zero config as the default.)
4. **Image gen (3.1)** — is a self-hosted ComfyUI on the Proxmox box wanted, and
   should we run the SD-Turbo timing gate? (Otherwise stays de-scoped.)
5. **Serper** — do you have/want a Serper key? (Brave/DDG already work; Serper is
   additive.)
