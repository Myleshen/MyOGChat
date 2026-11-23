# Feature 5 — Export & backup, edit & resend, fork

Three independent, additive features. None changes the schema; all DB access is
read, in-place update or a new row set, so ordinary databases need no migration.

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

## C. Continue from here (fork)

Branch a chat at one message: the head of the transcript up to and including it
becomes a new conversation, and the source keeps everything.

### Store

- `ForkConversation(ctx, srcID, anchorID, dst, newID, maxMessages)` — checks the
  anchor belongs to `srcID`, reads the message prefix with `ListMessages`
  ordering (`created_at, rowid`), then writes the new conversation and its
  messages **in one transaction**, so a failed fork leaves nothing behind.
  Copies get fresh ids and keep the source `created_at`; `(created_at, rowid)`
  ordering inside the fork is therefore identical to the source's.
- An anchor outside `srcID` is `ErrNotFound`; a prefix above `maxMessages` is
  `ErrForkTooLarge` and writes nothing.
- `InsertConversation` / `InsertMessage` are now thin wrappers over
  `insertConversation` / `insertMessage`, which take any executor so the fork can
  reuse the exact same INSERT statements inside its transaction.

### Endpoint

`POST /api/conversations/{id}/fork` with `{"message_id":"<id>"}` → `201`
`{"id","title","message_count"}`; `404` for an unknown/cross-user conversation or
a message outside it; `413` `{"error":"conversation too large to fork"}` above
**5,000** copied messages; `400` on a missing `message_id` (`API_CONTRACT.md` §3).

- The fork keeps the source's `project_id`, `model`, `effort` and `agent_mode`
  and belongs to the same user; it is not pinned.
- Title is `<source title> — fork`, with the source part truncated to
  `200 - len(" — fork")` runes so the whole title still fits the `PATCH` cap. An
  untitled source becomes `Untitled — fork`.
- No upstream call, no title generation, no workspace.

### Attachments, artifacts, images

The message JSON references (`attachments`, `images`, `artifacts`) are copied
verbatim. Those rows are scoped to the **user**, not the conversation — an
artifact download is already owner-only — so the fork reads the same bytes with
no duplication and no cross-user exposure.

The one trade-off: an artifact row carries the source `conversation_id` and is
deleted with it, so deleting the source chat makes the fork's artifact chips
`404` (attachments and images are unaffected). Upgrading to a shared-bytes table
is only worth it if forks must outlive their sources.

### Agent conversations

An agent fork gets a **fresh, empty workspace** (`sandbox/agent/<user>/<conv>`):
nothing is copied from the source's files. The copied transcript is context, not
files, so a forked agent starts from a clean disk like a brand-new chat.

### UI (separate pass)

A per-message **Continue from here** action POSTs the fork and switches to the
returned `id`, so the user lands in the new chat with the copied transcript
loaded. Edited/deleted messages in either chat never touch the other.

## Files

- `internal/store/store.go` — `EditUserMessage`, `TruncateAfter`, `Backup`,
  `ForkConversation`.
- `internal/server/export.go`, `internal/server/edit.go`, `internal/server/fork.go`
  — handlers.
- `internal/server/server.go` — routes.
- `internal/store/store_test.go`, `internal/server/export_test.go`,
  `internal/server/edit_test.go`, `internal/server/fork_test.go` — tests.
- `static/index.html`, `static/app.js`, `static/style.css` — UI.
