# Feature 5 — Export & backup, edit & resend

Two independent, additive features. Neither changes the schema; all DB access is
read or in-place update, so ordinary databases need no migration.

## A. Export & backup

### Conversation export

`GET /api/conversations/{id}/export?format=md|json` (session required, owner-scoped)

- `format` defaults to `md`. Any other value is `400`.
- **md** — a Markdown transcript: a `# <title>` header (or "Untitled
  conversation"), a metadata block (model / created / updated / message count),
  then each message as `## You` or `## Assistant` followed by its content and a
  footer line `_<model> · <UTC time> · <cost>_` (fields shown only where
  present; zero cost renders as `Free`). A single message's content is capped at
  100 KB (UTF-8-safe, cut marked with `… [truncated]`).
- **json** — exactly the shape of `GET /api/conversations/{id}`:
  `{…conversation…, messages:[…]}`.
- The handler sets `Content-Disposition: attachment; filename="<slug>-<id>.<ext>"`.
  The slug is derived from the title (lowercase `a-z0-9`, dashes, ≤60 chars,
  `chat` when empty). Nothing untrusted reaches the header.
- Ownership follows every other conversation route: a non-owner gets `404`.

### Admin database backup

`GET /api/admin/backup` (admin session required)

- `403` for non-admins.
- The handler runs `VACUUM INTO '<tempfile>'` through the new store method
  `Backup(ctx, destPath)` — a **consistent** snapshot of the live DB (including
  WAL content) — then streams the file with
  `Content-Disposition: attachment; filename="myogchat-backup-<date>.db"` and
  `Content-Type: application/octet-stream`, and removes the temp file.
- `VACUUM INTO` refuses to overwrite an existing file, so the handler reserves a
  unique name with `os.CreateTemp`, removes the empty placeholder, and snapshots
  into it.

### UI

- The conversation header gains an overflow ("…") button (`#export-btn`, shown
  only while a conversation is open). It opens the shared popup menu with
  **Export Markdown** / **Export JSON**, which trigger a normal browser download
  (the cookie authenticates the request).
- Settings → **About** gains a **Download backup** button (`#download-backup`,
  admins only) that downloads `GET /api/admin/backup`.

## B. Edit & resend

### Store

- `EditUserMessage(ctx, convID, msgID, userID, content)` — updates
  `messages.content` **only** when the row is in `convID`, that conversation is
  owned by `userID`, and the row's `role = 'user'`. Any other match returns
  `ErrNotFound`, so a cross-user or non-user target is indistinguishable from a
  missing row.
- `TruncateAfter(ctx, convID, msgID)` — deletes every message after `msgID` in
  the conversation, ordered by `(created_at, rowid)` (the same order
  `ListMessages` uses). A missing anchor is a no-op.

### Endpoint

`POST /api/conversations/{id}/messages/{messageId}/edit`

- Body `{"content": string}`; non-empty, ≤ 100 KB (same limit as sending).
- Owner-scoped: `404` for an unknown conversation, a message that is not the
  caller's, or one whose role is not `user`. `400` on empty/oversized content.
- On success: edit the row, truncate everything after it, and return
  `200 {"ok": true}`. The client then calls the existing
  `POST /api/conversations/{id}/regenerate` to stream the replacement reply —
  the two-step design reuses the SSE path unchanged.

### UI

Each user message bubble gains an **Edit** action (alongside the existing Copy /
Retry patterns). Edit swaps the bubble text for a prefilled inline textarea with
**Save** / **Cancel** (Esc also cancels). Save POSTs the edit, drops the local
messages after the edited one, re-renders, and calls `regenerate()`.

## Files

- `internal/store/store.go` — `EditUserMessage`, `TruncateAfter`, `Backup`.
- `internal/server/export.go`, `internal/server/edit.go` — handlers.
- `internal/server/server.go` — three routes.
- `internal/store/store_test.go`, `internal/server/export_test.go`,
  `internal/server/edit_test.go` — tests.
- `static/index.html`, `static/app.js`, `static/style.css` — UI.
