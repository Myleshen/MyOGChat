# Phase 5 — hardening (audit findings)

From a four-agent read-only audit of the merged Phase-5 code (5.1/5.3/5.4/5.6)
and the search/pin workstream. Ordered by severity. **No items implemented yet.**

## Must fix

1. **Edit is two non-atomic statements** — `internal/server/edit.go` commits
   `EditUserMessage`, then `TruncateAfter` as a separate `Exec`. A failure between
   them leaves the edited message with its stale assistant tail. → add a single
   transaction (`EditUserMessageAndTruncate`).
2. **Backup temp file is world-readable** — `internal/server/export.go` lets
   SQLite create the snapshot with `0644 & ~umask`; the snapshot holds every
   user's hash + encrypted secrets. Safe only because the unit sets
   `PrivateTmp=true`. → `os.MkdirTemp` (0700) + `VACUUM INTO <dir>/backup.db`, or
   `os.Chmod(tmp, 0600)` after `Backup`.
3. **Vision image caps validated for every model** — `handlePostMessage` calls
   `cleanVisionImages` before looking at the model, so a **non-vision** model with
   5 images (or >16 MB total) now returns `400` where it used to fold OCR text.
   → look up the model first; when `!Vision`, ignore `images`; and gate the SPA
   (`body.images` only when `modelById(conv.model).vision`).
4. **Client allows 20 attachments but vision caps at 4** → the whole send fails on
   the 5th image. → cap image attachments client-side (4) with a clear toast and a
   cumulative-size budget, so the send never 400s.
5. **Image-only send is impossible** — `content` is required and the Send button
   is disabled on empty input, so "what's in this image?" cannot be asked. →
   allow `content == ""` when images are present (server + SPA).
6. **`PATCH /api/conversations/{id}` silently no-oped** (owner guard compared
   `user_id` against NULL). **FIXED** by the search/pin workstream (loads
   `user_id` in `conversationCols`/`scanConversation`) + regression test.

## Should fix

7. **`isImageDataURI` too loose** — accepts empty payloads and `image/svg+xml`.
   → require a non-empty base64 body and restrict to raster types.
8. **Per-image cap (8 MB URI) < upload cap (10 MB raw)** — a file the attachment
   API accepted can be un-sendable. → align the caps and make the client
   downscale/loop until under target (or skip the inline bytes and warn).
9. **Markdown export does not escape/fence content** — a title/message containing
   `## Assistant` or `---` spoofs the transcript; raw HTML passes through. →
   normalize the title to one line and fence message bodies.
10. **`downloadUrl` blindly downloads error bodies** — an expired session saves a
    JSON error as the file. → fetch, check `res.ok`, then download; 401 → re-login.
11. **Export menu a11y + Esc** — `aria-expanded` is never toggled and the menu has
    no arrow-key roving focus; the inline-edit Esc handler only fires while the
    textarea is focused (the doc promises "Esc cancels"). → fix both.
12. **Edit stores untrimmed content** while validating the trimmed value. →
    persist `strings.TrimSpace`.
13. **`usage` nil-deref** — `streamReply` assumes a usage event arrived. →
    nil-check before building the assistant row.
14. **Backup is synchronous in the request + temp files leak on crash** — a large
    DB can exceed the tunnel timeout; SIGKILL leaves `myogchat-backup-*`. →
    single-flight guard + startup sweep (+ optionally size guard / async).

## Docs / hygiene

15. `README.md`, `AGENTS.md`, `deploy/env.example`, `deploy/deploy.sh` — the
    obsolete `MYOGCHAT_AUTH_TOKEN` is still listed as required; `OPENCODE_API_KEY`
    is now optional (BYOK). Fix.
16. `HANDOFF.md` — no Phase 5 status; stale "Deployed commit"; missing `5-*.md`
    in the docs index; still says media transcription is synchronous.
17. `TODO.md` — no Phase 5 section; 2.10/2.12 still list "static/sw.js cache bump".
18. **UI battery** (`scripts/ui-test.sh` / `ui-test-full.sh`) has **no Phase-5
    coverage** — add: export menu, admin backup row, edit & resend, image
    paste/thumb, chat search, pinned chats, and a "no horizontal overflow with a
    chat open" check.
19. Tap targets < 44px: `.msg-action` (28px), `.msg-edit-actions .btn` (32px),
    `.menu-item` (40px). Raise on touch.

## Verified clean

- No merge conflict markers; no duplicate function/const/DOM-id definitions; every
  `getElementById` target exists; every route pattern unique; migrations additive;
  API_CONTRACT headings unique. `go build/vet/test`, `node --check` all green.
- Vision wire rendering (both dialects, both paths) and the fold/drop/regenerate
  semantics are correct; export scoping (404), backup admin-only (403), the
  filename slug (ASCII, ≤60, `chat` fallback) and truncation ordering are correct.
