# Phase 4 · 2 — Showing attachments in the chat bubble

**Question:** the owner attaches a file; the model uses it correctly; but the user
message only renders `"1 file attached"`. Make the attachment *visible* — name,
type, size, and (when possible) the actual content.

**Owner complaint (verbatim intent):** *"I attached a file, it was used correctly,
but the bubble only shows '1 attachment' and nothing else. I want the attachment
actually shown."*

**Verdict (TL;DR).** The display metadata needed for a rich chip is **already
persisted and round-tripped** — no storage change is required to render a
name/type/size chip + a download link *target*. But the **original bytes are not
stored anywhere**: the `attachments` table holds only extracted text, and the
existing `GET /api/attachments/{id}` returns **JSON metadata, not bytes**. So:

- **Chip + preview UI:** pure frontend work, ~0.5 day. No schema change.
- **Actual download/preview of the original file:** requires a small **additive
  migration + upload change + a byte-serving endpoint**, ~1–2 days.
- Retention means any chip can outlive its bytes — the UI must treat a `404` as a
  normal, expected state ("expired"), not an error.

Everything below is verified against the repo at `HEAD` (Oct 2026).

---

## 1. Current data flow (verified)

### 1.1 Upload path

`internal/server/attachments.go`

- `handleUploadAttachment` (`:32`) reads the whole body (`io.ReadAll`,
  `:64`), extracts text (`:81`), then **discards `data`** — only metadata + text
  are stored (`:87-95`):

```go
87: 	att := store.Attachment{
88: 		ID:        newUUID(),
89: 		UserID:    u.ID,
90: 		Name:      name,
91: 		Mime:      mimeType,
92: 		Size:      int64(len(data)),   // original byte length — kept
93: 		Text:      text,               // extracted plain text only
94: 		CreatedAt: time.Now().UnixMilli(),
95: 	}
```

The `201` response (`:101-107`) returns `{id,name,mime,size,chars}`.

### 1.2 Storage

`internal/store/store.go` — schema (`:73-81`):

```sql
CREATE TABLE IF NOT EXISTS attachments (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  mime       TEXT,
  size       INTEGER NOT NULL,
  text       TEXT NOT NULL,      -- extracted text, NOT the original bytes
  created_at INTEGER NOT NULL
);
```

`store.Attachment` (`:247-257`) confirms it: `Text string json:"-"` is the only
payload; there is **no `data`/`blob` field**.

> The `images` table (`store.go:132-144`, with `data BLOB`) is for **generated
> images only** (`handleCreateImage`, `internal/server/images.go`). It is *not*
> wired to uploads and must not be reused for them — different provenance,
> different display semantics.

### 1.3 Message display metadata

`store.AttachedFile` (`store.go:209-218`):

```go
type AttachedFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Mime string `json:"mime,omitempty"`
	Size int64  `json:"size"`
}
```

Projected at send time by `attachmentMeta(atts)` (`attachments.go:199-208`) and
written to `messages.attachments` as JSON (`chat.go:107`). The extracted text is
folded into the upstream request only at send time (`foldAttachments`,
`attachments.go:212`; `chat.go:208`) and is never re-shown in the UI.

**Conclusion:** `messages.attachments` already carries `id`, `name`, `mime`,
`size` — more than enough to render a chip and build a download URL. It does
**not** carry bytes or an inline preview.

---

## 2. Is there a download/read endpoint for the original bytes?

**No.** The route table (`internal/server/server.go:147-150`) has:

```
POST   /api/attachments         → upload (extract + store)
GET    /api/attachments/{id}    → JSON metadata  ← NOT bytes
DELETE /api/attachments/{id}    → 204
```

`handleGetAttachment` (`attachments.go:110-134`) returns JSON only:

```go
126: 	writeJSON(w, http.StatusOK, map[string]any{
127: 		"id":         att.ID,
128: 		"name":       att.Name,
129: 		"mime":       att.Mime,
130: 		"size":       att.Size,
131: 		"chars":      att.Chars(),
132: 		"created_at": att.CreatedAt,
133: 	})
```

Contrast with generated images, which *do* serve bytes owner-scoped
(`images.go:231-258`, `Content-Type` + `Content-Length` + a long cache header
`imageCacheControl = "private, max-age=31536000, immutable"`, `images.go:25-27`).

Because the bytes are dropped at upload, **there is currently nothing to serve.**
To display/download the original, all three layers must change.

---

## 3. What it takes to make originals displayable/downloadable

Mirror the `images` table pattern (already proven in this codebase). All additive;
safe on existing DBs via `Store.migrate()` (`store.go:344-363`).

### 3.1 Schema (additive)

Add a nullable BLOB column so **existing rows keep `NULL`** (legacy/purged):

```sql
-- one step appended to steps in store.go migrate():
{"attachments", "data", `ALTER TABLE attachments ADD COLUMN data BLOB`}
```

Keep `size` exactly as-is (already `int64(len(data))`), so no backfill is needed
for size. Existing rows simply have no bytes to serve.

> BLOBs in SQLite are fine at personal scale, matching the `images` decision
> (`docs/research/3.1-imagegen-recheck.md` §8c: "BLOBs are fine at personal
> scale"). One caveat: `db.SetMaxOpenConns(1)` (`store.go:330`) means every byte
> read serializes behind the write connection — irrelevant at single-user volume.
> Watch DB growth: 10 MB × many attachments can bloat the Pi's SQLite file and
> WAL. Consider keeping the `maxAttachmentBytes = 10 << 20` cap (`:18`) and, if
> growth matters, a lower `MYOGCHAT_ATTACHMENT_RETENTION` (see §5).

### 3.2 Store CRUD

- `attachmentCols` (`store.go:1196`) must gain `data`; `scanAttachment`
  (`:1198-1209`) must scan it into `a.Data` (use `[]byte`, no `NullString`).
- `InsertAttachment` (`:1212-1217`) must bind `a.Data`.
- `GetAttachment` (`:1221-1228`) stays owner-guarded (`WHERE id = ? AND
  user_id = ?` → `ErrNotFound`) — reuse verbatim for the bytes endpoint.
- Add a `Data []byte json:"-"` field to `store.Attachment` (`:247-257`).
- The upload handler (`attachments.go:87-95`) sets `Data: data`.

### 3.3 Endpoint

Do **not** repurpose `GET /api/attachments/{id}` (it is a documented JSON
contract; `API_CONTRACT.md:281-284`). Add a bytes route:

```
GET /api/attachments/{id}/content   →  200 bytes, owner only
```

Handler sketch (mirrors `handleGetImage`, `images.go:231-258`):

```go
// owner-only, ErrNotFound → 404
a, err := s.st.GetAttachment(ctx, r.PathValue("id"), u.ID)
if len(a.Data) == 0 {  // legacy row or purged bytes
    writeError(w, http.StatusNotFound, "attachment content unavailable")
    return
}
w.Header().Set("Content-Type", mimeOr(a.Mime, "application/octet-stream"))
w.Header().Set("X-Content-Type-Options", "nosniff")
w.Header().Set("Content-Length", strconv.Itoa(len(a.Data)))
if download {
    w.Header().Set("Content-Disposition", contentDisposition(a.Name))
}
w.Write(a.Data)
```

Key requirements:

| Concern | Handling |
|---|---|
| **Owner scoping** | reuse `GetAttachment`'s `WHERE ... AND user_id = ?`; cross-user → `404`, indistinguishable from missing (existing comment `store.go:1219-1220`). |
| **Content-Disposition** | `?download=1` (or a `.pdf`-style preview vs. `attachment`) sets `Content-Disposition: attachment; filename*=UTF-8''<pct-encoded>`, **sanitized** (strip CR/LF/`"`/control chars — header-injection guard). Default `inline` for previewable types (`image/*`, `application/pdf`, `text/*`). |
| **MIME safety** | `Content-Type` from stored `mime`, fall back to `application/octet-stream`; always `X-Content-Type-Options: nosniff` so a mislabelled `.html` can't execute in origin. Prefer serving `text/html` as `attachment`. |
| **Size caps** | none needed at read (bytes already ≤ 10 MB by upload cap). The endpoint is naturally bounded. |
| **Caching** | reuse `imageCacheControl`-style header only if bytes are immutable — they are (id is a UUID, bytes never change), but **do not** cache a 404 permanently; use `Cache-Control: private, no-store` for the unavailable case. |
| **Route ordering** | register `GET /api/attachments/{id}/content` — Go 1.22 mux prefers the longer pattern over `/{id}`, so both coexist cleanly. |

> Alternative considered: store the path on disk instead of a BLOB. Current app
> has no configurable data dir and is SQLite-only; the BLOB matches the existing
> `images` precedent and needs no new config. Recommend BLOB.

---

## 4. Proposed UI

**No build step, embedded assets only** (`//go:embed all:static`, per
`AGENTS.md`). Reuse existing helpers: `el()` (`app.js:367`), `enc()`
(`app.js:373`), `fmtBytes()` (`app.js:3533-3538`), the modal infra
`openModal/closeModal` (`app.js:745-765`) and the `.modal`/`.modal-card` CSS
(`style.css:861+`).

### 4.1 Replace the "N files attached" line

`buildUserMessage` (`app.js:2873-2885`) currently:

```js
2877:  const n = Array.isArray(m.attachments) ? m.attachments.length : 0;
2878:  if (n > 0) {
2879:    const note = el("div", "msg-attachments");
2880:    note.textContent = n + " file" + (n === 1 ? "" : "s") + " attached";
2881:    bubble.appendChild(note);
2882:  }
```

Replace with a `renderAttachmentChips(m.attachments)` call that appends a
`.msg-attachment-list` row under the bubble text. Keep a fallback to the old
"N files attached" text only if `m.attachments` is missing/empty.

### 4.2 Chip anatomy

Per attachment (`{id,name,mime,size}`):

- **file-type icon** — emoji or an inline SVG glyph keyed off mime, with an
  **extension fallback** because `Mime` is `omitempty` and may be generic/empty
  (`store.go:216`; extension maps already exist server-side in
  `extract.go:45-69` — mirror a small JS map).
- **name** — truncate with `text-overflow: ellipsis` (same idiom as
  `.attachment-chip-name`, `style.css:691-696`).
- **human size** — `fmtBytes(a.size)`.
- **click** — for images: open lightbox; for everything else: anchor to
  `/api/attachments/<id>/content?download=1`.
- Optional: a `chars`/fold indicator is not available on `messages.attachments`
  (only on the upload response), so omit it. If desired, it would need adding to
  `AttachedFile`.

### 4.3 Image thumbnails + lightbox

- Detect images by `mime.startsWith("image/")`, else by extension (`.png .jpg
  .jpeg .gif .webp .bmp`). Since OCR may have been used, `mime` is preserved
  from the upload header (`attachments.go:91`).
- Thumbnail: `<img src="/api/attachments/<id>/content" loading="lazy"
  class="msg-attachment-thumb">`, constrained by CSS `max-height` (reuse
  `.msg-image` sizing, `style.css:774-782`). This downloads the full ≤10 MB
  original — acceptable at personal scale, but see §6 (a small server-side
  downscale endpoint via stdlib `image/png`/`image/jpeg` is the cheap follow-up
  if mobile data matters).
- Lightbox: a hidden `.modal` element (like `#project-modal`,
  `index.html:764`) containing full-size `<img>`, opened with `openModal(node)`,
  closed on backdrop/Esc via existing handlers (`App` global key handler —
  search `Escape` handling) — self-contained, no vendor lib.
- Generated images already open in a new tab (`buildImagesBlock`,
  `app.js:2983-3000`); a lightbox is a nicer upgrade but keep the two paths
  visually consistent.

### 4.4 Graceful handling of missing/purged originals

- Chip always renders from persisted metadata (never blocks on the network).
- For image thumbs: set `img.onerror = () => chip.classList.add("expired")` and
  swap to a "file expired" glyph + tooltip. Do **not** show a broken-image icon.
- For download links: never pre-check; treat the endpoint's `404` on click as
  "this file has expired" (the chip is already marked expired after a failed
  thumbnail `HEAD`/`GET`, or optimistically via `onerror`).
- CSS: a muted `.expired` variant (mirror `.attachment-chip.uploading`
  opacity idiom, `style.css:689`) with `pointer-events: none` on the link.

### 4.5 CSS addition (new, alongside `.msg-attachments`)

Existing `.msg-attachments` (`style.css:790-794`) is just a dim line. Add a
`.msg-attachment-list` (flex-wrap row) and `.msg-attachment-chip` — reuse
`.attachment-chip` look (`style.css:677-713`) but drop the `.attachment-remove`
control (sent attachments are immutable). Keep mobile-first: full-width wrap,
`max-width: 100%`, tap targets ≥ 40 px.

---

## 5. Retention implications

The janitor purges attachments on the same retention window as conversations
(`main.go:809-851`):

```go
820: 		// Attachments share the retention window; a purged attachment is
821: 		// silently skipped when an old message is regenerated (2.3).
822: 		if a, err := st.DeleteAttachmentsOlderThan(ctx, cutoff); err != nil {
```

`DeleteAttachmentsOlderThan` (`store.go:1242-1248`) deletes by `created_at <
cutoff`. Two consequences:

1. **The message JSON outlives the row.** `messages.attachments` is plain JSON
   with no FK to `attachments`, so a purged attachment gives a chip pointing at
   a gone file, while the message stays (until the conversation itself is
   purged, `DeleteOlderThan`, `main.go:812`). This is already true for the
   regenerate path (`chat.go:346-349`, "Missing attachments … are skipped").
2. **Bytes and text are purged together** once the byte column exists, so a
   purged file is gone at every layer — the `404` is the entire signal.

**Recommended handling:** the UI is designed around `404` = expected (§4.4).
Optionally, the sender could store a compact `chars`/`bytes_stored` flag in
`AttachedFile` to grey out chips proactively, but this is not required and would
widen the JSON contract. No server-side change is needed for correctness.

**Design note:** if the owner wants attachments to persist longer than chat
retention (e.g. to keep download links alive), that needs a **separate retention
knob** for attachments and a decision to *not* cascade-delete on conversation
purge. Out of scope for the display fix; flag it explicitly if requested.

---

## 6. Tests

There are **no server-level attachment endpoint tests today** (verified: no
`handleGetAttachment`/`/api/attachments` references in `internal/server/*_test.go`;
store coverage lives in `store_test.go:436-490`).

| Layer | Test | Where |
|---|---|---|
| Store round-trip | extend `TestAttachmentsScopingAndMessageMetadata` (`store_test.go:436`) to include `Data: []byte("...")` and assert `GetAttachment(...).Data` matches; add a case inserting `Data: nil` (legacy) and assert empty read. | `internal/store/store_test.go` |
| Store migration | assert the new `attachments.data` column exists after `migrate()` (mirror the column check at `store_test.go:93`). | `internal/store/store_test.go` |
| Endpoint auth/scoping | new `attachments_test.go`: owner `200` + `Content-Type` + `X-Content-Type-Options`; non-owner `404`; missing id `404`; legacy/no-bytes row `404`; `?download=1` sets a sanitized `Content-Disposition` (include a filename with a quote/newline to prove sanitization). Mirror `images_test.go:212-233` (Alice/Bob scoping) and `:162-196` (status/content-type). | `internal/server/attachments_test.go` |
| UI | no JS test harness exists (vanilla SPA, no build step). Verify manually: chip renders from metadata, image thumb + lightbox, `onerror` → expired state, download link works, and a purged attachment shows expired rather than a broken image. | manual |

The `migrate()` step must be exercised against a DB created with the *old*
schema (the store test suite already recreates DBs fresh; add an explicit
"old schema then migrate" case if you want to guard the additive path — see the
existing migration coverage pattern around `store_test.go:93`).

---

## 7. Effort estimate

| Workstream | Estimate |
|---|---|
| Store: migration + `data` field + CRUD plumbing | 0.5 day |
| Server: upload keeps bytes + `/content` endpoint (scoping, disposition, nosniff) | 0.5 day |
| Frontend: chips, icons, size, image thumbs, lightbox, expired state, CSS | 0.5–1 day |
| Tests (store + endpoint) | 0.5 day |
| **Total** | **~2–2.5 days** |

A **chip-only** version (no bytes; chips render name/type/size + a disabled or
"expired" download affordance) is **~0.5 day** and could ship first if the owner
just wants the attachment *visible* immediately, with the byte endpoint as a
fast follow. Given the complaint explicitly says *"I want the attachment
actually shown"*, the full path (real preview/download) is the right target.

---

## 8. Risks & mitigations

| Risk | Mitigation |
|---|---|
| **DB bloat / WAL growth** on the Pi (10 MB BLOB × many files, single write conn `store.go:330`) | keep the 10 MB cap; monitor DB size; offer a separate attachment retention window; document that bytes share the existing janitor window (`main.go:820-826`). |
| **Header injection via filename** in `Content-Disposition` | sanitize (strip CR/LF/control/`"`), use RFC 5987 `filename*=UTF-8''…`. |
| **Stored-XSS via mislabelled upload** (`.html` / SVG with `image/svg+xml`) | `X-Content-Type-Options: nosniff`; serve HTML/SVG as `attachment`, not `inline`; sanitize displayed names (use `textContent`, never `innerHTML`). |
| **Broken-image UX after purge/legacy rows** | `onerror` → expired chip; endpoint `404` is treated as normal (§4.4). |
| **Memory on mobile** for full-size thumbnails | CSS `max-height` + `loading="lazy"`; optional stdlib downscale endpoint later (§4.3). |
| **Contract drift** with `GET /api/attachments/{id}` | keep it JSON; add a distinct `/content` route and document it in `API_CONTRACT.md` (§8.4 area, near `:281`). |
| **Multi-user scoping regression** | reuse `GetAttachment`'s owner guard; add the explicit cross-user `404` test (§6). |

---

## 9. Concrete change list (for the implementer)

1. `internal/store/store.go`
   - Append `{"attachments","data", `ALTER TABLE attachments ADD COLUMN data BLOB`}` to `migrate()` steps (`:344-363`).
   - Add `Data []byte \`json:"-"\`` to `store.Attachment` (`:247-257`).
   - Add `data` to `attachmentCols` (`:1196`) and the scan in `scanAttachment` (`:1198-1209`).
   - Bind `a.Data` in `InsertAttachment` (`:1212-1217`).
2. `internal/server/attachments.go`
   - Set `Data: data` in the upload handler (`:87-95`).
   - Add `handleGetAttachmentContent` (owner-scoped bytes; disposition; nosniff).
3. `internal/server/server.go` — register `GET /api/attachments/{id}/content` (`:147-150`).
4. `static/app.js` — replace the "N files attached" branch in `buildUserMessage` (`:2873-2885`) with chip rendering; add `renderAttachmentChips`, `attachmentIcon`, lightbox open/close; reuse `fmtBytes` (`:3533`), `enc` (`:373`), `openModal` (`:747`).
5. `static/style.css` — `.msg-attachment-list`, `.msg-attachment-chip`, `.expired`, `.msg-attachment-thumb`; lightbox reuses `.modal` (`:861+`).
6. `API_CONTRACT.md` — document `GET /api/attachments/{id}/content` and the `data` column near §8.4 (`:281`).
7. Tests: `internal/store/store_test.go` (extend `:436`), new `internal/server/attachments_test.go`.

---

## 10. Sources (repo, verified)

- `internal/store/store.go` — schema `:73-81`, `images` `:132-144`, `AttachedFile` `:209-218`, `Attachment` `:247-257`, `migrate` `:344-363`, attachment CRUD `:1194-1248`.
- `internal/server/attachments.go` — upload `:32-108`, `GET` metadata `:110-134`, delete `:137-153`, `loadAttachments` `:174-196`, `attachmentMeta` `:199-208`, `foldAttachments` `:212-258`.
- `internal/server/chat.go` — `handlePostMessage` `:56-116` (meta write `:107`), `attachmentsFromLastUser` `:346-379`.
- `internal/server/server.go` — routes `:147-150` (attachments), `:163-165` (images).
- `internal/server/images.go` — byte serving `:231-258`, cache header `:25-27`.
- `main.go` — janitor `:809-851` (attachment purge `:820-826`).
- `static/app.js` — `buildUserMessage` `:2873-2885`, `buildImagesBlock` `:2983-3000`, `fmtBytes` `:3533-3538`, pending `renderAttachments` `:3566-3594`, modal helpers `:745-765`, `el` `:367`, `enc` `:373`.
- `static/style.css` — `.attachment-chip` `:677-713`, `.msg-image(s)` `:768-782`, `.msg-attachments` `:790-794`, `.modal` `:861+`.
- `static/index.html` — `#project-modal` `:764-786`.
- `API_CONTRACT.md` — message JSON additions `:281-288`, OCR `:397-440`, image-gen binaries `:926-944`.
- `internal/store/store_test.go` — attachment test `:436-490`, migration column check `:93`.
- Prior art: `docs/research/3.1-imagegen-recheck.md` §8 (binary store + byte endpoint pattern).
