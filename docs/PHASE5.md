# Phase 5 — vision input, search, export, editing, pinning

> Phase 4 is complete and deployed. Phase 5 is the next batch (owner-approved).
> Parallel worktrees off `main`; UI items share `static/app.js` / `index.html` /
> `style.css`, so merges are resolved sequentially.

## Items

| # | Item | Notes |
|---|---|---|
| 5.1 | **Vision image input** | Attach a photo and let a vision-capable model actually *see* it (not just OCR-to-text). Images are sent inline with the message (no byte storage); the conversation model must declare `vision`; otherwise fall back to OCR. Also: **paste an image** straight into the composer (5.6). |
| 5.2 | **Search across chats** | `GET /api/search?q=` → titles + message snippets; a sidebar search box + results view. |
| 5.3 | **Export & backup** | `GET /api/conversations/{id}/export?format=md\|json`; admin `GET /api/admin/backup` (VACUUM INTO the live DB). |
| 5.4 | **Edit & resend a message** | Edit a user message, truncate the tail, regenerate from there. |
| 5.5 | **Pinned chats** | `conversations.pinned`; pinned sort to the top; a pin toggle in the sidebar. |
| 5.6 | **Paste image into composer** | Paste a screenshot → attaches it (implemented with 5.1). |

## Workstreams (parallel worktrees)

- `feat/p5-search-pin` — 5.2 + 5.5 (sidebar/store).
- `feat/p5-export-edit` — 5.3 + 5.4 (endpoints/message actions).
- `feat/p5-vision` — 5.1 + 5.6 (composer/upstream).

## Acceptance

- `go build/vet/test ./...`, `node --check static/app.js` green.
- Isolated UI battery (`scripts/ui-test-run.sh`) + tunnel battery green.
- New UI audited on phone / iPad / desktop.
