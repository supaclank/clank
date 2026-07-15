# Implementation Checklist — Android (Kotlin)

> **Scope note.** The Kotlin client is the native **preview-overlay floating box**
> (`clank-mobile/modules/preview-launcher/android/…/`) that floats over a running preview app.
> As of clank-mobile PR #78 it grew from a *subset* "agent working / last action / answer-this-question"
> chip into a **near-full chat consumer**: a structured streaming transcript (text / thinking /
> tool cards), monotonic merge, reconnect-reconcile, stop/abort, and revert. It is still a
> constrained surface (a ~280 dp panel, light-mode only, no session list), and the full-screen
> mobile chat remains the [React Native client](react-native-ts.md). This file tracks the
> Kotlin consumer's conformance and the **hard-won lessons** it paid for — several of which
> hardened the spec itself ([INV-TOOL-RESULT-CARRIER-001](../08-invariants.md), [INV-ABORT-SETTLE-TOOLS-001](../08-invariants.md),
> [INV-ABORT-DONE-001](../08-invariants.md), and the [INV-OPTIMISTIC-001](../08-invariants.md) normalization clause).

**Platform:** Kotlin / Jetpack Compose / OkHttp / Coroutines (Expo native module) ·
**Client kind:** **near-full consumer** (preview floating box) · **Spec version:** 0.3.0 ·
**Last updated:** 2026-06-30 (PR #87)

## Where the pieces live

| Concern | Component |
|---|---|
| SSE stream | `…/session/SessionEventStream.kt` (OkHttp, hand-rolled frame parser) |
| HTTP — messages / send / abort / revert / images | `…/session/SessionClient.kt` |
| Transcript model + merge/reduce | `…/session/ChatTranscript.kt` — ports `clank-mobile/src/lib/mergeMessages.ts` + `src/hooks/dispatch.ts`: `mergePart`/`mergeMessageLists` (monotonic), delta/snapshot, `applyPart`/`applyMessage`, **`foldToolResults`**, **`cancelPendingParts`**, `preferStatus` (incl. `canceled`) |
| Transcript rendering | `…/fab/PromptBoxContent.kt` (`InlineChatList`), `…/fab/TranscriptCards.kt` (thinking / tool cards) |
| Abort / revert / "Done" state | `…/fab/PreviewOverlayState.kt` (`aborting`, `stoppedSinceLastSend`, `revertMessageId`), `…/PreviewOverlayContainer.kt` (wiring) |
| Question parsing | `…/session/AskQuestion.kt`, `parseAskQuestions` |
| Health probe | `…/session/PreviewServerHealth.kt` |

## Invariants

| Rule | Status | How / divergence |
|---|---|---|
| INV-CREATE-RACE-001 | N/A | overlay attaches to an existing session; lazy-creates on first send but never races a subscribe |
| INV-STALE-STREAM-001 | ✅ | `activeCall` tracked; `stop()` cancels the socket |
| INV-SSE-DOUBLE-001 | ✅ | `start()` cancels the prior job before relaunch |
| INV-NO-END-001 | ✅ | detects close via `readLine() ?: break`; also handles `event: end` — **correct**, this client subscribes to the per-session `/sessions/{id}/events`, which emits `end` on host shutdown |
| INV-DELTA-001 | ✅ | `applyPart` appends on `is_delta=true`, replaces on snapshot (was chip-only) |
| INV-TOOL-MERGE-001 | ✅ | `mergePart` keeps `input`/`output`/`tool` by part id |
| INV-TOOL-RESULT-CARRIER-001 | ✅ | `foldToolResults` merges the call+result across messages and drops the user-role carrier — **this client found the bug** (doubled tool cards in history) |
| INV-MONOTONIC-001 | ✅ | `mergeMessageLists` (ported); `InlineChatCache.refresh()` merges, never replaces |
| INV-MSGID-001 | ✅ | `apiMsgIdFromPartId` + latest-assistant fallback |
| INV-SHELL-001 | ✅ | empty assistant shell dropped in parse + `applyMessage` |
| INV-OPTIMISTIC-001 | ✅ | optimistic echo + **trimmed, any-user-message** dedup — **found the trailing-space duplicate bug** |
| INV-PERM-SINGLEFLIGHT-001 | 🟡 | AskUserQuestion answered via the parked permission (deny+message); generic tool permissions still deferred to the host |
| INV-PERMMODE-* | N/A | overlay doesn't choose modes |
| INV-INTERACTIVE-001 | 🟡 | AskUserQuestion inline card; ExitPlanMode plan card + inline comments deferred (vs [11](../11-interactive-tools.md)) |
| INV-META-REPLACE-001 | N/A | ignores `meta` (no session list on this surface) |
| INV-REVERT-001 | ✅ | `revertMessageId` filter on render; tap a user bubble → revert; cleared on next send. Works on **both** backends — `onRevert` is gated on session-presence, not backend (matches [OP-005](../05-operations.md)); only the doc-comments said "Claude-only" and are now fixed |
| INV-RECONCILE-001 | ✅ | `onConnected()` → `refresh()` + monotonic merge |
| INV-RECONNECT-SEMANTICS-001 | ✅ | reconnect driven by its own transport loop, not the `reconnected` event |
| INV-ABORT-PERM-001 | ✅ | abort clears the parked question locally; settles on the next `status` |
| INV-ABORT-SETTLE-TOOLS-001 | ✅ | `cancelPendingParts` settles running tools to `canceled` (terminal in `preferStatus`) — **found the "spinner resumes after a refetch" bug** |
| INV-ABORT-DONE-001 | ✅ | `stoppedSinceLastSend` gates the "Done" pill, not the one-shot `aborting` — **found the delayed-"Done" bug** |
| INV-SIDEBAR-META-001 | N/A | no session list / sidebar on this surface |
| INV-PENDING-PERM-GAP-001 | ⛔ | doesn't call `/pending-permission` on (re)attach; the host now serves the parked queue (OP-007) |
| INV-HEARTBEAT-GAP-001 | ✅ | OkHttp `readTimeout(0)` + an explicit capped-backoff reconnect loop (1 s → 15 s) |

## Conformance

Now exercises the core transcript matrix: `CONF-STREAM-DELTA`, `CONF-TOOL-MERGE`,
**`CONF-TOOL-MERGE-CROSSMSG`**, `CONF-MONOTONIC`, `CONF-MSGID-OWNER`, `CONF-SHELL-DROP`,
`CONF-OPTIMISTIC-BACKFILL`, **`CONF-OPTIMISTIC-DEDUP-NORMALIZE`**, `CONF-RECONCILE`,
`CONF-REVERT-FILTER`, `CONF-ABORT-PERM`, `CONF-ABORT-NOISE`, **`CONF-ABORT-SETTLE-TOOLS`**,
**`CONF-ABORT-DONE-SUPPRESS`**, plus `CONF-NO-END` / `CONF-STALE-STREAM` / `CONF-SINGLE-STREAM`
and `CONF-INTERACTIVE-ASK`. N/A on this surface: `CONF-META-REPLACE`, `CONF-SIDEBAR-SYNC`,
`CONF-INTERACTIVE-PLAN`, `CONF-INLINE-COMMENT`, `CONF-PERMMODE-NOCHANGE`.

## Platform gotchas (paid for, in order)

- **Cross-message tool merge** — the `tool_call` (assistant message) and `tool_result`
  (a *following* `role=user` carrier) share one part id but live in **different messages**;
  fold them across messages at the merged-transcript layer, not per-message. Two earlier
  "merge within one message" attempts were wrong; verified against the live gateway. See
  [INV-TOOL-RESULT-CARRIER-001](../08-invariants.md).
- **A client-only `canceled` status must be terminal-ranked** — otherwise the post-abort
  refetch (which still shows the tool `running`) monotonically un-cancels it and the spinner
  resumes. See [INV-ABORT-SETTLE-TOOLS-001](../08-invariants.md).
- **Delayed post-abort idle** — the backend emits a *later* `status → idle` after a stop;
  gate the "Done" affordance on `stoppedSinceLastSend` (cleared on the next send), not the
  one-shot `aborting`. See [INV-ABORT-DONE-001](../08-invariants.md).
- **Optimistic echo** — the gateway preserves trailing whitespace the client trims, so dedup
  the echo by **trimmed** text and against **any** recent user message (the committed copy is
  not the tail once the agent streams). See [INV-OPTIMISTIC-001](../08-invariants.md).
- **Tailing** — `reverseLayout` did **not** pin a *growing* last item; tail explicitly,
  keyed on a content-length signature (not message count), and toggle `follow` only on a real
  user scroll gesture (not the initial settled position, which opened the chat at the top).
  See [STATE-FOLLOW-001](../06-state-model.md).
- OkHttp has no native SSE; the hand-rolled parser only handles the host's single-line
  `event:`/`data:` shape — fine, but not a full SSE parser (no multi-line data, no
  `id:`/`retry:`).
- `readTimeout(0)` is required or the idle SSE socket times out; cancelling the coroutine does
  **not** unblock `readLine()` — only `Call.cancel()` does.
- (Compose-host, not protocol) the floating box's bottom-anchor baseline must be sampled
  whenever the big panels are collapsed; gating it on transient rows (server banner / error /
  Done) meant it never sampled while a preview had an error, so panels grew *downward*.

## Open gaps / deviations

1. ExitPlanMode plan card and inline comments are not yet rendered on this surface
   (AskUserQuestion is). Generic (non-AskUserQuestion) permissions are still handed to the host
   rather than answered inline.
2. No session list, `meta`, or revert/fork-from-history beyond the single revert affordance —
   this is the preview surface, not the full chat. A full-screen native chat would fill the
   N/A rows; it should reuse `ChatTranscript.kt` rather than re-derive the merge.
