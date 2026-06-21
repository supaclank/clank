# Implementation Checklist — TUI (Go) · golden reference

> This is the **golden reference for the core event/state machine**: most of the spec was
> derived from it. It proves [CONF-GATE-001](../10-conformance.md) — every `MUST` maps to real
> behavior in *some* reference client — and is the canonical "how" for the parts the TUI leads.
> A ❌ here means the TUI is **not** the reference for that rule (e.g. interactive tools, where
> the RN client leads, [11](../11-interactive-tools.md)); such gaps are expected and noted, not
> necessarily bugs.

**Platform:** Go / Bubble Tea TUI · **Client kind:** full chat client ·
**Spec version:** 0.2.0 · **Last updated:** 2026-06-21

## Where the pieces live

| Concern | Component |
|---|---|
| HTTP client / bearer | `internal/daemonclient/transport.go` (`do`, `:46`) |
| SSE stream | `internal/daemonclient/transport.go` (`openSSE` `:122`, `parseSSEStream` `:157`); `internal/daemonclient/sessions.go` (`Subscribe`) |
| Reducer | `internal/tui/sessionview.go` (`Update`, `handleEvent` `:1370`) |
| Transcript merge | `internal/tui/sessionview.go` (`handleSessionMessages` `:1521`, `upsertPartEntry` `:1638`) |
| Permission UI + reply | `internal/tui/sessionview.go` (`:1099` gate, `replyPermission` `:2168`) |
| Compose / create | `internal/tui/sessionview_compose.go` (`createSessionCmd` `:284`) |

## Invariants

| Rule | Status | How (golden source) |
|---|---|---|
| INV-CREATE-RACE-001 | ✅ | `sessionview_compose.go:284` — Subscribe then Create |
| INV-STALE-STREAM-001 | ✅ | `sessionview.go:828` (closed-signal identity), `:857` (re-arm only on live) |
| INV-SSE-DOUBLE-001 | ✅ | one `eventsCh`; cancel on replace (`cancelEvents`) |
| INV-NO-END-001 | ✅ | `parseSSEStream` returns on EOF; no `end` handling (`transport.go:189`) |
| INV-DELTA-001 | ✅ | `upsertPartEntry` `:1665` (append on delta, replace on snapshot) |
| INV-TOOL-MERGE-001 | ✅ | `upsertPartEntry` `:1649` (merge Input/Output/Tool) |
| INV-MONOTONIC-001 | ✅ | idempotent-by-partID upsert; history re-applied through same path `:1515` |
| INV-MSGID-001 | ✅ | parts carry message id from history; SSE parts upsert by part id |
| INV-SHELL-001 | ✅ | `handleMessage` `:1489` (`historyLoaded` skip), `:1492` (skip empty) |
| INV-OPTIMISTIC-001 | ✅ | echo `:1165`, backfill `:1470` |
| INV-PERM-SINGLEFLIGHT-001 | ✅ | `:1099` (`replyingPermID` + `!inputActive`), `:1112` (lock other keys) |
| INV-DENY-SETTLE-001 | ✅ | `:899` + `markRunningToolsFailed` `:1608` |
| INV-ABORT-PERM-001 | 🟡 gap | host `failPendingPermissions` denies server-side, but the TUI does **not** clear `pendingPerms`/refetch on abort (`:952` handles only the error case; `handleStatusChange` settle doesn't touch the queue) → composer can stay wedged. See [INV-ABORT-PERM-001](../08-invariants.md). |
| INV-PERMMODE-001 | ✅ | `sendMessage` `:2162` sends only the selected mode |
| INV-PERMMODE-EXITPLAN-001 | ✅ | backend `claude_permissions.go:97`; TUI sends `""` on follow-ups |
| INV-META-REPLACE-001 | ✅ | consumes `meta` as full `SessionInfo` (inbox view) |
| INV-REVERT-001 | ✅ | `handleSessionMessages` `:1535` (filter), `:1162` (clear on send) |
| INV-RECONCILE-001 | ✅ | `Init` `:511` fetches messages+pending+info; refetch on reopen |
| INV-RECONNECT-SEMANTICS-001 | ✅ | does not key reconcile off `reconnected`; reconnect handled by re-subscribe |
| INV-PENDING-PERM-GAP-001 | 🟡 | calls `fetchPendingPermission` `:553` (receives `[]`); blocked-state not specially surfaced — same host gap as all clients |
| INV-HEARTBEAT-GAP-001 | 🟡 | SSE client has no read timeout (`transport.go:122`); relies on re-subscribe; no explicit liveness timer |
| INV-INTERACTIVE-001 | ❌ gap | handles the permission but renders no AskUserQuestion/ExitPlanMode UI and no inline comments — RN is the reference ([11](../11-interactive-tools.md)) |
| INV-SIDEBAR-META-001 | ✅ | `inbox_sse.go:149` — list driven by `meta`; field-level `status`/`title` deliberately ignored for the list (`:23`) |

## Conformance

All `MUST`-guarding scenarios are exercised by the golden tests; lift fixtures from them
([10 §fixture sources](../10-conformance.md)):
`internal/tui/session_sse_stale_test.go` (CONF-STALE-STREAM, CONF-SINGLE-STREAM),
`internal/tui/sessionview_test.go` (CONF-PERM-LOCK, CONF-OPTIMISTIC-BACKFILL, CONF-REVERT-FILTER,
CONF-ABORT-*, CONF-PENDING-PERM-GAP restore path), `internal/agent/claude_*_test.go`
(CONF-PLAN-EXIT, revert). A formal `applyInput`/`project` harness is **not yet** extracted —
follow-up ([NFR-TEST-001](../09-non-functional.md)).

## Notes / gotchas

- Two SSE consumption layers: per-view `eventsCh` + the `sourceCh`/`eventsCh` identity dance
  is the canonical answer to [INV-STALE-STREAM-001]; study `session_sse_stale_test.go` before
  touching it.
- The TUI does not implement the capped-backoff auto-reconnect of [NFR-REL-001] inside the
  chat view; reconnection is a re-subscribe at the inbox layer. New full clients SHOULD
  implement explicit capped backoff.
