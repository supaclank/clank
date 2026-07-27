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
**Golden:** `clank-mobile/src/api/events.ts` (`connect()`: the double `closed`/generation
guard around the token await and the socket assignment — the reconnect loop generalizes the
original boolean to a generation counter so a superseded connect can also never attach).
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

### [INV-STREAM-SUPERVISE-001] (MUST) The stream reconnects forever; a dead stream is never accepted
Supervise the subscription: **every** termination path schedules a reconnect with capped
backoff, indefinitely, until deliberate teardown ([EVT-006], [NFR-REL-001]). Resubscribe on
return-to-foreground — a suspended app's socket can be a zombie that will never error, and
with no heartbeat ([INV-HEARTBEAT-GAP-001]) nothing else exposes it. Enumerate your SSE
library's kill paths and route each one to the supervisor — including any that fire **no
callback** (with `react-native-sse` + `pollingInterval: 0`, a clean server close is exactly
that; the golden client sets a small `pollingInterval` to delegate that one path to the
library's re-poll and owns every other path itself).
**Why:** mobile shipped the counterexample. Its stream died permanently on the first
transport failure whose single immediate retry also failed — both fell inside the same
outage (airplane toggle, Wi-Fi↔cellular handoff, Doze, gateway deploy, sprite suspend) —
and a clean server close killed it with no callback at all. The app then looked healthy
(HTTP fine) but heard nothing: a **new** session opened to its prompt plus an eternal
"Working…" (empty transcript, no `part`/`status` ever arriving), the session list stayed
stale (`session.create`/`status`/`meta` all lost), and only pull-to-refresh + re-entering
(plain HTTP refetch on mount) revealed the long-finished transcript.
**Golden:** `clank-mobile/src/api/events.ts` (`openEventStream`: generation-guarded
supervised loop, `scheduleReconnect`, `restart()`), `clank-mobile/src/hooks/useEventStream.ts`
(AppState foreground `restart()`; `onReconnect` → `resyncAfterStreamGap`),
`…/session/SessionEventStream.kt:121` (Kotlin 1s→15s backoff loop).
**Conformance:** `CONF-STREAM-SUPERVISE`.

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
A tool's `input` (on the `tool_call`) and `output` (on the later `tool_result`) arrive in
separate updates for the **same part ID**. Merge them — preserve existing `input`/`output`/
`tool` when an update omits them. Never replace-and-lose. The result also carries an **empty
`tool`** name, so the merge MUST keep the call's name or the card renders as a nameless
"tool". The two parts arrive in **separate messages** — see [INV-TOOL-RESULT-CARRIER-001].
**Why:** the result update carries no `input` (and no `tool`); a naive replace erases the
arguments and the name, so the tool card loses what it ran.
**Golden:** `internal/tui/sessionview.go:1698` (`upsertPartEntry`),
`clank-mobile/src/lib/mergeMessages.ts:46`.
**Conformance:** `CONF-TOOL-MERGE`.

### [INV-TOOL-RESULT-CARRIER-001] (MUST) Fold the tool-result carrier across messages
The `tool_call` and its `tool_result` share one part id but, in the committed transcript and
in `message` events, arrive in **separate messages**: the call in the assistant message, the
result in a **following `role=user` message** whose only payload is that `tool_result` part
(no text content, empty `tool`). A client MUST merge the two (by part id) into a **single
rendered tool card** at the call's position, and MUST drop the now-empty user-role carrier —
never render it as a user turn or as a second, nameless "tool" card.
**Why:** a per-message part renderer — or a monotonic merge that keeps the server's grouping —
shows the tool twice ("Edit" with the input, then a nameless "tool" with the output) plus a
phantom user bubble. The **live** `part` stream hides this because the id-less `tool_result`
part attaches to the current assistant message by [INV-MSGID-001]; but the **refetched**
transcript ([INV-MONOTONIC-001]) re-introduces the split, so the fold MUST run on the merged
transcript, not only on the live stream. Verified against the live gateway; the Kotlin
preview client doubled tool cards in history until it folded across messages (two earlier
"merge within one message" attempts were wrong — the split is cross-message).
**Golden:** `clank-mobile/modules/preview-launcher/android/…/session/ChatTranscript.kt`
(`foldToolResults`); the TUI folds by part id in its flat entry list
(`internal/tui/sessionview.go:1698`, `upsertPartEntry`); the wire shape is built by
`internal/agent/claude.go:1167` (`coalesceSessionMessages`) + `:1254` (`sessionBlockToPart`).
**Conformance:** `CONF-TOOL-MERGE-CROSSMSG`.

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
keep one (consume-once by content for id-less echoes). Dedup by **normalized text** (trim leading/trailing whitespace): the gateway preserves the trailing whitespace a user typed, while a
client typically trims when extracting a message's text, so a raw `==` leaves the echo as a
permanent duplicate. And the committed copy is **not necessarily the latest entry** — once the
agent starts streaming, assistant parts follow the user message — so dedup against **any**
recent user message, not just the transcript tail.
**Why:** optimistic echo is the perceived-latency win; without backfill, per-message actions
(revert/fork) never enable; without **normalized + position-agnostic** dedup, the echo sticks
to the bottom of the chat as a duplicate for the whole streaming turn (a shipped Kotlin bug,
traced to a single trailing space the gateway preserved but the client trimmed).
**Golden:** `internal/tui/sessionview.go:1165` (echo), `:1470` (backfill);
`clank-mobile/src/hooks/dispatch.ts:73` (dedup);
`clank-mobile/modules/preview-launcher/android/…/fab/PromptBoxContent.kt` (trimmed,
any-user-message dedup of the optimistic overlay). **Conformance:**
`CONF-OPTIMISTIC-BACKFILL`, `CONF-OPTIMISTIC-DEDUP-NORMALIZE`.

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
**Golden:** `internal/tui/sessionview.go:924` (deny path), `:1668` (`markRunningToolsFailed`).
**Conformance:** `CONF-DENY-SETTLE`.

### [INV-ABORT-PERM-001] (MUST) Abort clears pending permissions without breaking the session
Aborting denies all parked permissions server-side; the client MUST then clear its pending
queue and re-enable the composer, and the session survives (the agent re-prompts on the next
turn).
**Why:** an abort that left the composer locked or the session wedged was a shipped bug.
**Golden:** the host denies server-side — `internal/agent/claude_permissions.go:152`
(`failPendingPermissions`) — and emits **no** event to clear the client queue (only an
eventual `status → idle`), so the client clears it on that settle: the abort-settle in
`internal/tui/sessionview.go` (`handleStatusChange`) drops `pendingPerms`/`replyingPermID`, so
an abort while a prompt is pending no longer wedges the composer. **Conformance:**
`CONF-ABORT-PERM` (`internal/tui/sessionview_test.go`, `TestAbortClearsPendingPermissions`).

### [INV-ABORT-SETTLE-TOOLS-001] (MUST) On abort, settle still-running tools
When an abort settles (the post-abort `status ∉ {busy, starting}`), a client MUST mark every
still-`pending`/`running` tool part **terminal** — the interrupted tool never returns a
result, so its spinner otherwise runs forever. This is the abort analog of
[INV-DENY-SETTLE-001]. The wire has no `canceled` status; a client MAY render a neutral
"canceled" presentation but MUST treat it as **terminal in the monotonic rank** ([DATA-021])
so the post-abort transcript refetch — which still carries the tool as `running` — cannot
regress it.
**Why:** a tool left `running` after Stop spins indefinitely; and a "canceled" marker that is
not terminal-ranked is "advanced" back to `running` by the refetch's monotonic merge — a
shipped Kotlin bug (the spinner resumed after a cancel).
**Golden:** `clank-mobile/modules/preview-launcher/android/…/session/ChatTranscript.kt`
(`cancelPendingParts`; `canceled` ranked terminal in `statusRank`/`preferStatus`). The TUI does
the analogous settle on **deny** (`internal/tui/sessionview.go:1668` `markRunningToolsFailed`,
called at `:924`) but not on abort — this native client leads the abort case.
**Conformance:** `CONF-ABORT-SETTLE-TOOLS`.

### [INV-ABORT-DONE-001] (MUST) Suppress "turn complete" for idles that follow an abort
A client that surfaces a "turn complete / done" affordance on `status → idle` MUST suppress it
for **every** idle that follows an abort, until the user's **next send** — not merely the first
settle. A one-shot `aborting` flag (cleared on the first settle, [STATE-ABORT-RESULT-001]) is
**insufficient**: in practice a **second** settle is observed after a stop — the interrupt's own
result settles to idle (`internal/agent/claude.go:945`, `handleResult` abort branch) and
on-device a further `status` still arrives as the turn unwinds — and that later idle flashes a
misleading "Done". Track a `stoppedSinceLastSend` flag (set on abort, cleared on the next send)
and gate the done affordance on it. (`[Request interrupted by user]` is written into the
transcript by the Claude CLI, **not** emitted by clank — don't treat it as a signal.)
**Why:** a "Done" banner moments after the user pressed Stop misrepresents a canceled turn as
a completed one (a shipped Kotlin bug, seen in PR #78 device testing).
**Golden:** `clank-mobile/modules/preview-launcher/android/…/fab/PreviewOverlayState.kt`
(`stoppedSinceLastSend`). **Conformance:** `CONF-ABORT-DONE-SUPPRESS`.

---

## Permission modes

### [INV-PERMMODE-001] (MUST) an omitted `config` key means "no change"
(0.6.2: `permission_mode` became `config`'s `mode` key — [DATA-040](03-data-model.md).)
Omit `config` (or the `mode` key) unless the user just changed the mode. Never send a
concrete default to mean "unchanged".
**Why:** any non-empty value re-asserts the mode on the backend, silently flipping a
`plan`/`acceptEdits` session to whatever you sent.
**Golden:** `internal/agent/agent.go:571`, `internal/tui/sessionview.go:2162`.
**Conformance:** `CONF-PERMMODE-NOCHANGE`.

### ~~[INV-PERMMODE-EXITPLAN-001]~~ (retired 0.6.0 — M3) (MUST) ExitPlanMode is approve/reject; mode self-resets
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

### ~~[INV-REVERT-001]~~ (retired 0.6.0 — M3) (MUST) Filter the transcript by `revert_message_id`
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
**Golden:** `internal/tui/sessionview.go:511`; `clank-mobile/src/hooks/useEventStream.ts`
(`onReconnect` → `resyncAfterStreamGap`). **Conformance:** `CONF-RECONCILE`.

### [INV-RECONNECT-SEMANTICS-001] (MUST) `reconnected` is the backend's link, not your socket
The `reconnecting`/`reconnected` events describe the host↔backend link, delivered over your
stream. Do not treat `reconnected` as "my SSE came back" and do not rely on it to trigger
recovery after your own transport drop (which emits no event, since the socket was down).
**Why:** conflating them means a real client-side blip never triggers a resync, leaving a
stale transcript.
**Golden:** `internal/agent/agent.go:297`; `clank-mobile/src/hooks/dispatch.ts`
(`resyncAfterStreamGap` — run from the `reconnected` event *and*, separately, from the
client's own transport reconnect via `useEventStream`'s `onReconnect`).
**Conformance:** `CONF-RECONNECT-SEMANTICS`.

### [INV-DEAD-BACKEND-REHYDRATE-001] (MUST, host) A dropped backend connection is `dead`, not reused
When a session backend loses its connection to the agent process (the agent message stream
closes), the host MUST set status `dead` regardless of the turn's prior status, and the next
operation that needs the backend (`/message`, `/abort`, …) MUST tear the dead backend down and
lazily rehydrate a fresh one (resume via the stored external id) rather than dispatch into the
dead one. A failed dispatch MUST surface a reason ([STATE-ERR-001](06-state-model.md)), never a
silent status flip. The messages GET is not such an operation — it is a pure read that serves
history without repairing the registry ([OP-010](05-operations.md)); rehydration stays with the
dispatching ops.
**Why:** an interrupt issued the instant a turn starts can leave the agent process exited while
the session still reads `idle`; reusing that backend fails every subsequent dispatch
(`client not connected`), flips the session to `error` with no recovery path, and — since a chat
client uses only `/message` + `/abort` and relies on lazy rehydration ([05](05-operations.md)) —
leaves it wedged until a daemon restart. The fix mirrors the Open-failure teardown already in
`ensureBackend`.
**Golden:** `internal/agent/claude.go` (receiveLoop marks `dead` on stream close; Send/OpenAndSend
emit an error reason on dispatch failure), `internal/host/service.go` (`ensureBackend` drops a
`dead` backend and recreates).
**Conformance:** host regression tests — `internal/agent/claude_status_regression_test.go`
(`TestConnectionClosedWhileIdle_MarksDead`, `TestSendDispatchFailure_EmitsErrorReason`),
`internal/host/ensure_backend_test.go` (`TestEnsureBackend_DeadBackendIsRehydrated`),
`internal/host/session_messages_test.go` (`TestSessionMessages_DeadBackendServedFromTranscript`
— the read-path half: a fetch serves history without rehydrating).

---

## Interactive tools & session list

### ~~[INV-INTERACTIVE-001]~~ (retired 0.6.0 — M3: question tags and plan review went with the ACP migration; inline comments live on in 11) (MUST) Render interactive tools as structured UI
Render questions from the `part.question` tag ([QST-001](11-interactive-tools.md)) and plans
(`ExitPlanMode`) from the `tool_call` part `input`, and submit answers per
[11](11-interactive-tools.md). A client MUST NOT present a question as a bare allow/deny
permission when it understands the tag ([QST-003] suppression). Legacy clients that predate
the tag fall back to tool-name matching on the part input (RN reference:
`clank-mobile/src/lib/askQuestion.ts`, `…/planReview.ts`).
**Why:** a client that treats them as opaque tool cards loses the plan/question UX.
**Golden:** `internal/tui/sessionview_question.go` (questions), `clank-mobile/src/lib/planReview.ts`
(plan review), `…/chatReview.ts` (inline comments).
**Conformance:** `CONF-QUESTION-TAG`, `CONF-INTERACTIVE-ASK`, `CONF-INTERACTIVE-PLAN`, `CONF-INLINE-COMMENT`.

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
([EVT-012], [INV-RECONCILE-001]). On suspendable platforms, where no liveness signal is
available at all, resubscribe on return-to-foreground instead ([EVT-006],
[INV-STREAM-SUPERVISE-001]).
**Recommended host fix:** emit a periodic SSE comment (`:\n\n`) as a heartbeat.
**Golden:** `internal/host/mux/events.go:45` (no heartbeat write). **Conformance:** covered by
`CONF-RECONCILE` under an injected stall.

### [INV-NO-RESUME-GAP-001] (known limitation) No resumable stream
There is no `Last-Event-ID` / cursor; reconnection always falls back to a full transcript
refetch. This is correct but heavier than a resumable stream.
**Recommended host fix:** event IDs + `Last-Event-ID` resume would let a client recover the
gap without a full refetch. Not required for conformance today.
