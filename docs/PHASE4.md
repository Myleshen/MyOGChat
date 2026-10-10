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

| # | Item | Status | Notes |
|---|---|---|---|
| 4.1 | **Collapsible tool calls + full persistence** | ✅ done | Persist tool args + output (+ error, duration, truncated) on `messages.tools` (JSON, size-capped 16 KB args / 64 KB output / 4 KB error, UTF-8-safe); one collapsible `<details>` per message, per-tool inner Input/Output. Retention is free (message cascade). |
| 4.2 | **Attachment chips in chat** | ✅ done | Render name/type/size chips under the sent user bubble using the metadata already on `messages.attachments`. No byte storage. |
| 4.3a | **Image prompt enhancement** | ✅ done | `POST /api/images/enhance` — LLM rewrite (reuses the caller's key) with a deterministic template fallback (`enhanced:false`); preview/edit modal before ComfyUI. |
| 4.3b | **`generate_image` model tool** | ✅ done | Mid-conversation generation with context over the 2.4 tool loop: the model calls `generate_image`, the bytes are stored owner-scoped and attached to the assistant message (`messages.images`). Registered only when tools are enabled + a ComfyUI backend is configured. |
| 4.4 | **Settings IA + UX overhaul** | ✅ done | Categorized into ~9 sections with desktop/tablet nav rail + mobile chip bar; admin web-search block relabelled "Server web search (admin)"; per-user Serper under Integrations → Web search. UI battery 45/45 on mobile. |
| 4.5a | **Document extraction** | ✅ done | `internal/extract` dispatcher: OOXML (`.docx/.xlsx/.pptx`), ODF (`.odt/.ods/.odp`), `.epub`, `.rtf` — stdlib only (no new deps). |
| 4.5b | **Media transcription** | ✅ done | Audio/video (`.mp3/.wav/.m4a/.ogg/.opus/.flac/.mp4/.mov/.webm/.mkv`) via the configured STT service. **Asynchronous**: upload returns `202 pending`, a bounded background worker writes the transcript, the SPA polls until ready (no tunnel-timeout limit). |

> **Status:** Phase 4 is complete — 4.1, 4.2, 4.3a, 4.3b, 4.4, 4.5a, 4.5b are all
> **done** (merged to `main` and deployed).

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
