# Implementation Checklist — React Native (TypeScript)

> The full mobile chat client (`clank-mobile/src/`). Largely conformant; this file records
> the mapping and the **known gaps** so they stop resurfacing as fresh bugs.

**Platform:** React Native / Expo + react-query + Zustand · **Client kind:** full chat client ·
**Spec version:** 0.6.3 · **Last updated:** 2026-08-12 (clank-mobile#178/#181)

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
| INV-STALE-STREAM-001 | ✅ | `events.ts` generation counter (`gen`) + single `current` source |
| INV-SSE-DOUBLE-001 | ✅ | `events.ts` `connect()` double `closed`/generation guard (fixed the doubled-event bug) |
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
| ~~INV-PERMMODE-EXITPLAN-001~~ | ➖ | retired 0.6.0 — plan review is an ordinary permission prompt |
| INV-PRESET-MATCH-001 | ✅ | `presetEditor.ts presetMatchingConfig` (staged → advertised → persisted `SessionInfo.config`); sheet highlight + chip share it (#172→#178) |
| INV-PRESET-LABEL-001 | ✅ | `presetEditor.ts sessionChipLabel` in `ComposeBar` — preset name, mode-name fallback, persisted raw mode on undecorated rows |
| INV-PRESET-BADGE-001 | ✅ | `presetEditor.ts sessionModeBadge` (#181); no draft flow on this surface yet |
| INV-CONFIG-REFRESH-001 | ✅ | `useSessions.ts refreshSessionAfterConfigSend` — exact-match invalidation awaited before the staged config clears (no stale-label flash) |
| INV-META-REPLACE-001 | 🟡 | uses `patchSessionInCache` per-field for title/status; **verify** `meta` does a full replace, not a field-merge |
| INV-REVERT-001 | ✅ | `dispatch.ts:165` (revert marker + invalidate messages) |
| INV-RECONCILE-001 | ✅ | `resyncAfterStreamGap` (`dispatch.ts`) runs on the backend `reconnected` event, on the client's own transport reconnect (`useEventStream.ts` `onReconnect`), and after every foreground `restart()` |
| INV-RECONNECT-SEMANTICS-001 | ✅ | own-transport recovery is `useEventStream.ts` `onReconnect` → `resyncAfterStreamGap`, independent of the backend `reconnected` event (fixed 2026-07-02; was the frozen-new-session bug) |
| ~~INV-PENDING-PERM-GAP-001~~ (resolved 0.6.3, [OP-007](../05-operations.md)) | ⛔ | the host now serves parked prompts from `/pending-permission`; this client doesn't fetch it on (re)join yet, so a blocked session still renders without its prompt |
| INV-STREAM-SUPERVISE-001 | ✅ | `events.ts` supervised loop: capped 1s→30s full-jitter backoff, never gives up, 401→`forceRefresh`; clean-close delegated to the library re-poll (`CLEAN_CLOSE_REPOLL_MS`); foreground `restart()` in `useEventStream.ts` ([NFR-REL-001/002]). No liveness timer while foregrounded (heartbeat gap remains) |
| INV-INTERACTIVE-001 | ✅ (reference) | `src/lib/{askQuestion,planReview,chatReview}.ts` + `AskQuestionCard.tsx`/`PlanReviewCard.tsx`. Tool-name sniffing (known hack); answer via `SendMessage` |
| INV-SIDEBAR-META-001 | ⛔ | **gap**: no `meta` case in `dispatch.ts`; list patched from `status`/`title` + invalidation; `meta`-only changes (visibility/draft/follow-up) don't push live |
| INV-DEAD-BACKEND-REHYDRATE-001 | ✅ | client is correct — it relies on lazy rehydration (only `/message`+`/abort`, no `/stop`/`/open`). The host wedge that surfaced here as a red **"Needs attention"** (the client's label for status `error`) with every send bouncing, after cancelling a turn almost instantly, was a host bug, fixed in [supaclank/clank#80](https://github.com/supaclank/clank/pull/80). `format.ts` maps `dead`→"Stopped" / `error`→"Needs attention" |

## Conformance

`src/hooks/__tests__/dispatch.test.ts` and `src/lib/__tests__/*` already cover CONF-STREAM-DELTA,
CONF-MONOTONIC, CONF-SHELL-DROP, CONF-TOOL-MERGE, CONF-MSGID-OWNER. Wire the shared fixtures
([10](../10-conformance.md)) into the same harness and fill the gate table. Prioritize the ⛔/🟡
rows above.

## Platform gotchas

- `react-native-sse` uses `'error'` as its **transport** error channel; application errors
  also arrive there carrying `data`. Disambiguate by inspecting `e.data` (`events.ts`).
- `react-native-sse` with `pollingInterval: 0` fires **no callback at all** on a clean server
  close — the stream just silently ends ([INV-STREAM-SUPERVISE-001]). `events.ts` sets a small
  `pollingInterval` precisely so the library's own re-poll covers that path; every `error` path
  closes the socket (cancelling that re-poll) and goes through `scheduleReconnect` instead.
  Don't set it back to 0.
- Async token fetch during connect races teardown → the doubled-socket bug. The
  `closed`/generation re-checks in `events.ts` are load-bearing; don't remove them.
- react-query: `cancelQueries` on the messages key during a `message`/`part` dispatch prevents
  a lagging refetch from clobbering the patch (`dispatch.ts:62`).

## Open gaps / deviations

1. **Pending-permission restore ([OP-007](../05-operations.md))** — fetch
   `/sessions/{id}/pending-permission` in `resyncAfterStreamGap` and replace the local
   pending queue; the host side shipped in 0.6.3.
2. **INV-SIDEBAR-META-001** — consume the `meta` event to push-update list rows (incl.
   visibility/draft/follow-up from other clients) instead of relying on `status`/`title`
   patches + list invalidation.
3. Verify the 🟡 rows against golden behavior and the fixtures.

### ACP migration follow-ups (0.6.x, not yet done)

4. ~~**Modes are hardcoded**~~ — resolved: the create flow is preset-first
   (`GET /presets` + the `config` map) and `src/lib/modes.ts` renders agent-owned
   `available_modes` only; in-session presets follow the
   [Presets in-session](../08-invariants.md) rules (#172→#178, #181).
5. **`revert` is gone** — the endpoint was removed in 0.6.0; the client still calls it and
   the affordance is user-reachable, so it 404s. Remove it.
6. **Questions are retired** — `/questions/.../reply` and the `question` part tag no longer
   exist ([README retire register](../README.md)); the card is unreachable. Delete it.
7. **`/modes` and `/models` are retired (0.6.2)** — creates without the Default-preset
   config keys now fail `400 config_incomplete`; fetch `GET /presets`, send the chosen
   preset's `config`, and use `GET /config-options` only for knob editors
   ([OP-013](../05-operations.md), [OP-016](../05-operations.md)).
8. **Deny messages now become a follow-up user message** ([OP-015](../05-operations.md)) —
   expect the text in the transcript and the session to go busy after a rejection.

Fixed 2026-07-02 (was #1/#2 here): own-transport reconnect + supervised backoff loop +
foreground resubscribe — `events.ts` rewrite ([EVT-006], [INV-STREAM-SUPERVISE-001]). The
shipped symptom was a brand-new session frozen at "Working…" with a stale session list until
pull-to-refresh.
