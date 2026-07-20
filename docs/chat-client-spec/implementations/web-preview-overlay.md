# Implementation Checklist — Web preview overlay

**Platform:** Browser (vanilla JS, `internal/webpreview/overlay/`) ·
**Client kind:** partial consumer — a lightweight chat box injected into the user's own app
by the preview proxy; deliberately not a full chat client (no session list, no history view,
rolling transcript window) · **Spec version targeted:** 0.2.0 · **Last updated:** 2026-07-18

> Not the greenfield web client ([web.md](web.md)) — that remains unbuilt. This documents what
> the overlay consumes so gaps are explicit rather than discovered in the field.

**Status legend:** ✅ done · 🟡 partial · ⛔ not started · N/A (justify)

## Where the pieces live

| Concern | This platform's component |
|---|---|
| HTTP client / bearer injection | `api()` helper; token from `window.__CLANK_PREVIEW` config, `Authorization: Bearer` on every call |
| SSE stream | hand-rolled `fetch()` + `ReadableStream` frame parser (`subscribe`/`handleFrame`) — native `EventSource` can't set the header |
| Reducer (apply event → state) | `handleFrame` switch in `overlay.js`; protocol decisions in `chat.js` (pure, DOM-free) |
| Transcript merge / reconcile | `reconcile()` — messages refetch on every stream open, projected via `chat.js chatFromMessages` |
| Permission UI + reply | FIFO `store.perms` queue, head renders; plan prompts get the review card; allow/deny(+message) → permissions reply endpoint |
| Question UI + reply | `store.question` + `renderQuestion()`; structured reply → questions reply endpoint |
| Token storage / refresh | injected per-page-load by the proxy; no refresh (preview token, `pkg/preview/tokens`) |
| Conformance harness | `chat_test.mjs` (`node --test`, wired into `go test` via `TestOverlayChatJS`) — covers the chat.js protocol logic, not the DOM |

## Invariants ([08](../08-invariants.md))

Only rules the overlay's surface touches are listed; the rest are N/A for a partial consumer
(no sidebar, no optimistic backfill from refetch, no permission-mode UI).

| Rule | Status | How / gap |
|---|---|---|
| INV-CREATE-RACE-001 (subscribe before create) | 🟡 | still subscribes **after** create returns (create carries the first prompt, so there is no id to subscribe with earlier); the reconcile refetch at stream open recovers settled content from the gap |
| INV-SSE-DOUBLE-001 (exactly one stream) | ✅ | `sseAbort` aborts the old stream before opening a new one |
| INV-NO-END-001 (no `end` frame) | ✅ | ends on socket close; capped-backoff resubscribe |
| INV-DELTA-001 (is_delta append/replace) | ✅ | text parts only |
| INV-TOOL-MERGE-001 (merge input+output) | 🟡 | tool calls flip the border to "working"; only question tags and ExitPlanMode plans get UI — generic tool cards are a non-goal for the box |
| INV-MONOTONIC-001 (refetch never shrinks) | N/A | rolling window is a deliberate cap; refetch replaces wholesale |
| INV-PERM-SINGLEFLIGHT-001 (lock + single reply) | ✅ | FIFO queue (`chat.js pushPermission`, dedup by request_id); reply pops the head, next prompt renders |
| INV-DENY-SETTLE-001 (settle tools on deny) | N/A | tools not rendered |
| INV-ABORT-PERM-001 (abort clears perms) | ✅ | abort clears the queue and the question card |
| INV-PERMMODE-001 (`""` = no change) | ✅ | never sends `permission_mode` |
| INV-PERMMODE-EXITPLAN-001 (ExitPlanMode) | ✅ | plan review card: Approve = allow; Request changes = deny with notes as the reason |
| INV-REVERT-001 (revert filter) | 🟡 | revert issued; `revert` event only toasts — the view catches up at the next reconcile (reconnect/reload) |
| INV-RECONCILE-001 (reconcile on reconnect) | ✅ | `reconcile()` refetches messages on every stream open (reload, reconnect, first subscribe); point-in-time — it never clears a live question |
| INV-INTERACTIVE-001 (render interactive tools) | ✅ | question card + plan review (below) |
| INV-PENDING-PERM-GAP-001 (honest blocked-state) | 🟡 | a pending **question** survives reload (tag rides the transcript); a bare permission does not — the host has no snapshot yet (`handlePendingPermissions` TODO) |

## Interactive tools ([11](../11-interactive-tools.md))

| Rule | Status | How |
|---|---|---|
| QST-001 (render `part.question` tag, structured reply) | ✅ | tag read from live part events, settled messages, and the reconcile refetch; card renders options (multi-select, descriptions), free-text "Other" when `allow_custom`, Dismiss = `{reject:true}`; reply posts `{answers:[{selected,custom}]}` in question order — all-empty = delegation |
| QST-002 (positional answerability) | ✅ | `chat.js activeQuestionFromParts` (paired tool_result/thinking/blank text don't retire; error status does); live events clear the card when a later text/tool part or user message arrives |
| QST-003 (suppress the paired permission) | ✅ | matched by `request_id` or `tool_use_id` at queue-ingress and when the tag activates; the question reply drops any queued copy |
| ITOOL-00x (legacy part-sniffing) | N/A | tag path only; the overlay has no pre-tag installed base |
| Plan review (ExitPlanMode) | ✅ | plan text located from the gated tool part (`chat.js planTextFor`: tool_use_id match, newest fallback); scrollable plan block + revision-notes field |
| ICOMMENT-001 (inline comments) | N/A | no rendered assistant-markdown selection surface (SHOULD; out of scope for the box) |

## Supporting layers (quick check)

- Connection & auth ([02](../02-connection-and-auth.md)): N/A PKCE/refresh — proxy-injected
  preview token; bearer on all calls incl. SSE ✅. `?t=` query token on the voice WS only.
- Data model ([03](../03-data-model.md)): tolerant — unknown event types ignored ✅;
  `allow_custom` tri-state honored ✅.
- Operations ([05](../05-operations.md)): single-flight send (`store.sending`) and question
  reply (`sending` flag) ✅; create carries the first prompt ✅; messages refetch as the
  reconcile source ✅.
- Projections ([06](../06-state-model.md)): streaming vs settled ✅; cancelling ✅
  (send button doubles as stop); error border ✅; composer lock ⛔ (the textarea
  stays editable while sending/agent-busy — only the send button state changes).

## Platform gotchas

- The overlay runs inside the *user's* app page: everything lives in a shadow root and must
  not leak globals or styles; heavy chat UI is a non-goal.
- `EventSource` can't set `Authorization` — keep the hand-rolled fetch-stream parser.
- Events are at-most-once with no replay (see comment at `subscribe`): the reconcile refetch,
  not the stream, is the recovery path. The refetch is point-in-time and may race a fresher
  live event, so it only ever *adds* a question, never clears one.
- `chat.js` is a separate ES module (overlay.js imports it) so `node --test` can execute the
  protocol logic without a DOM; the proxy serves it at `/__clank/chat.js`.
- Rebuilding the question card on render would steal focus from the "Other" input — the
  rebuild saves and restores focus + caret.

## Open gaps / deviations

1. **Generic tool-call cards** — tool activity still renders as a border state only
   (INV-TOOL-MERGE-001 🟡). Deliberate for now; revisit if users ask what the agent is doing.
2. **Pending bare permission lost on reload** (INV-PENDING-PERM-GAP-001) — needs the host-side
   permission snapshot (`internal/host/mux` `handlePendingPermissions` TODO), not overlay work.
3. **Revert view lag** (INV-REVERT-001 🟡) — reverted messages disappear at the next
   reconcile, not on the `revert` event.
