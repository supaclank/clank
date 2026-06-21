# 05 · Operations

The HTTP operations a chat client uses, reached through the gateway ([01](01-architecture.md)).
Source of truth for routes: `internal/host/mux/mux.go:72`. All require the bearer
([CONN-010](02-connection-and-auth.md)). Error envelope and status mapping: [CONN-020/021](02-connection-and-auth.md).

## Catalog

| Method | Path | Request | Success | Idempotent | Notes |
|---|---|---|---|---|---|
| POST | `/sessions` | `StartRequest` | `201` `SessionInfo` | **No** | Create + dispatch first prompt. Slow on cold host. |
| GET | `/sessions` | — | `200` `SessionInfo[]` | Yes | Inbox list, newest-updated first. |
| GET | `/sessions/search` | query | `200` `SessionInfo[]` | Yes | `q`, `visibility`, `since`, `until`, `limit`. |
| GET | `/sessions/{id}` | — | `200` `SessionInfo` | Yes | Single session. |
| DELETE | `/sessions/{id}` | — | `204` | Yes | Stops backend + removes metadata. |
| POST | `/sessions/{id}/message` | `SendMessageOpts` | `204` | **No** | Follow-up; fire-and-forget. Alias: `/send`. |
| POST | `/sessions/{id}/abort` | — | `204` | Yes | Interrupt the current turn. |
| POST | `/sessions/{id}/revert` | `{ message_id }` | `204` | **No** | Claude only; OpenCode errors. |
| POST | `/sessions/{id}/fork` | `{ message_id? }` | `200` `SessionInfo` | **No** | OpenCode only; Claude errors. |
| GET | `/sessions/{id}/messages` | — | `200` `MessageData[]` | Yes | The reconciliation source. |
| GET | `/sessions/{id}/pending-permission` | — | `200` `[]` | Yes | **Currently always empty** — see [OP-007]. |
| POST | `/sessions/{id}/permissions/{permID}/reply` | `{ allow, message? }` | `204` | Yes* | Answer a permission prompt. |
| POST | `/sessions/{id}/read` | — | `204` | Yes | Mark read; does not bump `updated_at`. |
| POST | `/sessions/{id}/visibility` | `{ visibility }` | `204` | Yes | `done`/`archived`/`""`. |
| POST | `/sessions/{id}/draft` | `{ draft }` | `204` | Yes | Save composer draft. |
| POST | `/sessions/{id}/followup` | — | `200` `SessionInfo` | Yes | Toggle follow-up flag. |
| GET | `/events`, `/sessions/{id}/events` | — | `200` SSE | — | See [04](04-event-protocol.md). |
| GET | `/backends`, `/agents`, `/models` | query | `200` …`Info[]` | Yes | Capability lookups. |

`*` reply is idempotent at the host (an unknown/duplicate permID errors fast); clients
still single-flight it in the UI ([INV-PERM-SINGLEFLIGHT-001](08-invariants.md)).

**Intentionally omitted.** The host exposes session-lifecycle/host-management routes a chat
client driving Create + `/message` + SSE does not need: `POST /sessions/{id}/open` and
`POST /sessions/{id}/open-and-send` (backend rehydration/orchestration — `POST /sessions` already opens-and-sends
internally, and the SSE/messages paths lazily rehydrate), and `POST /sessions/{id}/stop`
(backend **process teardown** — distinct from `/abort`, which only interrupts the current
turn). Worktree / project / sync / auth / GitHub routes are likewise out of chat scope.

## The operations that need care

### Create — `POST /sessions`

- **[OP-001] (MUST)** A client MUST treat create as **non-idempotent** and single-flight it
  (disable the submit affordance until it resolves), with a generous timeout for cold-host
  wake. It MUST NOT auto-retry on a slow response. **Why:** a duplicate create makes a
  duplicate session. Cross-ref [CONN-030/031](02-connection-and-auth.md). **Golden:**
  `internal/tui/sessionview_compose.go:235`, `:296`. Note the **create race**: subscribe to
  events *before* creating — see [INV-CREATE-RACE-001](08-invariants.md).

### Send — `POST /sessions/{id}/message`

- **[OP-002] (MUST)** Send is **fire-and-forget**: a `204` means *the prompt was dispatched*,
  not that the agent finished. A client MUST observe completion via the `status` event
  stream (→ `idle`), never by the send response. It MUST single-flight sends (one in flight
  at a time). **Why:** treating `204` as "done" or allowing concurrent sends desynchronizes
  the turn. **Golden:** `internal/agent/agent.go:597` (Send "returns once dispatched, NOT
  when the LLM finishes"), `internal/tui/sessionview.go:1151` (`submitting` guard), `:2163`.

### Reply to permission — `POST /sessions/{id}/permissions/{permID}/reply`

- **[OP-003] (MUST)** Body is `{ "allow": bool, "message"?: string }`. `message` is the
  reason forwarded to the model when `allow=false` (e.g. plan-review comments) and is
  **ignored when `allow=true`**. A client MUST single-flight the reply per request ID. **Why:**
  double-replying races the backend; the deny-reason is the model's only feedback channel on
  a rejection. **Golden:** `internal/host/mux/sessions.go:252`, `internal/agent/claude_permissions.go:134`
  (`RespondPermission`), `internal/tui/sessionview.go:2168`.

### Abort — `POST /sessions/{id}/abort`

- **[OP-004] (MUST)** Abort interrupts the current turn; it is best-effort and idempotent.
  The `204` means *the interrupt was delivered*, not that the agent stopped — the client
  observes the actual stop via `status` → `idle`/`error`. Aborting also **denies all parked
  permission prompts** server-side. **Why:** the abort UX must be driven by the status event,
  not the call return; pending permissions must be cleared after abort. **Golden:**
  `internal/agent/claude_permissions.go:152` (`failPendingPermissions`),
  `internal/tui/sessionview.go:2190`, `:2203` (`startAbort`).

### Revert / fork — backend-specific

- **[OP-005] (MUST)** `revert` is Claude-only (file rollback + transcript truncation);
  `fork` is OpenCode-only. Calling the unsupported one returns an error. A client MUST handle
  that error gracefully (e.g. hide the action for the wrong backend) rather than surfacing it
  as a failure. Revert's effect is observed via the `revert` event + a messages refetch
  filtered by `revert_message_id` ([STATE-REVERT-001](06-state-model.md)); the call itself
  returns `204` with no body. **Why:** offering the wrong action per backend produces
  confusing errors. **Golden:** `internal/agent/agent.go:637` (Revert errors on unsupported),
  `internal/agent/claude.go:612` (revert internals), `internal/tui/sessionview.go:2276`.

### Messages — `GET /sessions/{id}/messages`

- **[OP-006] (MUST)** This is the **reconciliation source of truth**: a client MUST fetch it
  on open and after every (re)connection, and reconcile it monotonically against live stream
  state ([EVT-010](04-event-protocol.md), [INV-MONOTONIC-001](08-invariants.md)). The
  transcript is committed **asynchronously**, so a fetch fired mid-turn can lag the live
  stream — the client MUST NOT let the lagging snapshot erase streamed content. **Golden:**
  `internal/tui/sessionview.go:1521` (`handleSessionMessages`), `clank-mobile/src/lib/mergeMessages.ts`.

### Pending permission — `GET /sessions/{id}/pending-permission`

- **[OP-007] (MUST — known limitation)** This endpoint **currently always returns an empty
  array.** The host does not yet snapshot pending permissions; the SSE `permission` event is
  the only source. Consequence: a client that opens or reconnects to a session **blocked on a
  permission** cannot recover the prompt — the session looks idle but the agent is stuck. A
  client MUST still call it (its contract may change), MUST NOT treat `[]` as "no permission
  was ever pending," and SHOULD surface this state honestly if detected. See
  [INV-PENDING-PERM-GAP-001](08-invariants.md) for the recommended host fix. **Golden:**
  `internal/host/mux/sessions.go:236` (returns `[]`, `TODO: persistent permission snapshot`),
  `internal/tui/sessionview.go:808` (restore path, receives `[]`).

### Metadata mutations

- **[OP-008] (MUST)** `read`, `visibility`, `draft`, and `followup` are user-driven and MUST
  NOT be expected to bump `updated_at` (so the inbox doesn't reorder under the user). They
  may be reflected to other clients via the `meta` event. **Golden:**
  `internal/host/sessions_meta.go`, [DATA-011](03-data-model.md).

### Capability lookups

- **[OP-009] (SHOULD)** `GET /backends`, `/agents`, `/models` populate pickers. A client
  SHOULD fetch them lazily/cached, not on every render. They are pure reads. **Golden:**
  `internal/host/mux/mux.go:79`, `:87`, `:88`.
