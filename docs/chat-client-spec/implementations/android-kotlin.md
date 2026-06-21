# Implementation Checklist — Android (Kotlin)

> **Important scope note.** Today's Kotlin client is **not** a full chat client. It is the
> native **preview-overlay FAB** (`clank-mobile/modules/preview-launcher/android/.../session/`)
> that consumes a *subset* of the stream to drive a floating "agent working / last action /
> answer-this-question" widget. The full mobile chat experience is the
> [React Native client](react-native-ts.md). This file tracks the Kotlin consumer's
> conformance **for the slice it implements**, and flags where it diverged by guessing at the
> protocol — exactly the drift this spec exists to stop. A future full native Kotlin client
> would fill the N/A rows.

**Platform:** Kotlin / OkHttp / Coroutines (Expo native module) ·
**Client kind:** **partial consumer** (preview FAB) · **Spec version:** 0.1.0 ·
**Last updated:** 2026-06-21

## Where the pieces live

| Concern | Component |
|---|---|
| SSE stream | `…/session/SessionEventStream.kt` (OkHttp, hand-rolled frame parser) |
| HTTP (messages, etc.) | `…/session/SessionClient.kt` |
| Question parsing | `…/session/AskQuestion.kt`, `parseAskQuestions` |
| Health probe | `…/session/PreviewServerHealth.kt` |

## Invariants (for the implemented slice)

| Rule | Status | How / divergence |
|---|---|---|
| INV-CREATE-RACE-001 | N/A | overlay attaches to an existing session, never creates |
| INV-STALE-STREAM-001 | ✅ | `activeCall` tracked; `stop()` cancels the socket (`:114`) |
| INV-SSE-DOUBLE-001 | ✅ | `start()` cancels the prior job before relaunch (`:108`) |
| INV-NO-END-001 | ✅ | detects close via `readLine() ?: break` (`:185`); also handles `event: end` (`:264`) — **correct**, because this client subscribes to the per-session `/sessions/{id}/events`, which emits `end` on host shutdown ([INV-NO-END-001](../08-invariants.md)). Not a phantom branch. |
| INV-DELTA-001 | 🟡 | forwards `is_delta` to the listener (`:228`) but the FAB only shows a chip, so it doesn't accumulate text. A full client MUST append. |
| INV-TOOL-MERGE-001 | N/A | no transcript; renders a one-line chip per part (`summarizePart` `:340`) |
| INV-MONOTONIC-001 | N/A | holds no transcript to reconcile (chip + last-message only) |
| INV-MSGID-001 | 🟡 | reads `message_id` but doesn't resolve the id-less-part owner (no transcript) |
| INV-SHELL-001 | ✅ | `onMessage` only fires when text is non-blank (`:250`) |
| INV-OPTIMISTIC-001 | N/A | overlay doesn't send user messages |
| INV-PERM-SINGLEFLIGHT-001 | 🟡 | surfaces permission (`:254`); only *acts on* AskUserQuestion; other tools handed to the container. Verify single-flight in the answer path. |
| INV-PERMMODE-* | N/A | overlay doesn't choose modes |
| INV-INTERACTIVE-001 | 🟡 | renders AskUserQuestion inline (`:230`, `AskQuestion.kt`); no ExitPlanMode plan card, no inline comments — partial vs [11](../11-interactive-tools.md) |
| INV-META-REPLACE-001 | N/A | ignores `meta` (`:267` — subset consumer) |
| INV-REVERT-001 | N/A | ignores `revert` |
| INV-RECONCILE-001 | 🟡 | reconnects (below) but does not refetch a transcript (it has none); a full client MUST reconcile |
| INV-RECONNECT-SEMANTICS-001 | ✅ | reconnect is driven by its own transport loop, not the `reconnected` event (which it ignores) |
| INV-SIDEBAR-META-001 | N/A | subset consumer; no session list or sidebar |
| INV-PENDING-PERM-GAP-001 | ⛔ | on (re)attach to a blocked session it won't learn of the parked permission (host gap) |
| INV-HEARTBEAT-GAP-001 | ✅ | OkHttp `readTimeout(0)` for the long-lived socket **plus** an explicit capped-backoff reconnect loop (`:121`, 1s→15s) — the [NFR-REL-001] reference behavior |

## Conformance

Applicable subset: CONF-NO-END (the `end` branch is correct for the per-session stream), CONF-STALE-STREAM, CONF-SINGLE-STREAM,
CONF-PLAN-EXIT / CONF-INTERACTIVE-ASK (AskUserQuestion parsing + terminal-status clear, `:230`). The rest
are N/A until a full native chat client exists.

## Platform gotchas

- OkHttp has no native SSE; the hand-rolled parser only handles the host's single-line
  `event:`/`data:` shape (`:174`) — fine, but it is *not* a full SSE parser (no multi-line
  data, no `id:`/`retry:`).
- `readTimeout(0)` is required or the idle SSE socket times out; cancelling the coroutine does
  **not** unblock `readLine()` — only `Call.cancel()` does (`:100`).
- AskUserQuestion: a terminal part `status` means already-answered → clear the card, don't
  re-show (`:78` doc, `:230`).

## Open gaps / deviations

1. ~~Remove the phantom `"end"` event branch~~ — **resolved**: the `"end"` branch is correct;
   the per-session `/sessions/{id}/events` stream emits `event: end` on host shutdown
   ([INV-NO-END-001](../08-invariants.md)). The branch should stay.
2. If this grows into a full chat client, implement the N/A rows (transcript, monotonic merge,
   reconcile-on-reconnect, optimistic send) rather than re-deriving them.
