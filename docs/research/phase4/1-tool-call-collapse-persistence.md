# Phase 4.1 — Collapsible tool calls + full tool I/O persistence

Repo: `~/dev/myogchat` (single Go binary, stdlib `net/http`,
`modernc.org/sqlite`, embedded vanilla-JS SPA, no build step).
Research only — no code was modified. All line numbers are current as of this
investigation.

Owner goals:

1. Tool calls render **collapsible**, exactly like the thinking block
   (`<details>/<summary>`, default collapsed, count + status line).
2. Persist **all** tool calls with **full inputs (arguments)** and **full outputs
   (results)**, retained ~30 days alongside chats.

---

## 0. TL;DR recommendation

- **Data is partially available today and is discarded at the server boundary.**
  The upstream agent already holds the tool **arguments** (`ToolCall.Arguments`,
  `internal/upstream/upstream.go:73-78`) and the **result/error** text
  (`runTool`, `internal/upstream/agent.go:349-358`), but the emitter throws them
  away: it only sends `{name,status,summary}` on `EventTool`
  (`agent.go:350,353,356`). No per-tool duration is measured anywhere.
- **Recommended storage: extend the existing `messages.tools` JSON array** with
  `id`, `args`, `output`, `error`, `duration_ms`, `truncated` — **not** a child
  table. It matches the established `attachments`/`images` JSON-column pattern,
  needs no migration, and keeps the reload path unchanged. Add hard server-side
  caps (proposed: 16 KB args, 64 KB output, 512 KB per message) with a
  `truncated` flag. Pick the child table only if you later want lazy-loading or
  SQL-level queries over tool I/O.
- **Caps must be enforced at the server persistence boundary**
  (`streamReply`, `internal/server/chat.go:274-284`) so the SSE wire and the DB
  copy are byte-identical. What the model receives (`agent.go`) stays uncapped.
- **Retention needs no new work**: the janitor deletes conversations
  (`internal/store/store.go:766`, `main.go:809-852`) and `messages` cascade via
  `conversation_id TEXT ... ON DELETE CASCADE` (`store.go:62`); a JSON column
  dies with the row.
- **UI**: wrap the per-message tool list in one `<details class="msg-tools">`
  (reuse `.msg-tools`/`.msg-tool` class names, follow `buildThinking`,
  `static/app.js:2894-2905`), summary `"🔧 3 tools · 2 done, 1 error"`, each tool
  its own inner `<details>` with `Input`/`Output` `<pre>` (textContent, never
  `innerHTML`) and a Copy button.
- **Effort: Medium, ~2–3 days.** Highest risk is payload/DB growth and secrets
  appearing in tool args/outputs.

---

## 1. Ground truth: what exists today

### 1.1 SSE event level

Wire format is a normalized SSE stream (`API_CONTRACT.md:104-115`). The `tool`
event **is not documented in that table** — it exists in code but the contract
table lists only `meta/reasoning/text/usage/done/error`. Doc drift to fix.

Emitter: `internal/server/chat.go:48-53`:

```go
type toolEvent struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // running | done | error
	Summary string `json:"summary,omitempty"`
}
```

Emitted in `streamReply` (`chat.go:274-284`):

```go
case upstream.EventTool:
	if ev.Tool != nil {
		tools = mergeToolEvent(tools, ev.Tool)
		writeSSE(w, flusher, "tool", toolEvent{
			Name:    ev.Tool.Name,
			Status:  ev.Tool.Status,
			Summary: ev.Tool.Summary,
		})
	}
```

So the SSE frame carries **name + status + 80-char summary only**. No arguments,
no output, no error text (beyond summary), no duration.

### 1.2 Upstream agent loop — where the real data lives

`runTool` (`internal/upstream/agent.go:349-358`):

```go
func runTool(ctx context.Context, exec ExecuteTool, send func(Event) bool, call ToolCall) string {
	send(Event{Kind: EventTool, Tool: &ToolActivity{Name: call.Name, Status: ToolRunning}})
	result, summary, err := exec(ctx, call)
	if err != nil {
		send(Event{Kind: EventTool, Tool: &ToolActivity{Name: call.Name, Status: ToolError, Summary: firstN(err.Error(), 200)}})
		return "Tool error: " + err.Error()
	}
	send(Event{Kind: EventTool, Tool: &ToolActivity{Name: call.Name, Status: ToolDone, Summary: summary}})
	return result
}
```

Availability at this point:

| datum | available? | source |
|---|---|---|
| tool name | yes | `call.Name` |
| tool-call id | yes | `call.ID` (chat) / `call.CallID` (responses) — `upstream.go:70-78` |
| **arguments** | yes | `call.Arguments` (raw JSON string) |
| **result / output** | yes | local `result` |
| **error text** | yes | local `err` |
| duration | no | not measured; only whole-reply `duration_ms` (`chat.go:249-255`) |
| summary | yes | returned by `exec` (`toolExecutor` → `toolSummary`, `internal/server/integrations.go:316-338`) |

Both dialects go through `runTool`: chat at `agent.go:150-157`, responses at
`agent.go:328-332`. `agentResponses` supplies `call.Arguments` too
(`agent.go:405-412`, `functionCallItem`). `ExecuteTool` is
`func(ctx, ToolCall) (result, summary string, err error)` (`upstream.go:80-84`),
so the result string is fully in hand.

The tool registry itself is not the bottleneck:
`internal/tools/tools.go:19-29` — every tool's `Run(ctx, args json.RawMessage)`
receives the raw args and returns the full textual result. Nothing needs to
change inside individual tool implementations to expose args/output; the values
already flow through `exec`.

**Conclusion:** to persist full I/O you do **not** need to touch individual
tools. You need to (a) add fields to `upstream.ToolActivity`, (b) populate them
in `runTool`, (c) relay them in the server's `toolEvent`, and (d) store them.

### 1.3 Server collection / merge

`mergeToolEvent` (`chat.go:451-462`) folds `running` → terminal by **name**
(last running entry with the same name). Tools run sequentially
(`agent.go:150-157`), so this is safe, but matching by the stable `call.ID`
would be exact once ids are persisted (nice-to-have, backward compatible).

### 1.4 Persistence

- Type `store.ToolActivity` (`internal/store/store.go:200-207`):
  `{Name, Status, Summary}`.
- `messages.tools TEXT` migration (`store.go:352`), read in `scanMessage`
  (`store.go:803-806`), written in `InsertMessage` via `marshalJSON`
  (`store.go:819-822,836-847`).
- Assistant row assembled/inserted at `chat.go:298-316`.
- **No** per-tool duration, args, output, or error fields exist today.

### 1.5 SPA rendering

- `toolLine` / `buildToolsBlock` (`static/app.js:3218-3229`) render a flat,
  always-visible list of `"<icon> <name> · <status> · <summary>"`.
- Streaming upsert `updateToolActivity` (`app.js:3232-3251`) + `renderStreamTools`
  (`app.js:3253-3263`) rebuild the same flat list.
- Reload path `buildAssistantMessage` → `buildToolsBlock` (`app.js:2962-2964`).
- `finalizeStream` pushes optimistic `{name,status,summary}` copies
  (`app.js:3382-3385`) then `refreshCurrentConversation()` (`app.js:2832-2850`)
  overwrites them from `GET /api/conversations/{id}`.
- Thinking block pattern to copy (`app.js:2894-2905`):

```js
function buildThinking(text, opts) {
  const details = el("details", "thinking");
  const summary = document.createElement("summary");
  summary.textContent = opts.summary || thinkingSummary(opts);
  const body = el("div", "thinking-body");
  body.textContent = text;
  details.append(summary, body);
  return details;
}
```

- CSS: `.msg-tools`/`.msg-tool` (`static/style.css:542-552`); `.thinking`
  details with `min-height: 44px` summary (`style.css:554-576`).

### 1.6 API surface

`handleGetConversation` (`internal/server/server.go:298-317`) marshals
`func (s *Server) handleGetConversation` → `[]store.Message` directly, so adding
JSON fields to `store.ToolActivity` surfaces them with **no handler change**.

### 1.7 Retention

`janitor` (`main.go:809-852`) deletes conversations older than
`MYOGCHAT_RETENTION_DAYS` (default 30, max 60 — `main.go:373,449-456`) via
`DeleteOlderThan` (`store.go:766-772`); `messages` cascade
(`store.go:62` `ON DELETE CASCADE`). Messages are not purged independently.
**A child table keyed on `message_id ... ON DELETE CASCADE` would also be purged
automatically, but the JSON column needs zero new janitor code.**

---

## 2. Storage design

### 2.1 Option A — extend `messages.tools` JSON (RECOMMENDED)

New shape per element:

```json
{
  "id": "call_abc",
  "name": "web_search",
  "status": "done",
  "summary": "3 results",
  "args": "{\"query\":\"go sqlite wal\"}",
  "output": "…full textual result (capped)…",
  "error": "",              // terminal error text when status=error
  "duration_ms": 412,
  "truncated": false
}
```

New struct (`store.go`, beside the existing type at `:203-207`):

```go
type ToolActivity struct {
	ID         string `json:"id,omitempty"`
	Name       string `json:"name"`
	Status     string `json:"status"` // running | done | error
	Summary    string `json:"summary,omitempty"`
	Args       string `json:"args,omitempty"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}
```

Why: matches `attachments`/`images` JSON columns, **no schema migration** (the
TEXT column already exists), `ListMessages`/`handleGetConversation` need no
changes, and the janitor cascade already covers it. Row/disk limits are not a
concern for SQLite (TEXT max 1e9 bytes) but **HTTP payload size and memory are**,
so caps matter (see §3).

### 2.2 Option B — child table `message_tools`

```sql
CREATE TABLE IF NOT EXISTS message_tools (
  id          TEXT PRIMARY KEY,
  message_id  TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  call_id     TEXT,
  name        TEXT NOT NULL,
  status      TEXT NOT NULL,
  summary     TEXT,
  args        TEXT,
  output      TEXT,
  error       TEXT,
  duration_ms INTEGER,
  truncated   INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_message_tools_message ON message_tools(message_id, seq);
```

Pros: bounded single-row size; args/output loadable lazily on expand via a new
`GET /api/messages/{id}/tools`; queryable; outputs could be stored separately
from the message row. Cons: new table + migration + `ListMessages` must join and
group (or a second query), new store functions, and `handleGetConversation`
payload changes. More code for a display-only feature; the codebase deliberately
avoids this.

**Recommendation: Option A.** Revisit Option B only if you want lazy-loading of
large outputs or SQL analytics over tool I/O. Given a personal PWA and a bounded
agent loop (`maxToolIterations = 4`, `upstream.go:47`), Option A is right-sized.

---

## 3. Caps / truncation

Bounded outputs already exist inside tools — `fetch_url` caps readable text at
`fetchTextLimit = 12000` chars (`internal/tools/fetchurl.go:23-25`), the sandbox
caps combined stdout+stderr at `DefaultMaxOutput = 100_000` chars
(`internal/sandbox/sandbox.go:48,421-435`). But writes are unbounded, and the
persisted copy should still be capped.

Proposed caps (server-side, at the persistence boundary):

- args: **16 KB**
- output: **64 KB**
- per message (sum of args+output across all tools): **512 KB** safety net
- set `truncated: true` and optionally keep the original byte count.

Implement one helper in `internal/server/chat.go` (reuse existing
`truncateRunes`, `chat.go:438-444`, or a byte-wise variant to avoid splitting
UTF-8) and apply it in `streamReply` **before** both `writeSSE` and
`mergeToolEvent`, so the streamed frame and the persisted row are identical
(prevents a reload "surprise" where the SSE output was full but the DB copy
capped, or vice-versa).

Important: the copy fed back to the model in `agent.go` (`result`) must **not**
be truncated — only the display/persistence copy.

### 3.1 Data-flow changes required

1. `internal/upstream/upstream.go`
   - Add fields to `ToolActivity` (`:63-68`): `Args`, `Output`, `Error`,
     `DurationMs`, `ID`.
2. `internal/upstream/agent.go`
   - `runTool` (`:349-358`): time `exec`, put args on the **running** event and
     output/error/duration on the **terminal** event:
     ```go
     start := time.Now()
     send(Event{Kind: EventTool, Tool: &ToolActivity{ID: call.ID, Name: call.Name, Status: ToolRunning, Args: call.Arguments}})
     result, summary, err := exec(ctx, call)
     dur := time.Since(start).Milliseconds()
     // error → Tool{..., Status: ToolError, Error: err.Error(), DurationMs: dur}
     // ok    → Tool{..., Status: ToolDone, Output: result, DurationMs: dur}
     ```
   - `agentChat`/`agentResponses` call sites unchanged (they already pass `call`).
3. `internal/server/chat.go`
   - `toolEvent` (`:48-53`): add the same JSON fields.
   - `streamReply` `EventTool` case (`:274-284`): cap args/output, populate the
     frame, and extend `mergeToolEvent`.
   - `mergeToolEvent` (`:451-462`): carry args from the running entry; merge
     output/error/duration/truncated from the terminal entry; match on `ID`
     when present (fall back to name for legacy rows/events).
4. `internal/server/integrations.go` — no change needed
   (`toolExecutor` already returns the full result, `:316-329`).

### 3.2 Duration note

Only whole-reply `duration_ms` is measured today (`chat.go:249-255`). Per-tool
`duration_ms` comes from wrapping `exec` in `runTool`; no schema/interface change
is required beyond the new `ToolActivity.DurationMs` field.

---

## 4. Retention / janitor

No change required for Option A. `janitor` purges conversations whose
`updated_at` is older than the window; `messages` cascade; the `tools` JSON goes
with the row (`main.go:809-852`, `store.go:62,766-772`). Regenerate
(`chat.go:148-155` → `DeleteAssistantAfterLastUser`, `store.go:869-882`) deletes
the trailing assistant message, so its tool I/O is replaced with the new turn —
also automatic. With Option B you would add the `ON DELETE CASCADE` FK (already
shown in §2.2) and still touch no janitor code.

One caveat to document: active conversations keep all messages regardless of
age, so a long-lived chat retains its tool I/O even past 30 days — consistent
with how messages already behave.

---

## 5. API contract impact

- `internal/server/server.go:298-317` already dumps `[]store.Message`; the new
  fields appear automatically. **No handler edit.**
- `API_CONTRACT.md:275-288` (§8.4) must be updated: the `tools` entry becomes
  `[{id?,name,status,summary?,args?,output?,error?,duration_ms?,truncated?}]`,
  with caps and truncation semantics documented.
- `API_CONTRACT.md:104-115` (SSE table) must **add the missing `tool` row** and
  document the expanded payload:
  `{"name","status","id"?,"args"?,"output"?,"error"?,"duration_ms"?,"truncated"?}`
  (`args` on `running`, `output`/`error`/`duration_ms` on terminal).
- `API_CONTRACT.md:119-126` (upstream `Event` doc) should mention `EventTool`
  and `ToolActivity` fields.
- No new endpoints. No request-body changes.
- Old rows (pre-4.1) lack the new fields → decode to zero values; the UI must
  treat empty `args`/`output` as "not recorded" and hide those sections.

---

## 6. UI/UX (mobile-first, no build step)

Keep existing class names `.msg-tools` / `.msg-tool`; there are no tool element
ids today. Do not rename to avoid CSS/test churn; optionally add
`.msg-tools-block` / `.tool-io` classes.

Structure (per assistant message):

```
<details class="msg-tools">                ← collapsed by default
  <summary>🔧 3 tools · 2 done, 1 error</summary>
  <details class="msg-tool">               ← per tool, collapsed
    <summary>🔧 web_search · done · 412ms · 3 results</summary>
    <div class="tool-detail">
      <div class="tool-label">Input</div>
      <pre class="tool-io">{"query":"go sqlite wal"}</pre>
      <div class="tool-label">Output</div>
      <pre class="tool-io">…</pre>          ← textContent only
      <button class="msg-action">Copy</button>
    </div>
  </details>
  …
</details>
```

Implementation notes:

- Follow `buildThinking` (`app.js:2894-2905`) — create `<details>`/`<summary>`
  via `el()`/`createElement`, set text with `textContent` (never `innerHTML`) to
  avoid XSS from tool output. **Do not** route tool I/O through `renderMarkdown`.
- Replace `buildToolsBlock` (`app.js:3225-3229`) with a builder that returns the
  outer `<details>`; reuse `toolIcon` (`:3212-3216`). Summary line:
  `"🔧 {n} tool(s) · {done} done, {errors} error"` (omit zero clauses).
- Per-tool summary: `"{icon} {name} · {status} · {duration}ms · {summary}"`.
- `renderStreamTools` (`app.js:3253-3263`) must rebuild the same structure and
  update the outer summary as tools complete; keep it collapsed while streaming
  (the running/status text in the summary is the progress signal). Reuse the
  existing `scrollToBottom()`.
- `updateToolActivity` (`app.js:3232-3251`): retain the new fields on the
  optimistic entry so a post-stream `refreshCurrentConversation()` produces no
  flicker; also add the `id` match when available.
- CSS (add near `style.css:542-576`): `.msg-tools > summary` and
  `.msg-tool > summary` get `min-height: 44px` (touch target, matching
  `.thinking summary`); `.tool-io { white-space: pre-wrap; overflow-wrap: anywhere;
  max-height: 40vh; overflow: auto; font-family: monospace; font-size: .78rem; }`
  and add a `.tool-label` style.
- Copy: reuse `copyText()` (used by `copyMessage`, `app.js:2918-2925`) and the
  `actionButton` helper (`app.js:2907-2916`). Copy the output (fallback to the
  full input+output block).
- The thinking-display setting (`settings.thinking`) is thinking-specific; keep
  tool collapse always default-collapsed (owner requirement). No new setting
  needed, though a `settings.tools` pref could mirror it later.
- Bump `static/sw.js` `CACHE` (`sw.js:5`, currently `myogchat-shell-v10`).
- Optional perf: for very large outputs, render `<pre>` content on first
  `toggle` instead of up-front. Likely unnecessary at 64 KB cap.

---

## 7. Tests

- `internal/store/store_test.go:492-533` (`TestMessageToolsRoundTrip`): extend
  the seeded `ToolActivity` with `Args`/`Output`/`Error`/`DurationMs`/`Truncated`
  and assert deep-round-trip; keep the nil-for-empty assertion. No migration test
  change needed for Option A (column already exists; migration assert at `:93`).
- `internal/server/chat_test.go:15-44` (`TestMergeToolEvent`): extend to assert
  args carry from `running`, output/error/duration merge from terminal, and
  id-based matching. Add a unit test for the new truncation helper (cap + rune
  safety + `truncated` flag). `TestHandleGetConversationIncludesTools`
  (`:49-90`) already proves the JSON surfacing path — extend its assertions.
- `internal/upstream/agent_test.go:168-170`: assert `EventTool` carries args on
  `running` and output/duration on `done`/`error`.
- Frontend: `node --check static/app.js`; there is a `scripts/ui-test-run.sh`
  battery (per `docs/research/2.10-2.11.md:320-321`) — add a tool-collapse check
  if the harness supports it.

---

## 8. Effort estimate

**Medium — ~2–3 days** including tests and docs.

| area | effort |
|---|---|
| `upstream` agent + types | ~0.25 d |
| server SSE + cap + merge | ~0.5 d |
| store struct (JSON field extend) | ~0.1 d (Option B: +0.5 d) |
| SPA collapsible UI + CSS + copy | ~1 d |
| tests | ~0.5 d |
| docs (`API_CONTRACT.md`, `HANDOFF.md`, `TODO.md`, `sw.js`) | ~0.25 d |

---

## 9. Risks

1. **Payload / DB growth.** Tool-heavy turns can add tens–hundreds of KB per
   message. Mitigate with the caps in §3 and a per-message ceiling; document that
   the reload endpoint now ships tool I/O.
2. **Secrets in tool args/outputs.** Args include model-written code
   (`run_bash`/`run_python`); outputs include fetched pages, email bodies
   (`email_read`), calendar events, and file contents — any of which may contain
   tokens or keys. This is plaintext storage in the user's own SQLite DB,
   consistent with how message content is already stored, scoped per user. Do
   **not** log args/outputs at info level (`toolExecutor` currently logs only the
   tool name + error, `integrations.go:324`). Redaction is lossy and would defeat
   the feature's purpose; if desired, do opt-in pattern redaction (Bearer keys,
   `api_key=`, etc.) at the cap step. **Flag as a decision (see open questions).**
3. **XSS.** Tool output is untrusted; render with `textContent`/`<pre>` only,
   never `innerHTML`/markdown. This is the single most important UI invariant.
4. **Truncation correctness.** Truncate on rune/UTF-8 boundaries (existing
   `truncateRunes`; a byte cap must not split a multi-byte rune).
5. **SSE vs reload mismatch.** Cap at the server boundary so both copies match,
   and keep the model-facing result uncapped in `agent.go`.
6. **Backward compatibility.** Pre-4.1 rows have no new fields; the UI must
   gracefully hide missing args/output. Merge-by-name must remain as a fallback
   when `id` is absent.
7. **Regenerate.** The trailing assistant message (and its tool I/O) is deleted
   and replaced — expected, no orphan data.
8. **Option B only:** join/assembly complexity and a second query in the hot
   reload path.

---

## 10. Open questions

1. **Exact caps** — accept 16 KB args / 64 KB output / 512 KB per message, or
   raise output to 128–256 KB?
2. **Redaction** — store tool args/outputs verbatim (recommended), or scrub
   detected secrets at the persistence boundary? A per-user "store full tool
   I/O" toggle is possible but conflicts with the "persist all" goal.
3. **Lazy loading** — is inline-in-message acceptable on mobile, or should large
   outputs move behind an on-expand endpoint (i.e. Option B)?
4. **Per-tool duration display** — show `412ms` always, or only above a
   threshold?
5. **Tool-id matching** — adopt `call.ID` in `mergeToolEvent`/`updateToolActivity`
   (exact) while keeping name-based fallback for legacy data?
6. **Auto-expand on error** — should a tool that errored be auto-expanded so the
   user notices, or stay collapsed like everything else?
7. **Regenerate/redo UI** — out of scope, but storing full I/O makes a future
   "re-run this tool" feature possible; confirm it is not implied by this task.
