# Implementation Checklist — Web preview overlay

**Platform:** Browser (vanilla JS, `internal/webpreview/overlay/overlay.js`) ·
**Client kind:** partial consumer — a lightweight chat box injected into the user's own app
by the preview proxy; deliberately not a full chat client (no session list, no history view,
30-message rolling window) · **Spec version targeted:** 0.2.0 · **Last updated:** 2026-07-18

> Not the greenfield web client ([web.md](web.md)) — that remains unbuilt. This documents what
> the overlay *does* consume so gaps are explicit rather than discovered in the field.

**Status legend:** ✅ done · 🟡 partial · ⛔ not started · N/A (justify)

## Where the pieces live

| Concern | This platform's component |
|---|---|
| HTTP client / bearer injection | `api()` helper; token from `window.__CLANK_PREVIEW` config, `Authorization: Bearer` on every call |
| SSE stream | hand-rolled `fetch()` + `ReadableStream` frame parser (`subscribe`/`handleFrame`) — native `EventSource` can't set the header |
| Reducer (apply event → state) | inline `handleFrame` switch mutating `store` |
| Transcript merge / reconcile | ⛔ none — live stream only, no `Messages()` refetch |
| Permission UI + reply | single-slot `store.permission` card, allow/deny → `POST …/permissions/{id}/reply` |
| Token storage / refresh | injected per-page-load by the proxy; no refresh (preview token, `pkg/preview/tokens`) |
| Conformance harness | ⛔ none |

## Invariants ([08](../08-invariants.md))

Only rules the overlay's surface touches are listed; the rest are N/A for a partial consumer
(no sidebar, no optimistic backfill from refetch, no permission-mode UI).

| Rule | Status | How / gap |
|---|---|---|
| INV-CREATE-RACE-001 (subscribe before create) | 🟡 | subscribes **after** create returns; mitigated by the session-snapshot status sync, but early parts/permissions of turn 1 can be lost |
| INV-SSE-DOUBLE-001 (exactly one stream) | ✅ | `sseAbort` aborts the old stream before opening a new one |
| INV-NO-END-001 (no `end` frame) | ✅ | ends on socket close; capped-backoff resubscribe |
| INV-DELTA-001 (is_delta append/replace) | ✅ | text parts only |
| INV-TOOL-MERGE-001 (merge input+output) | ⛔ | tool_call parts only flip the border to "working"; never rendered |
| INV-MONOTONIC-001 (refetch never shrinks) | N/A | no refetch; 30-message window is a deliberate cap |
| INV-PERM-SINGLEFLIGHT-001 (lock + single reply) | 🟡 | card clears before the POST, but a second `permission` event **overwrites** an unanswered first — the older prompt is silently dropped |
| INV-DENY-SETTLE-001 (settle tools on deny) | N/A | tools not rendered |
| INV-ABORT-PERM-001 (abort clears perms) | ⛔ | abort leaves `store.permission` up |
| INV-PERMMODE-001 (`""` = no change) | ✅ | never sends `permission_mode` |
| INV-PERMMODE-EXITPLAN-001 (ExitPlanMode) | ⛔ | rendered as a generic allow/deny permission; no plan text, no revise-with-notes |
| INV-REVERT-001 (revert filter) | 🟡 | revert issued; `revert` event only toasts, transcript not filtered |
| INV-RECONCILE-001 (reconcile on reconnect) | ⛔ | only the coarse agent status is re-synced from the session snapshot; messages/parts emitted while disconnected are lost |
| INV-INTERACTIVE-001 (render interactive tools) | ⛔ | **the headline gap** — see below |
| INV-PENDING-PERM-GAP-001 (honest blocked-state) | 🟡 | page reload keeps `sessionId` (sessionStorage) but loses any pending prompt until the next event |

## Interactive tools ([11](../11-interactive-tools.md)) — the known gaps

| Rule | Status | Gap |
|---|---|---|
| QST-001 (render `part.question` tag, structured reply) | ⛔ | question arrives as a bare `permission` event: "Allow AskUserQuestion?" allow/deny. No option cards, no `POST …/questions/{request_id}/reply` (the endpoint is already reachable — `/__clank/api/` is a full daemon passthrough). TUI + RN solved this; port the tag path. |
| QST-002 (positional answerability) | ⛔ | follows QST-001 |
| QST-003 (suppress the paired permission) | ⛔ | the permission is the *only* thing shown today; once the tag renders, matching `request_id` must suppress it |
| ITOOL-00x (legacy part-sniffing) | N/A | prefer the tag path; overlay has no pre-tag installed base |
| Plan review (ExitPlanMode) | ⛔ | generic permission card; no plan markdown, no approve/revise |
| ICOMMENT-001 (inline comments) | N/A | overlay has no rendered assistant-markdown selection surface (SHOULD; out of scope for the box) |

## Supporting layers (quick check)

- Connection & auth ([02](../02-connection-and-auth.md)): N/A PKCE/refresh — proxy-injected
  preview token; bearer on all calls incl. SSE ✅. `?t=` query token on the voice WS only.
- Data model ([03](../03-data-model.md)): tolerant — unknown event types ignored ✅.
- Operations ([05](../05-operations.md)): single-flight send (`store.sending`) ✅; create
  carries the first prompt ✅; no messages-refetch reconcile ⛔.
- Projections ([06](../06-state-model.md)): streaming vs settled ✅; cancelling ✅
  (send button doubles as stop); error border ✅; composer lock ✅.

## Platform gotchas

- The overlay runs inside the *user's* app page: everything lives in a closed shadow-root-ish
  host and must not leak globals or styles; heavy chat UI is a non-goal.
- `EventSource` can't set `Authorization` — keep the hand-rolled fetch-stream parser.
- Events are at-most-once with no replay (see comment at `subscribe`): anything between
  session create and stream open is gone. Any fix for INV-RECONCILE-001 must refetch
  messages, not trust the stream.

## Open gaps / deviations (priority order)

1. **Question tag (QST-001..003)** — render option cards from `part.question`, reply via the
   questions endpoint, suppress the paired permission. Prereq: handle `part` events for
   `tool_call` parts enough to see the tag (today only `text` parts are read).
2. **Reconnect/reload reconcile (INV-RECONCILE-001)** — one `GET /sessions/{id}/messages` on
   (re)subscribe would fix lost turn-1 output, reload amnesia, and the create-race in one move.
3. **Permission queue (INV-PERM-SINGLEFLIGHT-001)** — don't overwrite an unanswered prompt.
4. **Plan review** — show `input.plan` markdown on ExitPlanMode permissions; approve/revise.
5. **Abort should clear the pending permission card (INV-ABORT-PERM-001).**
