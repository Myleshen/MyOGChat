# Phase 4 — UX polish, tool depth, formats

> Phase 3 is **complete**. Phase 4 is the follow-up batch requested after live use.
> Research reports: `docs/research/phase4/`. This file is the plan of record;
> keep it updated as items land.

## Owner decisions

- **Quality bar:** bug-free, fast, excellent UX. No "quick-win, defer later"
  mindset — every item ships complete, and all UI is audited for **mobile,
  tablet (iPad), and desktop**.
- **Attachments:** show **file name / type / size metadata only** — do **not**
  store the original bytes (avoids redundant storage).
- **Image generation:** target the full flow — first **auto-enhance + preview &
  edit** the prompt (LLM rewrite before ComfyUI), then add the **`generate_image`
  model tool** so the assistant can generate mid-conversation with context.

## Items

| # | Item | Notes |
|---|---|---|
| 4.1 | **Collapsible tool calls + full persistence** | Persist tool args + output (+ error, duration) on `messages.tools`; one collapsible `<details>` per message, per-tool inner `<details>`. Retention is free (message cascade). |
| 4.2 | **Attachment chips in chat** | Render name/type/size chips under the sent user bubble using the metadata already on `messages.attachments`. No byte storage. |
| 4.3 | **Image prompt enhancement** | LLM rewrite (reuse `upstream.ChatOnce` + `resolveKey`) with preview/edit before ComfyUI; positive-prompt safety. Then `generate_image` tool. |
| 4.4 | **Settings IA + UX overhaul** | Merge the two "Web search" surfaces; categorize into ~9 sections with desktop/tablet nav rail + mobile drill-down; a11y + touch-target pass. |
| 4.5 | **File formats** | Dispatcher in `internal/extract`: OOXML/ODF/EPUB/RTF (stdlib), XLSX (stdlib-first); media (.mp3/.mp4/…) via a hardened ffmpeg container → whisper STT, **async**. |

## Order & parallelism

UI items (4.1, 4.2, 4.3, 4.4, 4.5) all touch `static/app.js` / `index.html` /
`style.css`, so **UI work lands sequentially**; backend-only chunks may run in
parallel worktrees and merge additively.

Recommended: 4.1 → 4.2 → 4.4 → 4.3 → 4.5 (tools/attachments first; the layout
overhaul last so it wraps the finished sections; media last as the heaviest).

## Acceptance criteria

- `go build ./...`, `go vet ./...`, `go test ./...`, `node --check static/app.js`
  all green; `sw.js` cache bumped per release.
- `scripts/ui-test-run.sh` passes; pwa battery green on the tunnel.
- Every new UI surface audited on phone / iPad / desktop.
- No secrets in tool outputs/logs; additive migrations only.
