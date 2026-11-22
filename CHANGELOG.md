# Changelog

All notable changes to MyOGChat. Releases are tagged on `main`; each one ships a
`linux/arm64` binary plus digest-pinned sandbox images (see
[`docs/6.1-release-ci.md`](docs/6.1-release-ci.md)).

## Unreleased

Nothing since v0.2.1.

## [v0.2.1] - 2026-11-22

### Fixed

- Release workflow no longer prunes GHCR image versions. Pruning ran
  `actions/delete-package-versions` with `min-versions-to-keep: 1`, but every
  push tags an image twice (`<tag>` and `latest`) under one OCI index, so
  deleting a version took the shared child manifest blobs with it and the
  surviving index returned 404 on every pull. That is what broke the v0.2.0
  images. Every version is kept now (the packages are public, so pruning saved
  nothing), so an older release's `images.txt` digests stay pullable.

## [v0.2.0] - 2026-11-22

### Added

- **Project memory** ([`docs/6.2-project-memory.md`](docs/6.2-project-memory.md)):
  a per-project `memory` plus `memory_auto` toggle, learned from successful
  replies in any of the project's chats by one non-streaming call
  (`mimo-v2.6-flash`, 4000 tokens) off the reply's critical path, then injected
  as a `## Project memory` block. 32000-rune cap (~8k input tokens per turn at
  the ceiling); compare-and-set write, so a user edit is never clobbered.
- **Chat references** ([`docs/6.3-chat-references.md`](docs/6.3-chat-references.md)):
  read-only `list_chats` and `read_chat` tools, registered whenever tools are
  enabled. `read_chat` pages 40 messages (100 max) under a 200,000-rune page
  budget and puts the next `offset` in the footer. Owner-scoped; no schema
  change.
- **Anthropic Messages and Google generateContent dialects**
  ([`docs/6.4-upstream-dialects.md`](docs/6.4-upstream-dialects.md)), normalized
  into the same event stream and tool loop as chat/responses. Seven registry
  models now run on them: Claude Haiku 5.5, MiniMax M3, Qwen3.7 Plus, Qwen3.8
  Flash, Qwen3.8 Max and Qwen4 Max (Anthropic), Gemini 4 Argon (Google). The
  registry is 28 models; `hy4`, `longcat-2.5-free` and `hy4-flash-free` were
  added and `glm-5.2` retired (still loadable through `models.legacy`).
- **Custom providers** ([`docs/6.5-custom-providers.md`](docs/6.5-custom-providers.md)):
  per-user OpenAI-compatible endpoints (chat-completions only), models selected
  as `<slug>/<vendor-id>`, one AES-256-GCM-encrypted key per provider that is
  never returned. Third-party prices are unknown, so replies report
  `cost_tracked:false` while tokens still count toward usage and quotas.
- **Provider editing**: `PATCH /api/me/providers/{id}` changes name, base URL or
  key independently — omit the key to keep the current one. A slug never changes,
  so chats pinned to `<slug>/<model>` survive a rename.
- Collapsed the secondary composer actions into a phone menu (PR #6).

### Changed

- A custom-provider call fails after a **60 s first-byte timeout** instead of
  hanging; the error names the model and points at another one. OpenCode turns
  are unaffected.
- Auto-titles run on a cheap model with the caller's own key, in parallel with
  the reply and with a larger budget, retrying once when a reasoning model
  spends the whole first budget thinking and then falling back to the truncated
  prompt.
- Sandbox Python base is `python:3.15-slim` (3.15.2, Debian 13; 712 → 729 MB) in
  the Dockerfile, setup script, `run_python` tool description and docs.
- Responses dialect takes the shared turn budget in `max_output_tokens` (was a
  hard-coded 4096); Google maps the effort selector onto
  `thinkingConfig.thinkingBudget`.
- Dependabot for Go modules, Actions and the sandbox images (PR #17), plus
  `modernc.org/sqlite` 1.60.1 → 1.62.0, `go-imap/v2` beta.8 → beta.9,
  `actions/checkout` 5.1.0 → 7.0.1, `actions/setup-go` 6.5.0 → 7.1.0.
- Corrected the docs: groups are user-owned, not admin-gated (PR #25, behaviour
  unchanged), and Google enforces thought signatures only on the current turn.

### Fixed

- Gemini `thoughtSignature` is captured per `functionCall`, persisted in
  `messages.tools[].thought_signature` and echoed on replay, so a reloaded
  conversation can still use tools.
- A dropped stream no longer loses the reply: the SPA reattaches to a running
  generation, and a failed reply job persists the agent work instead of
  discarding it.
- Deploys now reach an installed PWA (content-hash shell cache-busting plus a
  versioned service worker).
- Auto-titles survive a reasoning model that spends the budget thinking or
  returns no text at all.

### Known issue

- **The v0.2.0 sandbox image digests are permanently unpullable — do not use its
  `images.txt`.** Both index manifests still resolve, but every child manifest
  they reference returns `404` (re-checked 2026-11-22), so `docker pull` fails.
  The binary asset and `SHA256SUMS` are fine. To roll back to this release,
  install **v0.2.1** instead — same app code plus the workflow fix, with working
  images. Do not re-run the v0.2.0 workflow to "repair" it: a re-run executes the
  same commit's workflow file, which is the one that prunes. A repair needs a new
  patch tag on a commit after the fix (that is what v0.2.1 is).

## [v0.1.0] - 2026-11-21

The first tag: the whole app through Phase 5 (summarized below), plus:

### Added

- **Agent mode**, opt-in per conversation: a 60-minute / 60-step job, a
  persistent per-user/per-conversation 5 GiB sandbox workspace with a 7-day idle
  sweep, one job per user behind a FIFO queue (`202 {"queued":true,...}`), and a
  partial reply persisted on stop.
- A **dev toolbelt** in both sandbox images (git, curl, ripgrep, jq, pdftotext,
  sqlite3, imagemagick, …) driven by `deploy/sandbox/tools.list`.
- **Tag-only release pipeline**: `release.yml` gates on `vMAJOR.MINOR.PATCH`,
  runs the tests, publishes `myogchat-linux-arm64` + `SHA256SUMS` with build
  provenance, and builds both sandbox images on a native arm64 runner, recording
  `images.txt`; `deploy/deploy-release.sh` installs on the Pi by digest, with no
  Go toolchain and no checkout.
- DeepSeek's leaked `｜DSML｜` tool markup is parsed into real tool calls, and
  assistant tool rounds are replayed in both dialects; a calendar's stored
  default collection is honoured, with a picker in Settings.

### Fixed

- Findings from live end-to-end testing on the Pi: Docker `--ulimit` `fsize` takes
  plain bytes (agent runs had failed with exit 125), containers run as the
  service's own uid:gid so a written directory can still be deleted, deleting a
  conversation removes its workspace, reasoning from separate tool rounds is
  joined correctly, a cancelled upload returns 499, and long unbroken text no
  longer stretches the thread on mobile.

## Before v0.1.0

Phase 1–5, one line each (status per [`TODO.md`](TODO.md)):

- **Phase 1 — foundation.** One Go binary (`net/http` + `modernc.org/sqlite`,
  WAL) serving an embedded PWA; SSE chat with markdown, thinking, cost,
  tokens/sec, regenerate and auto-titles; projects; themes; PBKDF2 multi-user
  auth with approval-gated registration; the visual-viewport mobile keyboard fix;
  systemd user service behind a Cloudflare Tunnel.
- **Phase 2 — integrations (2.1–2.13).** BYOK keys, groups and shared keys,
  file/PDF attachments, the agentic tool loop with web search (Tavily → Brave →
  Serper → DuckDuckGo, keyless by default), URL fetch and Linkwarden, Docker OCR
  (Tesseract and a vision-model engine), Python/bash sandboxes, the full settings
  page, persisted tool activity, usage and quotas.
- **Phase 3 — providers.** Self-hosted ComfyUI image generation, read-only IMAP
  email, CalDAV/ICS calendar, keyless OSM maps, whisper.cpp STT and Piper TTS.
- **Phase 4 — UX.** Collapsible tool calls with persisted args/output, attachment
  chips, image-prompt enhancement and the `generate_image` tool, categorized
  settings, OOXML/ODF/EPUB/RTF extraction, asynchronous media transcription.
- **Phase 5 — hardening and agents.** Vision image input, chat search, export and
  admin backup, edit-and-resend, pinned chats, background generation, sandbox
  artifacts, email digest, draft reuse, multiple mailboxes, an audit-driven
  hardening pass, and agent mode.

[v0.2.1]: https://github.com/Myleshen/MyOGChat/compare/v0.2.0...v0.2.1
[v0.2.0]: https://github.com/Myleshen/MyOGChat/compare/v0.1.0...v0.2.0
[v0.1.0]: https://github.com/Myleshen/MyOGChat/commits/v0.1.0
