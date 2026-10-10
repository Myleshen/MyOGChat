# Phase 4 — Settings page UX / IA audit (research only)

Status: **research + audit only — no code changed, nothing committed.**
Repo: `~/dev/myogchat`
Baseline inspected: `static/index.html`, `static/app.js`, `static/style.css`,
`internal/server/integrations.go`, `internal/server/linkwarden.go`,
`internal/server/users.go`, `internal/tools/search.go`,
`scripts/ui-test.sh`, `scripts/ui-test-run.sh`, `docs/2.9-settings-page.md`.

Method: static analysis (line-referenced). The live app was **not** running on
`:8787`/`:8791`, so no screenshots were taken; every claim below is traceable to a
file + line. Where a point is an estimate it is marked *(estimate)*.

---

## TL;DR

1. **The owner complaint is real, but it is not a data bug.** `GET /api/me/serper`
   returns the correct per-user status and `#my-search-status` renders it
   faithfully. The problem is **IA duplication + inconsistent status text**: for an
   admin the page shows **two** "Web search" blocks, and the admin block computes
   its status from **server-wide config only** (it ignores the caller's own Serper
   key). The two blocks can therefore contradict each other on the same screen —
   e.g. per-user block says *"Active provider: serper … your Serper key set"* while
   the admin block two sections lower says *"Active provider: duckduckgo …
   Serper: not configured"*. To the owner this reads as "my key isn't showing up".
2. There is also a **dead-end control**: the admin provider `<select>` offers
   `serper` but there is **no server-wide Serper key field** in the admin block
   (only Brave), so selecting it silently falls back to DuckDuckGo.
3. The page is **one ~4–7-screen vertical scroll** (non-admin → admin, estimate).
   This is the primary UX problem. Recommended IA + nav pattern in **Part B**.
4. The page scrolls an **inner container** (`.settings-body`), not the document —
   and there is **no scroll lock and no focus trap** on the fixed, z-index-60
   overlay, so keyboard/AT users can tab and screen-readers can reach the app
   behind it.

---

## Part A — Diagnosis of the owner complaint

### A.1 Where visibility is decided

| Concern | File / lines |
|---|---|
| Markup for all sections | `static/index.html:259-761` (`#settings-page` → `.settings-body:270`) |
| Role-based show/hide + initial loads | `static/app.js:807-834` (`showSettingsPage`) |
| Sidebar/admin visibility (badge, users, ocr, usage, voice) | `static/app.js:863-872` (`renderUser`) |
| Admin settings fetch + `apply*` | `static/app.js:1034-1060` (`loadSettingsInfo`) |
| Per-user Serper block loader | `static/app.js:1156-1179` (`loadMySearch`) |
| Per-user Linkwarden loader | `static/app.js:1691-1704` (`loadMyLinkwarden`) |
| `GET/PUT /api/me/serper` | `internal/server/integrations.go:377-421` |
| `GET/PUT/DELETE /api/me/linkwarden` | `internal/server/linkwarden.go:26-113` |
| Admin `GET/PATCH /api/settings` | `internal/server/users.go:236-439`; `writeSettings` 441+ |

`#my-search-section` (`index.html:355`) is **not** in any admin toggle list, so it
is visible to **all** users (as intended, comment at `index.html:354`).
`#integrations-section` (`index.html:469`) is toggled admin-only at
`app.js:813` / `1051`. **Confirmed: an admin sees both.**

### A.2 The duplication (confirmed)

`index.html:355-374` — per-user block:

```html
<section id="my-search-section" class="settings-section settings-search">
  <h2 class="settings-heading">Web search</h2>
  ...
  <p id="my-search-status" class="settings-subtitle-sm">Web search: loading…</p>
```

`index.html:469-510` — admin block (inside `#integrations-section`):

```html
<section id="integrations-section" class="settings-section settings-integrations" hidden>
  <h2 class="settings-heading">Integrations</h2>
  ...
  <div id="web-search-block" class="web-search-block">
    <p class="settings-subtitle settings-subtitle-sm">Web search</p>
    <p id="search-status" class="settings-subtitle-sm">Web search: loading…</p>
```

For an admin both render: an uppercase **WEB SEARCH** section and, lower down, an
**Integrations** section whose first sub-block is also labelled **Web search**.
Same label, two forms, two status lines.

### A.3 The status text is computed two different ways → they contradict

Per-user (`app.js:1156-1174`), from `GET /api/me/serper`:

```js
const s = await api("/api/me/serper");
const providers = [
  "Tavily: " + (s.tavily_configured ? "configured" : "not configured"),
  "Brave: "  + (s.brave_configured  ? "configured" : "not configured"),
  "Serper: " + (s.serper_configured ? "configured" : "not configured"),
  ...
];
const active = s.search_active ? s.search_active : "none";
const mine = s.has_key
  ? "your Serper key set" + (s.key_hint ? " (…" + s.key_hint + ")" : "")
  : s.server_key_present ? "using the server Serper key" : "no Serper key";
```

Admin (`app.js:1070-1082`), from `GET /api/settings`:

```js
function applySearchSettings(s) {
  ...
  const providers = [
    "Tavily: " + (s.tavily_configured ? "configured" : "not configured"),
    "Brave: "  + (s.brave_configured  ? "configured" : "not configured"),
    "Serper: " + (s.serper_configured ? "configured" : "not configured"),
    ...
  ];
  searchStatus.textContent = "Active provider: " + active + " · " + providers.join(" · ");
}
```

The two payloads are produced by different server paths:

- `GET /api/me/serper` → `handleGetMySerper` (integrations.go:377) uses
  **`searchConfigFor`** (integrations.go:67-73), which overlays the caller's own key:
  ```go
  func (s *Server) searchConfigFor(ctx context.Context, u store.User) tools.SearchConfig {
      cfg := s.searchConfig(ctx)
      if key, ok := s.userSerperKey(ctx, u); ok { cfg.SerperKey = key }
      return cfg
  }
  ```
- `GET /api/settings` → `integrationSettings` (integrations.go:341) uses plain
  **`searchConfig`** (integrations.go:43), **no per-user overlay**:
  ```go
  cfg := s.searchConfig(ctx)
  ...
  "serper_configured": strings.TrimSpace(cfg.SerperKey) != "",
  "search_active":     tools.SearchActive(cfg),
  ```

So for an admin whose *only* Serper key is their per-user key:
`cfg.SerperKey` in the admin payload is the **server env** key (`SearchDefaults.SerperKey`,
integrations.go:33) — usually empty → admin block prints **"Serper: not configured"**
and `SearchActive` (tools/search.go:170) skips Serper and reports **"duckduckgo"**.
The per-user block simultaneously prints **"Serper: configured … your Serper key set"**.

**That is the concrete defect behind the complaint: the same page reports two
different "active provider" and two different "Serper:" states for the same user.**

### A.4 Is `#my-search-status` actually inaccurate? No.

`handleGetMySerper` (integrations.go:382-397) returns `has_key` from the stored
ciphertext (`u.SerperKeyEnc != nil && *u.SerperKeyEnc != ""`), `key_hint` from a
successful decrypt, and `serper_configured`/`search_active` from `searchConfigFor`.
`loadMySearch` maps those 1:1. **The per-user block is faithful.** There is no
500, no null-ref, no wrong field. The bug is the **admin block under-reporting**
plus the visual duplication — i.e. an IA/consistency defect, not a lost key.

### A.5 Secondary defect — dead-end provider option

`index.html:485-491` offers `serper` in the admin provider select:

```html
<option value="serper">Serper (Google)</option>
```

but the admin block only has a **Brave** key input (`#search-brave-key`,
index.html:493-497). `PATCH /api/settings` accepts `brave_key` (users.go:261/368)
but **no** server-wide `serper_key`. So an admin can select Serper globally with no
way to give it a key through the UI (only env `SERPER_API_KEY`). Selecting it makes
the chain fall through to DuckDuckGo while the select still shows "Serper".

### A.6 Linkwarden / other "things" — no duplication

Linkwarden is now per-user only (`#linkwarden-section`, index.html:379). The old
admin Linkwarden block is gone, yet the admin section still references it: the
comment at `index.html:468` says "Phase 2.4 / 2.8" and the subtitle at
`index.html:471-472` says the model can "use Linkwarden" — neither control exists in
that section. Calendar (`#calendar-section`) and Email
(`#email-section`) are per-user. So the "other things" do render once, correctly;
only web search is duplicated.

### A.7 Scroll model & scroll-lock interaction (confirmed)

`style.css:987-1023` — the page is a **fixed overlay with an inner scroll container**:

```css
.settings-page { position: fixed; ... height: var(--app-h, 100dvh); overflow: hidden; }
.settings-header { flex-shrink: 0; ... }        /* effectively sticky */
.settings-body { flex: 1; min-height: 0; overflow-y: auto; overscroll-behavior: contain; }
```

- The **document does not scroll**; `.settings-body` does. `showSettingsPage` resets
  it with `body.scrollTop = 0` (app.js:819-820). This matches the UI test's
  `window.scrollY === 0` and "settings fits viewport" assertions.
- **No scroll lock is added** when settings opens. `body.modal-open { overflow:hidden }`
  (style.css:1251) is only applied by `openModal` (app.js:747-760), not by the
  settings route. In practice `.app` is also `position:fixed` (style.css:191-201)
  and `.settings-page` sits at `z-index:60`, so normal touch scrolling can't reach
  the chat behind — **but** `overscroll-behavior: contain` only stops scroll
  chaining; it does not stop *focus* or *AT cursor* from reaching the background.
- Worse for mobile keyboards: `.settings-page` follows the visual viewport via
  `--app-h`/`--app-top` (app.js:4484-4494). When the OS keyboard opens while an
  input deep in a long `.settings-body` is focused, the inner container shrinks;
  whether the focused field stays visible depends on the browser scrolling the
  *inner* container, which iOS does unreliably. This is a real risk to test on
  device (see Part C).

**No "bad interaction" today** (the surfaces don't fight), but there is **no lock**
and **no focus trap**, which the redesign should fix.

---

## Part B — Proposed information architecture

### B.1 Category grouping (order = recommended)

| # | Category | Sections folded in (existing ids) | Role |
|---|---|---|---|
| 1 | **Account** | `#account-section` | all |
| 2 | **Appearance** | `#appearance-section` | all |
| 3 | **AI & access** | `#key-section`, `#groups-section` | all |
| 4 | **Integrations** | `#my-search-section` (merged web search), `#linkwarden-section`, `#calendar-section`, `#email-section`, `#integrations-section` (admin: tools, maps, imagegen, web-search admin bits) | all (admin sub-parts) |
| 5 | **Voice** | `#voice-section` | admin |
| 6 | **Usage** | `#usage-section` (incl. `#usage-admin`) | all / admin |
| 7 | **Server** (admin) | `#tools-section`, `#ocr-section` | admin |
| 8 | **Users** | `#users-section` | admin |
| 9 | **About** | `#about-section` | all |

Notes:
- The owner's sketch put Voice/Usage/Admin separately; that maps cleanly here.
  `#tools-section` (sandbox) and `#ocr-section` are **server-wide machine config**,
  so a dedicated **Server** category reads better than burying them under
  "Integrations". `#usage-section` stays for everyone because it holds the user's
  own totals (`#usage-admin` is the admin-only tail inside it).
- **Web search must become ONE block.** Merge the per-user Serper form and the
  admin provider/Brave fields into a single section with role-aware rows:
  - always: status line (single source of truth), provider select (admin), Serper
    key field (per-user);
  - admin-only rows: Brave key + Brave mode, tools toggle.
  - See P0-1/P0-2 in Part D.

### B.2 Navigation pattern

| Breakpoint | Pattern |
|---|---|
| **Desktop ≥ 1024px** | Two-pane: left **nav rail** (sticky list of categories, icons + labels, active item highlighted) beside a scrollable pane showing the active category's sections. Sticky **search** field at the top of the rail. |
| **Tablet 768–1023px** (iPad portrait) | Same two-pane rail, narrower (~200px); optionally collapses to icon rail. Below 768px → mobile pattern. |
| **Mobile < 768px** | **Drill-down** (recommended) or accordion. Mobile shows the category **list** full-screen; tapping a category pushes `#/settings/<category>` and renders only that category (reusing the existing hash router, app.js:776-805). A back affordance returns to the list. Accordion is the fallback if we want to avoid a second history level. |

Shared across all breakpoints:
- **Sticky search** (`#settings-search`): filters/jumps to sections by label +
  keywords; keyboard-focusable, `role="searchbox"` via `<input type="search">`.
  Progressive enhancement — if empty/absent, all categories visible.
- **Title + breadcrumb**: `#settings-title` (index.html:268, currently static "Settings")
  becomes the breadcrumb: `Settings` → `Settings / Integrations`. Add
  `<nav aria-label="Breadcrumb">` with `aria-current="page"` on the last crumb.
- **Sticky header/back**: keep the current `.settings-header` (already effectively
  sticky); on mobile add the category name to the header and keep `#settings-back`
  (index.html:261) as the up-one-level control; Esc keeps working (app.js:4218-4233).

### B.3 Implementation constraints (merge-safety) — IMPORTANT

Keep **every existing element id** and **every `BEGIN/END` self-contained comment
block** (e.g. `index.html:353-375`, `377-404`, `479-511`, `512-538`, `539-562`,
`609-623`, `625-638`, `640-676`, `678-715`, `1519-1687` in app.js, etc.) untouched.
Recommended low-conflict approach:

1. **HTML**: add only *new* containers in the header:
   ```html
   <nav id="settings-nav" class="settings-nav" aria-label="Settings categories" hidden></nav>
   <form id="settings-search-form" class="settings-search" role="search" hidden>
     <input id="settings-search" type="search" placeholder="Search settings…"
            aria-label="Search settings" autocomplete="off">
   </form>
   ```
   and a breadcrumb slot. Do **not** wrap existing `<section>`s in new parents —
   that would break "safe to relocate".
2. **Categorisation is data, not markup**: a single new self-contained JS block
   holds `const SETTINGS_CATEGORIES = [{ id, label, icon, sections:[id,…] }, …]`.
   At runtime it stamps `data-settings-category` onto each section and renders the
   nav from the registry. Parallel workstreams can add a section + one registry row
   without touching anyone else's block.
3. **Show/hide vs navigation must not collide.** Role-based visibility stays on the
   `hidden` attribute (`usersSection.hidden`, etc.). Navigation uses a **separate**
   mechanism, e.g. `.settings-body[data-active-category="integrations"]
   .settings-section:not([data-settings-category="integrations"]) { display:none }`.
   `[hidden]{display:none!important}` (style.css:63) keeps role-hidden sections
   hidden regardless. This keeps `element.hidden` semantics (and existing tests)
   intact while only one category is *visually* shown.
4. **Hash sub-routes**: extend the router (app.js:776-805) with
   `#/settings/<category>` parsed in `showSettingsPage`; deep links and back/forward
   keep working with no new event wiring (hashchange already drives `applyRoute`).
5. **PWA cache**: `static/sw.js:5` is `myogchat-shell-v10` → bump to `v11` when the
   shell changes.

---

## Part C — Cross-device audit checklist

Legend: ✅ already OK · ⚠️ partial · ❌ missing/broken.

### C.1 Touch targets (min 44×44)

| Control | Size | Verdict | Source |
|---|---|---|---|
| `.btn` (Save/Clear/Connect/Log out…) | `min-height:44px` | ✅ | style.css:90 |
| `.icon-btn` (`#settings-back`) | 44×44 | ✅ | style.css:115-116 |
| `.field input/select` | `min-height:44px` | ✅ | style.css:972 |
| `.setting-toggle` (whole row = label) | `min-height:44px` | ✅ | style.css:957 |
| `.segmented span` (theme/thinking) | `min-height:40px` | ⚠️ 40 (4px short) | style.css:934 |
| `.user-action` (Approve/Disable…) | `min-height:32px` | ❌ | style.css:1114 |
| `.group-member-remove` | 28×28 | ❌ | style.css:1234 |
| `.menu-item` | `min-height:40px` | ⚠️ (menus, not settings) | style.css:1274 |

### C.2 Scroll depth

- Sections: 10 visible to a regular user, **15** to an admin (`index.html`).
  Each has multiple fields; on a Pixel 7 (412×915) that is roughly **4 screens
  (user) → 6–7 screens (admin)** *(estimate)*. Every admin then has to round-trip
  between the two web-search blocks (A.2) several screens apart.
- No jump/anchor nav, no sticky section index, no search. This is the core UX issue.

### C.3 Sticky header / back affordance

- `.settings-header` is `flex-shrink:0` outside the scroll container → effectively
  sticky; back button stays visible ✅ (style.css:1000-1011).
- Title is static; no per-section/per-category context ❌ (index.html:268).
- Deep-link/reload back behaviour is handled (app.js:786-799); browser/Android back
  works via hashchange (app.js:4520-4523) ✅.

### C.4 Focus management + trap

- On open, focus moves to `#settings-back` ✅ (app.js:833).
- **No focus trap** ❌: `Tab` (and AT virtual cursor) can move into the chat app
  behind the overlay, because the background DOM is never `inert`/`aria-hidden`.
- Escape closes (app.js:4218-4233) ✅, but it also closes sidebar/modals first, so it
  is context-dependent — fine, just document it.
- Return focus on close is implicit (the gear keeps focus) ✅-ish.

### C.5 Safe-area insets

- Header top ✅ (style.css:1006); body bottom ✅ (style.css:1022).
- **No left/right insets** ❌ — landscape on a notched phone can clip content
  (`env(safe-area-inset-left/right)` absent).
- iPad: OK.

### C.6 On-screen keyboard

- Layout pins to visual viewport via `--app-h`/`--app-top` (app.js:4484-4494) ✅.
- **Risk** ⚠️: `.settings-body` is an inner scroller inside a `position:fixed`
  overlay. iOS may not auto-scroll the inner container to keep the focused input
  above the keyboard. Must be verified on a real device; likely fix = scroll the
  focused field into view on `focusin`, or switch mobile to document scroll with a
  sticky header.

### C.7 Accessibility

| Item | Verdict | Notes |
|---|---|---|
| Heading order | ✅ | `h1#settings-title` → `h2.settings-heading`; but web-search/maps/imagegen sub-blocks use `<p class="settings-subtitle">` as pseudo-headings (index.html:481,514,541) — should be `h3`. |
| Landmarks | ❌ | No `<nav>`/`<aside>`/`role="region"` in settings; proposed nav needs `aria-label="Settings categories"`. |
| Sections labelled | ❌ | `<section>`s have no `aria-labelledby`; screen readers announce generic "region". |
| Async status announcements | ❌ | `#my-search-status`, `#search-status`, `#linkwarden-status`, `#sandbox-status`, `#voice-status`, `#usage-totals` update with **no `aria-live`** — SR users don't hear results. |
| Error messages | ✅ | `role="alert"` on `*-error` (e.g. index.html:368,397). |
| Collapsible nav (future) | — | Must add `aria-expanded`/`aria-controls`; if tabs, `role="tablist"`/`role="tab"`/`aria-selected` + arrow-key handling. |
| `aria-current` | ❌ | Nav active item needs `aria-current="page"`. |
| `prefers-reduced-motion` | ❌ | Not honoured anywhere in `style.css` (only in vendored highlight.min.js). Sidebar uses `transition: transform .2s` (style.css:221); gate nav/pane transitions. |
| Focus-visible | ✅ | Global `:focus-visible` outline (style.css:75-79). |

### C.8 Long labels / wrapping

Long strings that wrap on a 412px phone (verify after nav changes):
- `"Enable email tools (email_folders, email_search, email_read)"` (index.html:603)
- `"Sandbox: unavailable — Docker is not reachable, or images/network are missing (run deploy/sandbox-setup.sh)."` (app.js:1228)
- option `"Auto (Tavily → Brave → Serper → DuckDuckGo)"` (index.html:486)
- `"Monthly cost limit (USD)"` / `"Daily token limit"` (index.html:661-666)
Mitigations already present: `overflow-wrap:anywhere` on account/usage text
(style.css:1041-1042,1146), single-column, `width:100%` inputs. Keep nav labels short.

### C.9 Reduced motion

See C.7. Add:
```css
@media (prefers-reduced-motion: reduce) {
  .settings-nav, .settings-body, .sidebar { transition: none !important; scroll-behavior: auto !important; }
}
```

---

## Part D — Prioritised fix list

Effort: S ≈ <½ day, M ≈ ½–1 day, L ≈ 1–2 days.

### P0 — correctness / trust (do first)

**P0-1 — Merge the two web-search surfaces into one block.**
Reuse `#my-search-section` as the single "Web search" section; move the admin rows
(`#settings-tools-enabled`, `#search-provider`, `#search-brave-key`,
`#search-brave-mode`, `#search-save`, `#search-clear-key`, `#search-error`,
`#search-status`) into it, role-gated. Keep all ids. Effort **M**. Risk: element
relocation inside the "safe to relocate" blocks; `#search-status` must remain the
single status line. Tests: see Part E.

**P0-2 — Make the admin status reflect the caller's own Serper key, or stop
showing a server-only "Serper" line.**
Minimal fix in `integrationSettings` (integrations.go:341): take the user into
account (the handler `writeSettings` has `r`; `handleGetSettings` already resolves
`u` at users.go:237) and compute `serper_configured`/`search_active` via
`searchConfigFor(ctx, u)` — or split the payload so the admin web-search line is
labelled "Server Serper key" and never claims the user's key. Effort **S–M**.
Risk: `/api/settings` is admin-only; passing the admin's own key status is harmless
but touches a shared payload — keep field names stable.

**P0-3 — Remove or wire the dead-end `serper` provider option.**
Either add a server-wide Serper key field, or change the option label to make clear
Serper keys are per-user ("Serper (per-user key)") and drop it from the admin
defaults. Effort **S**. Risk: low.

**P0-4 — Add a focus trap + mark the background inert while settings is open.**
Set `aria-hidden`/`inert` on `#app` (or `.app`) when the settings route is active,
and trap `Tab` within `.settings-page`; release on `hideSettingsPage`
(app.js:836-840). Effort **S–M**. Risk: `inert` support (fine on modern
Safari/Chrome/FF); ensure the gear/sidebar interactions still work after close.

### P1 — UX / IA

**P1-1 — Category nav (rail on desktop/tablet, drill-down on mobile) + category
list + breadcrumb title.** Use the JS registry approach (B.3). Effort **L**. Risk:
must not disturb `hidden` semantics; keep `#settings-back`, Esc, hash router.
Tests: update Part E.

**P1-2 — Sticky settings search.** Effort **M**. Risk: keep it additive/progressive.

**P1-3 — Fix undersized touch targets:** `.user-action` 32→44 (style.css:1114),
`.group-member-remove` 28→44 (style.css:1234), `.segmented span` 40→44
(style.css:934). Effort **S**. Risk: layout reflow in Users/Groups/Appearance;
re-shoot screenshots.

**P1-4 — Accessibility pass:** `aria-labelledby` on sections, `<nav aria-label>`,
`aria-current="page"`, promote pseudo-headings to `h3`
(index.html:481,514,541), add `aria-live="polite"` to the status paragraphs
(`#my-search-status`, `#search-status`, `#linkwarden-status`, `#sandbox-status`,
`#voice-status`, `#ocr-status`, `#imagegen-status`, `#calendar-status`,
`#email-status`, `#usage-totals`), add `prefers-reduced-motion` block. Effort **M**.
Risk: `role="status"`/`aria-live` on frequently-updated text can be chatty — use
polite and update only on load/save.

**P1-5 — Safe-area left/right insets + keyboard-safe inner scroll.** Add
`env(safe-area-inset-left/right)` to `.settings-header`/`.settings-body`; add a
`focusin` handler that scrolls the focused field into view inside `.settings-body`
on mobile. Effort **S–M**. Risk: iOS specifics; verify on device.

### P2 — polish

- P2-1 Consolidate the per-user status sentences (they are long compound strings,
  app.js:1173-1174, 1164-1166) into a short status + details `<dl>`.
- P2-2 Add `h3`/sub-group headings and visual grouping (cards) per category.
- P2-3 Add a "1 of N" / category progress or scroll-spy for the long desktop pane.
- P2-4 Fix the stale admin-Integrations copy that still mentions Linkwarden
  (comment `index.html:468`; subtitle `index.html:471-472`).

### Suggested order

`P0-2 → P0-3 → P0-1 → P0-4 → P1-3 → P1-1 → P1-4 → P1-5 → P1-2 → P2-*`

---

## Part E — Tests affected (`scripts/ui-test.sh`)

The battery drives the settings page and asserts on `element.hidden` /
`!!getElementById`. **Preserve `hidden` semantics** (B.3) so role-based assertions
keep passing; navigation must use a separate class so non-active categories are
`display:none` but `hidden=false`.

Assertions that need review/update:

| Line(s) | Assertion | Impact |
|---|---|---|
| 110 | settings page opens (`!hidden && hash==='#/settings'`) | ✅ unchanged |
| 111 | settings fits viewport | May need re-check after nav adds a rail (mobile still full-screen) |
| 112 | no horizontal overflow | Re-verify with nav rail at 412px |
| 113-117 | account/key/groups/about present | ✅ element ids preserved |
| 116 | non-admin `#users-section.hidden` true | ✅ if role still sets `hidden` |
| 118 | `#my-search-section` present | ✅ but block is now merged → rename expectation to single "Web search" section |
| 119-120 | calendar/email present | ✅ |
| 125 | `scrollintoview "#groups-section"` | If grouped by category, `#groups-section` lives in "AI & access"; must click that nav item (or keeper approach) first |
| 129-134 | back button + `history.back()` close settings | ✅ keep; add a case for category sub-route back |
| 163 | settings opens (admin) | ✅ |
| 164 | `!#users-section.hidden` | ⚠️ **breaks** if nav hides non-active categories via `hidden`; use class-based hiding (B.3) to keep passing, or click the Users nav item first |
| 165 | share-server-key toggle present | ✅ |
| 166 | `!#integrations-section.hidden` | ⚠️ same as 164 |
| 167 | `!#web-search-block.hidden` | ⚠️ block moves/merges (P0-1) → update selector/expectation |
| 168-171 | tools/ocr/usage/voice visible | ⚠️ same category-visibility caveat as 164 |
| 172 | `scrollintoview "#users-section"` | needs Users category active |

Additions to add: nav rail present (`#settings-nav`), breadcrumb title updates,
search filters sections, focus trap (Tab stays inside `#settings-page`), Escape from
a category sub-route returns to the category list, and (P0-2) admin status shows the
per-user Serper key.

`scripts/ui-test-run.sh` (run.sh 44-52) builds/starts an isolated instance and calls
`ui-test.sh` with `ADMIN_USER`/`ADMIN_PASS`. No changes required to `run.sh` unless
we add a *second* admin-with-Serper scenario — recommended: seed a per-user Serper
key for the admin (via CLI or a fixture) so P0-2 is regression-covered.

---

## Appendix — quick file/line index

- Sections markup: `index.html:273`(account) `283`(appearance) `312`(key)
  `333`(groups) `355`(my-search) `379`(linkwarden) `407`(calendar) `469`(integrations)
  `480`(web-search-block) `513`(maps-block) `540`(imagegen-block) `566`(email)
  `611`(tools) `626`(ocr) `641`(usage) `679`(voice) `718`(users) `751`(about).
- Router: `app.js:776-805, 4520-4523`. Visibility: `app.js:807-834, 863-872, 1034-1060`.
- Per-user loaders: `app.js:1156-1179` (Serper), `1691-1704` (Linkwarden).
- Status computations: `app.js:1070-1083` (admin), `1156-1174` (per-user).
- Server: `integrations.go:43` `67` `341` `377`; `linkwarden.go:26`; `users.go:236` `248` `441`.
- CSS: `style.css:987-1023` (page/body), `1114`, `1234`, `934`, `1295-1321` (desktop).
- Cache version: `static/sw.js:5` (`v10`).

## Related handoff — one-line answer to the owner

> Your Serper key **is** stored and shown — in the per-user **Web search** block it
> reads "your Serper key set (…xxxx)". The confusing part is that admins also see a
> second, server-only **Integrations → Web search** block that ignores your personal
> key and can say "Serper: not configured" while the other block says "configured".
> We'll merge them into one block and make the status use your own key.
