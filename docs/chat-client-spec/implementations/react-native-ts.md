# Implementation Checklist — React Native (TypeScript)

> The full mobile chat client (`clank-mobile/src/`). Largely conformant; this file records
> the mapping and the **known gaps** so they stop resurfacing as fresh bugs.

**Platform:** React Native / Expo + react-query + Zustand · **Client kind:** full chat client ·
**Spec version:** 0.2.0 · **Last updated:** 2026-06-21

## Where the pieces live

| Concern | Component |
|---|---|
| HTTP client / bearer | `src/api/client.ts` (Bearer inject, 401 retry-once) |
| SSE stream | `src/api/events.ts` (`react-native-sse`) |
| Reducer | `src/hooks/dispatch.ts` (`dispatchEvent`) — pure, unit-tested |
| Transcript merge / reconcile | `src/lib/mergeMessages.ts` (`mergeMessageLists`), `src/hooks/useSessions.ts` |
| Stream mount | `src/hooks/useEventStream.ts` |
| Permission store + UI | `src/store/permissions.ts`, compose/prompt components |
| Token storage / refresh | `src/auth/session.ts`, `src/auth/oauth.ts`, `src/auth/discovery.ts` |

## Invariants

| Rule | Status | How / gap |
|---|---|---|
| INV-CREATE-RACE-001 | ✅ | stream mounted app-wide before create |
| INV-STALE-STREAM-001 | ✅ | `events.ts` `closed` flag; single `current` source |
| INV-SSE-DOUBLE-001 | ✅ | `events.ts:71`–`:98` double `if (closed)` guard (fixed the doubled-event bug) |
| INV-NO-END-001 | ✅ | no `end` listener; relies on transport error/close |
| INV-DELTA-001 | ✅ | `dispatch.ts:134` (append on `is_delta`) |
| INV-TOOL-MERGE-001 | ✅ | `mergeMessages.ts:46` (`mergePart`) |
| INV-MONOTONIC-001 | ✅ | `mergeMessages.ts` (`mergeMessageLists`); `useSessions.ts:62` |
| INV-MSGID-001 | ✅ | `dispatch.ts:104` (`message_id \|\| apiMsgIdFromPartId`) |
| INV-SHELL-001 | ✅ | `dispatch.ts:51` (shell drop) |
| INV-OPTIMISTIC-001 | ✅ | `dispatch.ts:73` (id-less echo dedup) |
| INV-PERM-SINGLEFLIGHT-001 | ✅ | `store/permissions.ts` queue; reply guarded in UI |
| INV-DENY-SETTLE-001 | 🟡 | **verify** running-tool settle on deny matches golden `markRunningToolsFailed` |
| INV-ABORT-PERM-001 | 🟡 | **verify** composer re-enables + queue clears after abort |
| INV-PERMMODE-001 | ✅ | `sessions.ts` send omits mode unless changed |
| INV-PERMMODE-EXITPLAN-001 | 🟡 | plan-review UI exists; **verify** mode stays `""` after approval |
| INV-META-REPLACE-001 | 🟡 | uses `patchSessionInCache` per-field for title/status; **verify** `meta` does a full replace, not a field-merge |
| INV-REVERT-001 | ✅ | `dispatch.ts:165` (revert marker + invalidate messages) |
| INV-RECONCILE-001 | 🟡 | reconciles on `reconnected` event (`dispatch.ts:196`) **but** see next row |
| INV-RECONNECT-SEMANTICS-001 | ⛔ | **gap**: resync is keyed off the backend `reconnected` event; the client's *own* SSE transport reconnect (`events.ts` one-shot refresh) does **not** trigger a messages refetch → a transport blip can leave a stale transcript. Fix: on own reconnect, invalidate messages + list. |
| INV-PENDING-PERM-GAP-001 | ⛔ | blocked-on-permission session after (re)join is not surfaced honestly (host gap + client) |
| INV-HEARTBEAT-GAP-001 | 🟡 | `pollingInterval:0`, single forced-refresh reconnect then `onError` — no capped-backoff retry, no liveness timer ([NFR-REL-001/002]) |
| INV-INTERACTIVE-001 | ✅ (reference) | `src/lib/{askQuestion,planReview,chatReview}.ts` + `AskQuestionCard.tsx`/`PlanReviewCard.tsx`. Tool-name sniffing (known hack); answer via `SendMessage` |
| INV-SIDEBAR-META-001 | ⛔ | **gap**: no `meta` case in `dispatch.ts`; list patched from `status`/`title` + invalidation; `meta`-only changes (visibility/draft/follow-up) don't push live |

## Conformance

`src/hooks/__tests__/dispatch.test.ts` and `src/lib/__tests__/*` already cover CONF-STREAM-DELTA,
CONF-MONOTONIC, CONF-SHELL-DROP, CONF-TOOL-MERGE, CONF-MSGID-OWNER. Wire the shared fixtures
([10](../10-conformance.md)) into the same harness and fill the gate table. Prioritize the ⛔/🟡
rows above.

## Platform gotchas

- `react-native-sse` uses `'error'` as its **transport** error channel; application errors
  also arrive there carrying `data`. Disambiguate by inspecting `e.data` (`events.ts:106`).
- Async token fetch during connect races teardown → the doubled-socket bug. The `closed`
  re-checks in `events.ts` are load-bearing; don't remove them.
- react-query: `cancelQueries` on the messages key during a `message`/`part` dispatch prevents
  a lagging refetch from clobbering the patch (`dispatch.ts:62`).

## Open gaps / deviations

1. **INV-RECONNECT-SEMANTICS-001 / INV-RECONCILE-001** — own-transport reconnect must trigger
   a messages+list refetch (not only the backend `reconnected` event). Highest-priority fix.
2. **NFR-REL-001** — replace one-shot reconnect with capped-backoff auto-reconnect.
3. **INV-PENDING-PERM-GAP-001** — surface a long blocked-with-no-activity session honestly.
4. **INV-SIDEBAR-META-001** — consume the `meta` event to push-update list rows (incl.
   visibility/draft/follow-up from other clients) instead of relying on `status`/`title`
   patches + list invalidation.
5. Verify the 🟡 rows against golden behavior and the fixtures.
