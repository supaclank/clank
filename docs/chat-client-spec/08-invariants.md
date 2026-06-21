# 08 · Invariants & Known-Bug Catalog

The regression firewall. Each entry is a behavior that was *wrong at least once* in some
client and is now fixed in the golden one. A client that violates any of these will
reproduce a bug we have already paid for. Every entry cites the **Why** (the bug), the
**Golden** source, and the **Conformance** scenario that guards it.

These restate, in one place, the strictest rules scattered across [04](04-event-protocol.md)–[07](07-lifecycle-flows.md).

---

## Stream & connection

### [INV-CREATE-RACE-001] (MUST) Subscribe before you create
Open the SSE stream **before** issuing `POST /sessions`; bind the new session's events to
that already-open stream.
**Why:** the host emits `starting→busy` and early `part` events during creation. With no
replay ([EVT-010]), subscribing after create loses them — the session looks stuck.
**Golden:** `internal/tui/sessionview_compose.go:284` (`createSessionCmd`: Subscribe then
Create). **Conformance:** `CONF-CREATE-RACE`.

### [INV-STALE-STREAM-001] (MUST) Carry stream identity; ignore stale streams
Tag each event and each stream-closed signal with the identity of the stream it came from.
Apply/handle only those from the **current** stream. On a stale-stream closed signal, do not
null the live stream; on a stale-stream event, do not re-arm the reader.
**Why:** switching sessions (or reconnecting) leaves an old reader briefly alive; its
late EOF can blank the live stream, and re-arming on its events spawns a duplicate reader
that races the real one.
**Golden:** `internal/tui/sessionview.go:828` (closed-signal identity check), `:857`
(re-arm only on live stream). **Conformance:** `CONF-STALE-STREAM`.

### [INV-SSE-DOUBLE-001] (MUST) Exactly one stream; guard teardown-vs-connect
Maintain one stream per scope. When connect is async (it awaits a token), re-check the
"closed" flag *after* the token resolves and *after* assigning the socket; if closed
meanwhile, close the socket instead of leaving it live.
**Why:** a teardown that runs while the token fetch is in flight sees a still-null socket
and no-ops; the later-created socket then leaks → **two** live connections delivering every
event **twice**.
**Golden:** `clank-mobile/src/api/events.ts:71`–`:98` (double `if (closed)` guard).
**Conformance:** `CONF-SINGLE-STREAM`.

### [INV-NO-END-001] (MUST) Detect end-of-stream from the closed connection, not an `end` frame
Detect end-of-stream from the connection closing (read returns EOF/null); never *depend* on
an application-level `end` event. The **global** `/events` stream sends no terminal frame at
all. The **per-session** `/sessions/{id}/events` stream emits `event: end` — but only on host
shutdown — so a consumer of it MAY treat `end` as an explicit "host going away" signal yet
MUST still tear down on the raw connection close.
**Why:** a consumer that *waits* for `end` never tears down on a silent close (the common
case). Note: the Kotlin preview consumer subscribes to the **per-session** stream, so its
`"end"` branch is **live, correct code** (it fires on host shutdown) — *not* a phantom; an
earlier draft wrongly called it dead by conflating the two endpoints.
**Golden:** `internal/host/mux/events.go` (`handleEvents`: global loop returns on channel
close, no terminal frame), `internal/host/mux/sessions.go` (`handleSessionEvents`: emits
`event: end` on shutdown); `…/session/SessionEventStream.kt` (`"end"` branch — correct for the
per-session stream). **Conformance:** `CONF-NO-END`.

---

## Streaming & transcript

### [INV-DELTA-001] (MUST) `is_delta` decides append vs replace
`is_delta=true` → append the chunk to the existing part text. `is_delta=false` → replace
with the snapshot (and treat as settled). Apply this in the dispatch layer, before any
length-based reconciliation.
**Why:** treating a delta as a replace loses already-shown text; treating a snapshot as an
append duplicates it. The snapshot is also the only self-heal after a dropped delta.
**Golden:** `internal/tui/sessionview.go:1665`, `clank-mobile/src/hooks/dispatch.ts:134`.
**Conformance:** `CONF-STREAM-DELTA`.

### [INV-TOOL-MERGE-001] (MUST) Merge tool `input` and `output`
A tool's `input` (on the call) and `output` (on a later result) arrive in separate updates
for the same part ID. Merge them — preserve existing `input`/`output`/`tool` when an update
omits them. Never replace-and-lose.
**Why:** the result update carries no `input`; a naive replace erases the arguments, so the
tool card loses what it ran.
**Golden:** `internal/tui/sessionview.go:1649`, `clank-mobile/src/lib/mergeMessages.ts:46`.
**Conformance:** `CONF-TOOL-MERGE`.

### [INV-MONOTONIC-001] (MUST) Updates are monotonic; refetch never shrinks live state
An update may add or grow; it may never remove a message/part, shorten text, or regress a
status. A transcript refetch ([OP-006]) is reconciled into live state by this rule, not by
wholesale replace.
**Why:** the transcript is committed asynchronously, so a mid-turn refetch returns a state
*behind* the stream (often just the empty assistant shell). A wholesale replace wipes
streamed content — the "text/tool flashes then disappears" bug.
**Golden:** `clank-mobile/src/lib/mergeMessages.ts` (whole file; `mergeMessageLists`).
**Conformance:** `CONF-MONOTONIC`.

### [INV-MSGID-001] (MUST) Parts may have no `message_id`; resolve the owner
Resolve a part's owning message as: `message_id` if present; else the id encoded in a
text/thinking part id (`{assistantMsgID}-{idx}`); else the current (latest) assistant
message, creating one if needed. Tool parts attach to the current assistant message.
**Why:** the Claude backend leaves `message_id` empty and streams parts before the message
exists; without owner resolution, parts orphan or never render.
**Golden:** `clank-mobile/src/hooks/dispatch.ts:104`, `clank-mobile/src/lib/mergeMessages.ts:106`
(`apiMsgIdFromPartId`). **Conformance:** `CONF-MSGID-OWNER`.

### [INV-SHELL-001] (MUST) Drop the empty assistant shell; dedup replayed messages
The streaming assistant "shell" (role=assistant, no id, no content, no parts) is a turn
marker — render nothing for it. After a full history load, suppress redundant `message`
shells the stream re-delivers (parts arrive via `part`).
**Why:** rendering the shell adds a blank assistant row; re-rendering post-history messages
duplicates the transcript.
**Golden:** `clank-mobile/src/hooks/dispatch.ts:51`, `internal/tui/sessionview.go:1489`
(`historyLoaded` skip). **Conformance:** `CONF-SHELL-DROP`.

### [INV-OPTIMISTIC-001] (MUST) Optimistic user echo + id backfill + dedup
Render the user's message immediately (no id). Backfill the server id when the matching
`message` event arrives. When the id-less echo and the server copy would both be present,
keep one (consume-once by content for id-less echoes).
**Why:** optimistic echo is the perceived-latency win; without backfill, per-message actions
(revert/fork) never enable; without dedup, the message shows twice.
**Golden:** `internal/tui/sessionview.go:1165` (echo), `:1470` (backfill);
`clank-mobile/src/hooks/dispatch.ts:73` (dedup). **Conformance:** `CONF-OPTIMISTIC-BACKFILL`.

---

## Permissions

### [INV-PERM-SINGLEFLIGHT-001] (MUST) Single-flight the reply; lock the composer
While a permission is pending or a reply is in flight, lock the composer and accept only an
answer (and cancel). Set a `replyInFlight` guard before dispatching so a double-tap can't
send two replies.
**Why:** an unlocked composer leaks keystrokes past a blocking decision; a double reply races
the backend.
**Golden:** `internal/tui/sessionview.go:1099`–`:1123`. **Conformance:** `CONF-PERM-SINGLEFLIGHT`,
`CONF-PERM-LOCK`.

### [INV-DENY-SETTLE-001] (MUST) On deny, settle running tools and reconcile
When the user denies, pessimistically mark still-`pending`/`running` tool parts as `error`
and refetch messages + pending-permission.
**Why:** the backend may cancel the whole tool batch without emitting per-tool error updates,
leaving spinners running forever.
**Golden:** `internal/tui/sessionview.go:899`, `:1608` (`markRunningToolsFailed`).
**Conformance:** `CONF-DENY-SETTLE`.

### [INV-ABORT-PERM-001] (MUST) Abort clears pending permissions without breaking the session
Aborting denies all parked permissions server-side; the client MUST then clear its pending
queue and re-enable the composer, and the session survives (the agent re-prompts on the next
turn).
**Why:** an abort that left the composer locked or the session wedged was a shipped bug.
**Golden / known gap:** the host denies server-side —
`internal/agent/claude_permissions.go:152` (`failPendingPermissions`) — but it emits **no**
event telling the client to clear its queue (only an eventual `status → idle`). The
**client-side clear is currently unmet by the golden TUI**: `internal/tui/sessionview.go`
(`sessionAbortResultMsg`, and the abort-settle in `handleStatusChange`) does not clear
`pendingPerms` or refetch pending-permission, so an abort *while a prompt is pending* leaves
the composer wedged — a live instance of the very bug this rule names. Per
[CONF-GATE-001](10-conformance.md) this is a spec-vs-code gap to **resolve by fixing the
client** (clear the queue / refetch pending-permission on abort-settle), not by relaxing the
rule. **Conformance:** `CONF-ABORT-PERM`.

---

## Permission modes

### [INV-PERMMODE-001] (MUST) `permission_mode: ""` means "no change"
Send `permission_mode: ""` (or omit it) unless the user just changed the mode. Never send a
concrete default to mean "unchanged".
**Why:** any non-empty value re-asserts the mode on the backend, silently flipping a
`plan`/`acceptEdits` session to whatever you sent.
**Golden:** `internal/agent/agent.go:571`, `internal/tui/sessionview.go:2162`.
**Conformance:** `CONF-PERMMODE-NOCHANGE`.

### [INV-PERMMODE-EXITPLAN-001] (MUST) ExitPlanMode is approve/reject; mode self-resets
Render `ExitPlanMode` from its tool-call part `input` (the plan) as an approve/reject
decision. After approval the backend exits plan mode and resets its tracked mode; the client
keeps sending `""` until the user changes the mode explicitly.
**Why:** treating it as an opaque tool hides the plan; re-sending a non-empty default would
re-flip the mode.
**Golden:** `internal/agent/claude_permissions.go:97`. **Conformance:** `CONF-PLAN-EXIT`.

---

## Metadata & revert

### [INV-META-REPLACE-001] (MUST) `meta` replaces the whole `SessionInfo`
On a `meta` event, replace the entire cached `SessionInfo` with `data.session`. Do not
field-merge.
**Why:** the event is a single-replacement snapshot (read/visibility/draft/follow-up/title/
revert at once); per-field merging drifts from the host.
It is also the **session-list** row-sync signal ([INV-SIDEBAR-META-001], [12](12-session-list.md));
the RN client currently has no `meta` handler (divergence).
**Golden:** `internal/agent/agent.go:205`. **Conformance:** `CONF-META-REPLACE`.

### [INV-REVERT-001] (MUST) Filter the transcript by `revert_message_id`
When `revert_message_id` is set, drop the message with that id and everything after it, on
the live view and on every refetch. A new send clears it (the tail reappears).
**Why:** the reverted tail must vanish and stay gone until the user resends.
**Golden:** `internal/tui/sessionview.go:1535`, `:1162` (clear on send). **Conformance:**
`CONF-REVERT-FILTER`.

---

## Reconnect & recovery

### [INV-RECONCILE-001] (MUST) Reconcile via refetch on every (re)connection
After every SSE (re)connection — first open, reconnect, foreground — refetch the transcript
and reconcile monotonically. Drive this from your own transport state.
**Why:** at-most-once + no replay ([EVT-010]) means a gap loses events; only a refetch
recovers them.
**Golden:** `internal/tui/sessionview.go:511`. **Conformance:** `CONF-RECONCILE`.

### [INV-RECONNECT-SEMANTICS-001] (MUST) `reconnected` is the backend's link, not your socket
The `reconnecting`/`reconnected` events describe the host↔backend link, delivered over your
stream. Do not treat `reconnected` as "my SSE came back" and do not rely on it to trigger
recovery after your own transport drop (which emits no event, since the socket was down).
**Why:** conflating them means a real client-side blip never triggers a resync, leaving a
stale transcript.
**Golden:** `internal/agent/agent.go:297`; current mobile behavior reconciles on this event
but does not cover its own transport reconnect (`clank-mobile/src/hooks/dispatch.ts:196`).
**Conformance:** `CONF-RECONNECT-SEMANTICS`.

---

## Interactive tools & session list

### [INV-INTERACTIVE-001] (MUST) Render interactive tools; the TUI is not golden here
Render `AskUserQuestion` / `ExitPlanMode` (and OpenCode's `question`) as structured UI from
the `tool_call` part `input`, and submit answers per [11](11-interactive-tools.md). The TUI
renders **no** interactive UI for these — the **RN client is the reference**; do not infer
from the TUI's absence that no UI is required. Identifying these by tool name is a known hack
([11 open questions](11-interactive-tools.md#open-design-questions-non-normative)).
**Why:** a client that treats them as opaque tool cards loses the plan/question UX the mobile
client already ships.
**Golden:** `clank-mobile/src/lib/askQuestion.ts`, `…/planReview.ts`, `…/chatReview.ts`.
**Conformance:** `CONF-INTERACTIVE-ASK`, `CONF-INTERACTIVE-PLAN`, `CONF-INLINE-COMMENT`.

### [INV-SIDEBAR-META-001] (MUST) Drive the session list from `meta`, not field-level events
A session list/sidebar MUST update rows from `meta` (+ `session.create`/`session.delete`),
not from the field-level `status`/`title` events. Only `meta` carries the bumped `updated_at`
the sort depends on.
**Why:** patching a row from `status`/`title` updates a field but leaves the sort stale — the
session that just got activity fails to hoist. RN diverges (no `meta` handler; patches from
`status`/`title` + list invalidation).
**Golden:** `internal/tui/inbox_sse.go:23`, `:149`. **Conformance:** `CONF-SIDEBAR-SYNC`.

## Known limitations & candidate host fixes

These are real gaps in the host today. Clients must cope; the host should fix.

### [INV-PENDING-PERM-GAP-001] (known limitation) Pending permissions don't survive a (re)join
`GET /sessions/{id}/pending-permission` always returns `[]` ([OP-007]), and the SSE
`permission` event is not replayed. A client that opens or reconnects to a session **blocked
on a permission** cannot recover the prompt — the agent stays parked and the session looks
idle.
**Client duty:** call the endpoint anyway; do not assume `[]` ⇒ "never blocked"; if a session
reads `busy`/`starting` for a long time with no activity, surface it honestly rather than as
"working".
**Recommended host fix:** snapshot pending permissions (return them from the endpoint) **or**
re-emit them to a new subscriber on connect.
**Golden:** `internal/host/mux/sessions.go:236`. **Conformance:** `CONF-PENDING-PERM-GAP`
(documents the limitation; the assertion is the honest-surfacing behavior).

### [INV-HEARTBEAT-GAP-001] (known limitation) No heartbeat
The stream sends no keepalive/heartbeat, and the reference clients set no SSE read timeout.
A half-open TCP connection can therefore go undetected, freezing the UI on stale state.
**Client duty:** enable transport keepalive or a liveness timeout, then reconnect + reconcile
([EVT-012], [INV-RECONCILE-001]).
**Recommended host fix:** emit a periodic SSE comment (`:\n\n`) as a heartbeat.
**Golden:** `internal/host/mux/events.go:45` (no heartbeat write). **Conformance:** covered by
`CONF-RECONCILE` under an injected stall.

### [INV-NO-RESUME-GAP-001] (known limitation) No resumable stream
There is no `Last-Event-ID` / cursor; reconnection always falls back to a full transcript
refetch. This is correct but heavier than a resumable stream.
**Recommended host fix:** event IDs + `Last-Event-ID` resume would let a client recover the
gap without a full refetch. Not required for conformance today.
