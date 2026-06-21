# 10 · Conformance

A client conforms when, for every scenario below, replaying the scenario's input trace
through the client's reducer yields the expected canonical state and view-projection. This
is mechanical and language-agnostic: it tests the [state model](06-state-model.md), not
pixels.

## The harness contract

Each client exposes a thin test-only shim around its reducer ([NFR-TEST-001](09-non-functional.md)):

```
applyInput(state, input) -> state      // the client's reducer, one input at a time
project(state)           -> view       // the derived view-projection (06 §projections)
```

A scenario runner (per platform, but identical logic) does:

```
state = initialState(scenario.setup)
for input in scenario.inputs:
    state = applyInput(state, input)
assert deepEqual(normalize(project(state)), scenario.expect.view)
assert deepEqual(normalize(state.transcript), scenario.expect.transcript)   // when given
```

No network, no clock, no real SSE socket — inputs are fed directly. `normalize` drops
platform-only fields (render caches, scroll pixels) and compares the **observable**
projection: transcript (messages→parts with text/status/tool I/O), `pendingPermissions`,
`session.status`/`title`/`revert_message_id`, and the boolean view flags
(`composerLocked`, `streaming`, `cancelling`, `errorBanner`).

## Input trace format

A fixture is JSON. `inputs` is an ordered list of tagged inputs mirroring the reducer's four
input kinds ([06](06-state-model.md)):

```json
{
  "id": "CONF-STREAM-DELTA",
  "description": "is_delta append vs snapshot replace",
  "rules": ["EVT-012", "INV-DELTA-001", "STATE-PART-001"],
  "setup": { "session": { "id": "s1", "backend": "claude-code", "status": "busy" } },
  "inputs": [
    { "kind": "event", "event": { "type": "part", "session_id": "s1",
      "data": { "part": { "id": "m1-0", "type": "text", "text": "Hel" }, "is_delta": true } } },
    { "kind": "event", "event": { "type": "part", "session_id": "s1",
      "data": { "part": { "id": "m1-0", "type": "text", "text": "lo" }, "is_delta": true } } },
    { "kind": "event", "event": { "type": "part", "session_id": "s1",
      "data": { "part": { "id": "m1-0", "type": "text", "text": "Hello world" }, "is_delta": false } } },
    { "kind": "event", "event": { "type": "status", "session_id": "s1",
      "data": { "old_status": "busy", "new_status": "idle" } } }
  ],
  "expect": {
    "transcript": [ { "role": "assistant", "parts": [ { "id": "m1-0", "type": "text", "text": "Hello world" } ] } ],
    "view": { "streaming": false, "composerLocked": false }
  }
}
```

Input kinds:
- `{ "kind": "event", "event": <Event> }` — an SSE event ([04](04-event-protocol.md)).
- `{ "kind": "op_result", "op": "send|reply|abort|revert|create", "ok": bool, "data": … }`.
- `{ "kind": "intent", "intent": "submit|answerPermission|abort|revert|scroll", "args": … }`.
- `{ "kind": "lifecycle", "event": "connect|disconnect|reconnect|foreground|background", "args": … }`.
  A `reconnect` that should recover state carries the `messages` the subsequent refetch
  returns, so the fixture can assert reconciliation without a live network.

## Fixture sources

Fixtures are not invented — they are captured or lifted:
- **Captured** from a real `GET /events` session (record the frames) plus the matching
  `GET /sessions/{id}/messages` snapshot for the refetch inputs.
- **Lifted** from the golden tests that already encode the hard cases:
  `internal/tui/session_sse_stale_test.go` (stale-stream, doubled-reader),
  `internal/tui/sessionview_test.go` (pending-permission restore, optimistic backfill,
  revert filter, abort), `internal/host/events_test.go` (buffer/close),
  `clank-mobile/src/hooks/__tests__/dispatch.test.ts` and `…/lib/__tests__` (delta, monotonic
  merge, shell drop).

Fixtures will live in `docs/chat-client-spec/fixtures/<CONF-ID>.json` (the directory is not
yet checked in; it grows as scenarios are authored). Each client's harness loads the same
files.

## Scenario matrix

| Scenario | Asserts | Guards |
|---|---|---|
| `CONF-CREATE-RACE` | Events emitted between subscribe and create-ack still land in state | INV-CREATE-RACE-001 |
| `CONF-STALE-STREAM` | Events/closes from a superseded stream don't mutate live state or re-arm a reader | INV-STALE-STREAM-001 |
| `CONF-SINGLE-STREAM` | A teardown during async connect leaves zero live sockets; no event is delivered twice | INV-SSE-DOUBLE-001, NFR-CONC-002 |
| `CONF-NO-END` | Stream close (EOF) ends the stream; no `end` event is awaited | INV-NO-END-001 |
| `CONF-STREAM-DELTA` | delta appends, snapshot replaces, settles on idle | INV-DELTA-001, STATE-PART-001 |
| `CONF-TOOL-MERGE` | input (call) + output (result) for one part id both survive | INV-TOOL-MERGE-001, DATA-022 |
| `CONF-MONOTONIC` | a lagging refetch mid-stream does not shrink/erase streamed content | INV-MONOTONIC-001, NFR-CONS-001 |
| `CONF-MSGID-OWNER` | id-less parts attach to the right assistant message | INV-MSGID-001 |
| `CONF-SHELL-DROP` | empty assistant shell renders nothing; post-history message shells dedup | INV-SHELL-001 |
| `CONF-OPTIMISTIC-BACKFILL` | optimistic user echo gets its id backfilled; no duplicate | INV-OPTIMISTIC-001 |
| `CONF-PERM-LOCK` | composer locks while a permission is pending | VIEW-INPUT-LOCK-001 |
| `CONF-PERM-SINGLEFLIGHT` | a double answer sends exactly one reply | INV-PERM-SINGLEFLIGHT-001 |
| `CONF-DENY-SETTLE` | on deny, running tools settle to error and state reconciles | INV-DENY-SETTLE-001 |
| `CONF-ABORT-PERM` | abort clears pending perms, unlocks composer, session survives | INV-ABORT-PERM-001 |
| `CONF-ABORT-NOISE` | during abort, intermediate statuses/errors are suppressed; "Cancelling"→"Cancelled" | STATE-STATUS-001, VIEW-CANCELLING-001 |
| `CONF-PERMMODE-NOCHANGE` | an unchanged-mode send carries `permission_mode:""` | INV-PERMMODE-001 |
| `CONF-PLAN-EXIT` | ExitPlanMode renders the plan + approve/reject; later sends stay `""` | INV-PERMMODE-EXITPLAN-001, FLOW-PLAN-001 |
| `CONF-META-REPLACE` | a `meta` event replaces the whole SessionInfo (no field-merge) | INV-META-REPLACE-001 |
| `CONF-REVERT-FILTER` | revert hides the tail; a new send un-hides it | INV-REVERT-001 |
| `CONF-RECONCILE` | reconnect + refetch recovers events missed during the gap | INV-RECONCILE-001, EVT-010 |
| `CONF-RECONNECT-SEMANTICS` | own-transport reconnect triggers reconcile; `reconnected` event alone does not substitute | INV-RECONNECT-SEMANTICS-001 |
| `CONF-PENDING-PERM-GAP` | a join to a blocked session is surfaced honestly (not shown as plain "working") | INV-PENDING-PERM-GAP-001 |
| `CONF-INTERACTIVE-ASK` | AskUserQuestion renders from part input; terminal status clears; answer submitted | ITOOL-001/002/004, FLOW-ASK-001 |
| `CONF-INTERACTIVE-PLAN` | ExitPlanMode plan renders; Approve→build, Revise→plan | ITOOL-004/005, FLOW-PLAN-001 |
| `CONF-INLINE-COMMENT` | per-block comments format as a quoted-comment SendMessage | ICOMMENT-001 |
| `CONF-SIDEBAR-SYNC` | list updates from `meta` + create/delete (not field-level); reconnect resyncs | INV-SIDEBAR-META-001, LIST-002/003/004/006 |

## Acceptance gates

- **[CONF-GATE-001] (MUST)** Every `MUST` rule MUST map to existing behavior in **some**
  reference client — the TUI for the core event/state machine, the RN client for interactive
  tools ([11](11-interactive-tools.md)). A `MUST` that **no** reference client satisfies is a
  spec bug — resolve before shipping the rule. `implementations/tui-go.md` records which rules
  the TUI is the reference for (and which it deliberately doesn't satisfy).
- **[CONF-GATE-002] (MUST)** A client claims conformance only when it passes every
  `MUST`-guarding scenario in the matrix. `SHOULD` scenarios are tracked per-client in its
  checklist with a rationale for any gap.
- **[CONF-GATE-003] (the real test)** The end-to-end acceptance: hand this spec +
  `implementations/_template.md` to a fresh implementer (human or agent), have them build a
  client, and run the fixtures. **Zero divergence from the projections = success.** Any
  divergence is either a client bug or a missing/ambiguous rule — both get fixed here.
