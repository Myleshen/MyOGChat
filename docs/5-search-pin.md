# 5 — Search across chats & pinned chats

Two related sidebar conveniences:

- **Feature A — Search across chats**: find a conversation by its title or by
  anything said inside it, without leaving the sidebar.
- **Feature B — Pinned chats**: keep important conversations at the top of the
  list.

Status: implemented on branch `feat/p5-search-pin`. Contract: `API_CONTRACT.md`
§18 (and the `PATCH /api/conversations/{id}` note in §3).

## Feature A — Search across chats

### Store

`internal/store/store.go`:

```go
type ConversationHit struct {
    ConversationID string `json:"id"`
    Title          string `json:"title"`
    Snippet        string `json:"snippet"`
    UpdatedAt      int64  `json:"updated_at"`
}

func (s *Store) SearchConversations(ctx context.Context, userID, query string, limit int) ([]ConversationHit, error)
```

- Matches `conversations.title` **and** `messages.content`, case-insensitively
  (`lower(...) LIKE '%<query>%'`).
- Scoped to the caller: joins/guards on `conversations.user_id`, so another
  user's chats can never appear.
- One row per conversation; the representative matching message is the earliest
  matching message of that conversation.
- Orders by `updated_at DESC` and caps at `limit`.
- `snippet` is built by `makeSnippet` — a ~120-rune window centred on the first
  match, newlines collapsed, `…` when truncated. A **title-only** match has an
  empty snippet (no message matched).
- An empty/whitespace query returns an empty slice without touching the DB.

### Server

`internal/server/server.go`:

```
GET /api/search?q=<query>   →   {"results":[{id,title,snippet,updated_at}]}
```

- Authenticated (session required).
- Empty or whitespace `q`, or `q` longer than **200 runes**, returns
  `{"results":[]}` (HTTP 200) — no error.
- The result cap is 50.
- Route registered next to the conversation routes.

### UI (vanilla JS, `static/`)

- `#search-input` sits at the top of the sidebar (`#search-box`), above the
  Projects/Chats sections. It is a native `type="search"` field plus a clear
  button, both ≥44px targets.
- Input is debounced **250 ms**; a sequence counter drops stale responses.
- While the query is non-empty a `searching` class on `.convo-nav` hides the
  Projects/Chats sections and shows `#search-results` instead, rendering each
  hit as title + snippet. An empty state (`#search-empty`) covers no matches.
- Clicking a result opens that conversation and clears the search; `Escape` in
  the box also clears it.
- The whole feature is wrapped in `BEGIN chat search` / `END chat search`
  markers in `index.html`, `app.js`, and `style.css` for clean merges, and uses
  `search-*` prefixed classes so it cannot collide with other sidebar work.

## Feature B — Pinned chats

### Store

- Additive migration: `conversations.pinned INTEGER NOT NULL DEFAULT 0` (in the
  `CREATE TABLE` and in `migrate()`).
- `Conversation` gains `Pinned bool` (`json:"pinned"`); scan + insert
  round-trip it.
- `ListConversations` orders `pinned DESC, updated_at DESC`.
- ```go
  func (s *Store) SetConversationPinned(ctx context.Context, id, userID string, pinned bool) error
  ```
  Owner-guarded; `0` rows affected returns `store.ErrNotFound`.

### Server

`PATCH /api/conversations/{id}` now also accepts `{"pinned": bool}` alongside the
existing `title` / `model` / `effort` / `project_id` fields. The flag is stored
through `SetConversationPinned`; a missing or other-user conversation returns
`404`. The response is the updated conversation, including `pinned`.

### UI (vanilla JS, `static/`)

- Each conversation row gets a pin/unpin button (`.convo-pin`, 44px target):
  hidden until hover/focus on pointer devices, always visible on touch
  (`@media (hover: none)`), and always visible (accent coloured) when pinned.
- A subtle indicator: pinned rows use a heavier title weight and the accented
  pin glyph.
- Toggling calls the PATCH endpoint, updates the row, and re-sorts the list
  pinned-first locally to match the server order.
- Wrapped in `BEGIN pinned chats` / `END pinned chats` markers.

## Tests

- `internal/store/store_test.go`
  - `TestSearchConversations` — content + title matching, case-insensitivity,
    user scoping, snippets, no-match/blank query.
  - `TestSearchConversationsOrderAndLimit` — ordering and the result cap.
  - `TestPinnedRoundTripAndScoping` — default, owner-guarded toggle,
    pinned-first ordering, insert round-trip, cross-user rejection.
  - `TestMigratesLegacySchema` now asserts `conversations.pinned` is added.
- `internal/server/conversation_search_test.go`
  - `GET /api/search` auth, empty query, oversize query, scoping + response
    shape, title-only snippet.
  - `PATCH .../pinned` round-trip + cross-user `404`.
  - New conversations default to unpinned.
