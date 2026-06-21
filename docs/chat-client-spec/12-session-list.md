# 12 · Session List / Sidebar Sync

The session list (inbox / sidebar) **SHOULD** be a **live** surface that stays in sync with
the host as sessions are created, deleted, change status, get a title, are marked read,
archived, etc. — **without the user refreshing.** It is the same SSE stream as the chat view,
consumed differently. Liveness is not free against a remote host, though — see
[Cost tradeoff](#cost-tradeoff-live-list-vs-sprite-uptime) — which is why this is a SHOULD,
not a MUST.

Golden reference: the TUI inbox (`internal/tui/inbox_sse.go`), which is explicit about the
right and wrong events to drive a list from.

## Cost tradeoff: live list vs. sprite uptime

The session list stays live by holding an open `GET /events` stream. Against a **remote host**
(a cloud sprite) an open stream keeps the sprite **awake — and billing — for as long as the
client holds it**: a client parked on the inbox doing nothing still pins it up. The local TUI
has no such cost — its host is the laptop, so holding `/events` open is free.

That asymmetry is why [LIST-001] is a **SHOULD**, not a **MUST**: a client talking to a remote
host MAY trade liveness for cost — suspend or close the stream while backgrounded or while
idling on the list, and reconcile on next foreground ([LIST-006],
[STATE-BACKGROUND-001](06-state-model.md)) — accepting that the list is only as fresh as its
last refetch while the stream is down. This resolves the tension with
[NFR-AVAIL-002](09-non-functional.md) ("a client SHOULD NOT perform routine background
activity that wakes the sprite").

The tradeoff would dissolve with a **gateway-side session mirror or push** that serves the
list without holding a stream open to the sprite — a forward-looking direction, not built
today.

## Subscribe once, globally

- **[LIST-001] (SHOULD)** When the list is live it subscribes to the **global** `GET /events`
  stream and mutates rows in place from events; it MUST NOT poll. A client that shows both the
  list and an open session SHOULD share **one** global stream between them (filter per
  consumer), per [EVT-001](04-event-protocol.md)/[EVT-005](04-event-protocol.md). Against a
  **remote** host a client MAY keep the list non-live while idle or backgrounded to avoid
  holding the sprite awake ([Cost tradeoff](#cost-tradeoff-live-list-vs-sprite-uptime)),
  reconciling on next foreground via [LIST-006]. **Golden:** `internal/tui/inbox_sse.go:12`.

## `meta` is the row-sync signal

- **[LIST-002] (MUST)** The `meta` event (`EventMetaChange`) is the canonical row update: the
  host emits one — carrying the **full post-mutation `SessionInfo`** — for **every** persisted
  session mutation (status flip, title, mark-read, draft, visibility, follow-up). On `meta`, a
  client MUST **replace the matching row wholesale** ([INV-META-REPLACE-001](08-invariants.md)).
  **Why:** the full row carries the freshly-bumped `updated_at`, which the list's sort and
  unread state depend on. **Golden:** `internal/tui/inbox_sse.go:149`, `internal/agent/agent.go:205`.

- **[LIST-003] (MUST)** A client MUST drive list rows from `meta` (+ create/delete below),
  **not** from the field-level `status`/`title` events. Those field-level events are emitted
  *first*, as a transition signal for the **open session view** ([STATE-STATUS-001](06-state-model.md)),
  and are followed by the consolidated `meta`. Patching a list row from `status`/`title`
  updates one field but **misses the bumped `updated_at`**, leaving the sort stale (the
  observed symptom: a session that just got activity does not hoist to the top).
  **Why:** this is the exact bug `inbox_sse.go` documents avoiding.
  **Golden:** `internal/tui/inbox_sse.go:23`–`:37`, `:156`. **Conformance:** `CONF-SIDEBAR-SYNC`.

  > **Divergence (RN):** the React Native client has no `meta` handler; it patches the list
  > from `status`/`title` and relies on a list-query invalidation (refetch) to re-sort, and
  > `meta`-only changes (visibility/draft/follow-up from another client) don't push at all
  > (they reconcile on the next refetch). This works but is not the push-based golden
  > behavior — tracked in [react-native-ts.md](implementations/react-native-ts.md).
  > **Golden contrast:** `clank-mobile/src/hooks/dispatch.ts` (no `meta` case).

## Create & delete

- **[LIST-004] (MUST)** On `session.create`, add the row; if the event carries the full
  `SessionInfo` payload, insert from it (dedup by id against a racing list fetch), else
  refetch the list. On `session.delete`, remove the row keyed by `session_id` (a refetch as a
  safety net is OPTIONAL — sibling rows, e.g. cascade-deleted worktree owners, may also have
  changed). **Why:** instant insert avoids a round-trip; delete must not leave a ghost row.
  **Golden:** `internal/tui/inbox_sse.go:118` (insert-from-payload + dedup), `:136` (drop inline).

  > Note: over SSE the `session.create` payload may decode generically rather than as a typed
  > `SessionInfo` (it is outside the typed-`data` switch, [EVT envelope](04-event-protocol.md)).
  > A client that can't read the payload MUST refetch — never drop the create.

## Ordering, unread, and what doesn't bump

- **[LIST-005] (MUST)** Sort by `updated_at` (descending); a row is **unread** when
  `last_read_at` is zero or older than `updated_at`. **User-driven** metadata mutations
  (mark-read, draft, visibility, follow-up) MUST NOT be expected to bump `updated_at` — only
  agent-driven activity does — so the list does not reorder under the user when they merely
  read or tag a session ([DATA-011](03-data-model.md)). The `meta` event still fires for these
  so the row reflects the change in place. **Golden:** `internal/agent/agent.go:505` (`Unread`),
  `internal/host/sessions_meta.go`.

## Resilience

- **[LIST-006] (MUST)** On stream close, the list MUST reconnect with capped backoff and
  **resync** by refetching the list, so rows missed during the gap are recovered (no replay,
  [EVT-010](04-event-protocol.md)). The golden inbox uses 250 ms → 5 s backoff + a full reload.
  **Why:** same at-most-once delivery as the chat stream; the list must self-heal.
  **Golden:** `internal/tui/inbox_sse.go:39` (reconnect + resync), `:45` (backoff bounds).
  **Conformance:** `CONF-SIDEBAR-SYNC` (with an injected drop).

## Two consumers, one stream

The chat view and the list consume the **same** global events with different rules:

| Event | Open session view ([06](06-state-model.md)) | Session list (this doc) |
|---|---|---|
| `status` | drives streaming-settle + busy/idle UI | **ignored** (use `meta`) |
| `title` | updates open session title | **ignored** (use `meta`) |
| `meta` | (open view may update its own `SessionInfo`) | **replace the row** |
| `session.create` / `session.delete` | note current session deleted | **add / remove row** |
| `message`/`part`/`permission`/`error`/`revert` | full transcript reducer | ignored |

- **[LIST-007] (SHOULD)** A client SHOULD route each event to the right consumer per this
  table rather than sharing one handler, so the list's `meta`-driven sort and the view's
  `status`-driven streaming don't interfere. **Golden:** `internal/tui/inbox_sse.go` (list)
  vs. `internal/tui/sessionview.go` (view).
