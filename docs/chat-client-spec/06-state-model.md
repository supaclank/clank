# 06 · State Model — the declarative reactive core

This is the spine of the spec. Chat is a **reducer**: a single canonical state, mutated only
by typed inputs (events, operation results, user intents, lifecycle signals), with the UI a
pure **projection** of that state. A client MAY structure its code however it likes, but its
*observable behavior* MUST match the reducer and projections defined here.

The contract a conformance test checks (see [10](10-conformance.md)) is exactly:
**given a sequence of inputs, the canonical state and the view-projections below MUST match.**
That is the formal meaning of "what the user sees corresponds to the spec."

## Canonical state

A per-session state value. (An inbox client also keeps a session-list; that is the same
`SessionInfo[]` mutated by `meta`/`session.*`/list refetch — covered inline.)

```
SessionState {
  connection:        disconnected | connecting | live
  session:           SessionInfo                 // host's metadata, the source of truth
  transcript:        Message[]                   // ordered; each Message has ordered Parts, keyed by id
  pendingPermissions: PermissionData[]           // FIFO queue; front is the active prompt
  replyInFlight:     requestId | null            // single-flight guard for permission replies
  compose:           { text, active, submitting } // the composer
  follow:            bool                         // auto-scroll intent
  aborting:          bool                         // suppresses noise during cancellation
  historyLoaded:     bool                         // a full transcript fetch has completed
}
```

Each `Part` additionally carries a derived `streaming` flag (true while text is arriving).

## Reducer principles

- **[STATE-001] (MUST)** All transcript mutation MUST be **idempotent by ID** (message ID,
  part ID) and **monotonic**: an update may add a message/part, grow text, advance a tool
  status, or fill input/output — it MUST NEVER remove a message/part, shorten text, or
  regress a status. **Why:** stream and refetch race and overlap; monotonic-by-ID is the only
  rule under which every interleaving converges. **Golden:** `clank-mobile/src/lib/mergeMessages.ts`,
  `internal/tui/sessionview.go:1638`. Conformance: [CONF-MONOTONIC](10-conformance.md).
- **[STATE-002] (MUST)** `session` (the host's `SessionInfo`) is authoritative for metadata;
  optimistic local edits MUST converge to it. **Why:** see [ARCH-002](01-architecture.md).

## Transitions — events

### `status` → [STATE-STATUS-001]
- **(MUST)** Set `session.status = new_status`. When `new_status ∉ {busy, starting}`, mark
  every streaming part **settled** (`streaming=false`) so it switches from incremental to
  final rendering. When `new_status == busy`, set `follow = true` (auto-follow a new turn).
  If `aborting`, suppress the intermediate transition and, once `new_status ∉ {busy,starting}`,
  finalize the cancel ([VIEW-CANCELLING-001]) and clear `aborting`.
  **Golden:** `internal/tui/sessionview.go:1425`–`:1462`.

### `message` → [STATE-MSG-001]
- **(MUST)** Upsert by `id`, monotonically ([STATE-001]). Specifically:
  - **Assistant "shell"** (role=assistant, no `id`, no `content`, no `parts`): a no-op. It is
    a turn-boundary marker; streamed content arrives via `part`. **Golden:**
    `clank-mobile/src/hooks/dispatch.ts:51`.
  - **Id-less user echo** (role=user, no `id`): if the latest transcript entry is already the
    same user text, drop it (it is the echo of the optimistic local message); otherwise it is
    a genuine message — keep it. **Golden:** `dispatch.ts:73`.
  - **User message with `id`**: backfill that `id` onto the most recent local user entry that
    lacks one (enables revert/fork actions on it). **Golden:**
    `internal/tui/sessionview.go:1470`.
  - **Assistant message with content/parts and an `id`**: merge into the existing message by
    id (don't shrink). After a full history load, a client MAY skip assistant `message` events
    entirely and rely on `part` events, since history already holds the committed message.
    **Golden:** `internal/tui/sessionview.go:1489` (post-history skip), `dispatch.ts:63`.

### `part` → [STATE-PART-001]
- **(MUST)** Resolve the owning message: use `message_id` if present; else, for Claude
  text/thinking parts, the owner is encoded in the part id as `{assistantMsgID}-{idx}`; else
  (tool parts with no owner) attach to the current (latest) assistant message, creating one if
  none exists yet. Then upsert the part by `part.id`:
  - **text/thinking, `is_delta=true`**: append `part.text` to the existing text; mark
    `streaming=true`.
  - **text/thinking, `is_delta=false`**: replace with the snapshot (monotonic: never shorter);
    mark settled.
  - **tool_call/tool_result**: merge — preserve existing `input`/`output`/`tool` when the
    update omits them; advance `status` monotonically.
  **Why:** this is the exact union of the three delta/merge invariants; getting any leg wrong
  corrupts streaming text or loses tool I/O. See [INV-DELTA-001], [INV-TOOL-MERGE-001],
  [INV-MSGID-001]. **Golden:** `internal/tui/sessionview.go:1622`–`:1681`,
  `clank-mobile/src/hooks/dispatch.ts:93`–`:149`.

### `permission` → [STATE-PERM-001]
- **(MUST)** Enqueue the `PermissionData` at the back of `pendingPermissions` and **lock the
  composer** (set `compose.active=false`) so answer keys/taps reach the permission affordance.
  The correlated tool-call part (same `tool_use_id` == part id) generally arrives in a
  *preceding* `part` event carrying the tool `input`; a client MUST be able to render the
  prompt's context from that part. **Why:** an unlocked composer lets keystrokes leak past a
  blocking decision; the prompt context lives in the part. **Golden:**
  `internal/tui/sessionview.go:1374`, `internal/agent/claude_permissions.go:58` (pre-emitted
  part), `:74` (permission), `dispatch.ts:152`.

### `error` → [STATE-ERR-001]
- **(MUST)** If `aborting`, **suppress** the error (it is abort noise, e.g.
  `MessageAbortedError`) — the cancel UX handles it. Otherwise record the error reason on the
  session for a recoverable banner ([VIEW-ERROR-001]). The reason MUST be cleared on the next
  non-error `status`. **Why:** surfacing abort errors as failures alarms the user; a stale
  reason must not outlive recovery. **Golden:** `internal/tui/sessionview.go:1385`,
  `dispatch.ts:33` (clear on recovery), `:180`.

### `title` / `meta` → [STATE-META-001 / STATE-META-002]
- **[STATE-META-001] (MUST)** On `title`, set `session.title`. **Golden:** `sessionview.go:1409`.
- **[STATE-META-002] (MUST)** On `meta`, **replace the entire cached `SessionInfo` wholesale**
  with `data.session`. A client MUST NOT field-merge or diff. **Why:** the event is designed
  as a single-replacement snapshot covering read/visibility/draft/follow-up/title/revert at
  once; per-field merging drifts. See [INV-META-REPLACE-001]. **Golden:** `internal/agent/agent.go:205`.

### `revert` → [STATE-REVERT-001]
- **(MUST)** Set `session.revert_message_id = data.message_id` (empty clears it), then refetch
  the transcript and apply the **revert filter**: drop the message whose id equals
  `revert_message_id` and everything after it. **Why:** the reverted tail must vanish from the
  display and must stay gone across refetches. See [INV-REVERT-001]. **Golden:**
  `internal/tui/sessionview.go:1416`, `:1535`, `dispatch.ts:165`.

### `session.create` / `session.delete` → [STATE-LIST-001]
- **(MUST)** Refresh the session list. A client viewing the deleted session SHOULD surface
  that it was deleted. These are accepted even on the global stream regardless of the open
  session ([EVT-005](04-event-protocol.md)). **Golden:** `dispatch.ts:190`,
  `internal/tui/sessionview.go:1401`.

## Transitions — operation results

- **[STATE-SEND-RESULT-001] (MUST)** On send completion, clear `compose.submitting`. On
  error, record it for display; do **not** remove the optimistic user message (it was
  dispatched-or-not, and history reconciliation will correct it). **Golden:** `sessionview.go:862`.
- **[STATE-PERM-RESULT-001] (MUST)** On reply completion, clear `replyInFlight`, pop the
  replied request from `pendingPermissions`, and record the outcome (allowed/denied). On a
  **denial**, pessimistically settle any still-`running`/`pending` tool parts to `error` (the
  backend may cancel the batch without per-tool updates) and reconcile via a messages +
  pending-permission refetch. **Golden:** `sessionview.go:869`–`:903`, `:1608`
  (`markRunningToolsFailed`).
- **[STATE-ABORT-RESULT-001] (MUST)** On abort dispatch, set `aborting=true` and show a
  "Cancelling…" marker. The actual settle is driven by the subsequent `status` event
  ([STATE-STATUS-001]), not the abort response. On abort *error*, clear `aborting` and mark
  the cancel failed. **Golden:** `sessionview.go:2203` (`startAbort`), `:952`.
- **[STATE-REVERT-RESULT-001] (MUST)** On revert success, set `session.revert_message_id`,
  prefill the composer with the reverted user prompt, activate + focus it, and refetch the
  filtered transcript. **Golden:** `sessionview.go:924`–`:938`.

## Transitions — user intents

- **[STATE-SUBMIT-001] (MUST)** On submit (non-empty text, not already submitting): append the
  user message **optimistically** (no id yet), **clear** `revert_message_id` (sending un-reverts
  the hidden tail), set `compose.submitting=true`, deactivate the composer, set `follow=true`,
  and dispatch send. The id is backfilled later by [STATE-MSG-001]. **Why:** optimistic echo is
  the perceived-latency win; clearing revert on send restores the previously-hidden messages.
  **Golden:** `internal/tui/sessionview.go:1149`–`:1176`.
- **[STATE-PERM-ANSWER-001] (MUST)** A permission answer is accepted **only** when
  `pendingPermissions` is non-empty, `replyInFlight == null`, and the composer is inactive.
  On answer, set `replyInFlight = frontRequestId` before dispatching the reply (single-flight).
  **Golden:** `internal/tui/sessionview.go:1099`–`:1110`.
- **[STATE-FOLLOW-001] (SHOULD)** `follow` is auto-enabled when a turn starts (`status=busy`)
  and when the user scrolls to the bottom; it is disabled when the user scrolls up. While
  `follow`, the viewport stays pinned to the latest content. **Golden:**
  `internal/tui/sessionview.go:1456`, mouse handlers.

## Transitions — lifecycle

- **[STATE-CONNECT-001] (MUST)** On opening a session: open the single SSE stream **and**
  fetch session info + transcript + pending-permission, then reconcile. For a *new* session,
  subscribe to the stream **before** issuing create ([INV-CREATE-RACE-001]). **Golden:**
  `internal/tui/sessionview.go:511` (Init order), `sessionview_compose.go:284`.
- **[STATE-RECONNECT-001] (MUST)** On detecting the stream closed ([EVT-004](04-event-protocol.md)),
  re-establish exactly one stream (with backoff) and **re-reconcile the transcript**
  ([EVT-010](04-event-protocol.md)) — reconnection without a refetch leaves missed events
  unrecovered. A client MUST discard events from a superseded stream (carry a stream identity
  and ignore stale-stream callbacks). **Why:** no replay means reconnect ⇒ refetch; stale-stream
  events re-arm duplicate readers. See [INV-RECONCILE-001], [INV-STALE-STREAM-001]. **Golden:**
  `internal/tui/sessionview.go:828`–`:860`.
- **[STATE-BACKGROUND-001] (SHOULD)** A mobile client SHOULD suspend the stream when
  backgrounded and, on foreground, reconnect + reconcile per [STATE-RECONNECT-001]. **Why:**
  battery; and a long-idle socket is likely half-open anyway. Cross-ref [NFR-BAT-001](09-non-functional.md).

## Derived view-projections (the observable surface)

These are pure functions of the canonical state. Conformance asserts on them.

- **[VIEW-INPUT-LOCK-001] (MUST)** The composer is **locked** (cannot send, does not capture
  text) whenever `pendingPermissions` is non-empty **or** `replyInFlight != null`. Only the
  permission-answer affordance and a cancel/abort affordance are active. **Golden:**
  `internal/tui/sessionview.go:1112`–`:1123`.
- **[VIEW-PENDING-PERM-001] (MUST)** When `pendingPermissions` is non-empty, the **front**
  entry is presented as the active prompt, shown with its correlated tool-call context
  (matched by `tool_use_id`). **Golden:** `internal/agent/claude_permissions.go:58`.
- **[VIEW-STREAMING-001] (MUST)** A part with `streaming=true` renders as incremental/raw; a
  settled part renders final (e.g. full markdown). The flip happens on settle ([STATE-STATUS-001]).
  **Golden:** `internal/tui/sessionview.go:1432`, `:1668`.
- **[VIEW-CANCELLING-001] (MUST)** While `aborting`, show a "Cancelling…" indicator that
  becomes "Cancelled" when the session settles; intermediate statuses and abort errors are not
  shown. **Golden:** `internal/tui/sessionview.go:1442`, `:2206`.
- **[VIEW-ERROR-001] (MUST)** A non-abort error shows as a **recoverable** banner/reason on the
  session, cleared on the next non-error status; it MUST NOT look like a terminal crash and
  MUST NOT appear during an abort. **Golden:** `dispatch.ts:180`, `:33`.
- **[VIEW-REVERT-PREFILL-001] (MUST)** After a revert, the composer is prefilled with the
  reverted user prompt and focused, and the reverted tail is hidden. **Golden:**
  `internal/tui/sessionview.go:934`.
