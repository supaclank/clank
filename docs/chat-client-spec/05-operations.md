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
| POST | `/sessions/{id}/revert` | `{ message_id }` | `204` | **No** | **Both** backends; empty id clears the marker on OpenCode. See [OP-005]. |
| POST | `/sessions/{id}/fork` | `{ message_id? }` | `200` `SessionInfo` | **No** | OpenCode only; Claude errors. |
| GET | `/sessions/{id}/messages` | — | `200` `MessageData[]` | Yes | The reconciliation source. Pure read — does not wake the agent ([OP-010]). |
| GET | `/sessions/{id}/pending-permission` | — | `200` `[]` | Yes | **Currently always empty** — see [OP-007]. |
| POST | `/sessions/{id}/permissions/{permID}/reply` | `{ allow, message? }` | `204` | Yes* | Answer a permission prompt. |
| POST | `/sessions/{id}/questions/{requestID}/reply` | `{ answers?, reject? }` | `204` | Yes* | Answer or dismiss a question prompt ([QST-001](11-interactive-tools.md)). |
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
internally, and every op that **dispatches into the backend** — `/message`, `/abort`,
`/revert`, `/fork`, permission/question replies — lazily rehydrates it), and `POST /sessions/{id}/stop`
(backend **process teardown** — distinct from `/abort`, which only interrupts the current
turn). Worktree / project / sync / auth / GitHub routes are likewise out of chat scope.
The GETs are reads: none of them is a wake/attach mechanism ([OP-010]).

This lazy rehydration MUST also recover a backend whose connection dropped *mid-session*
(status `dead`), not just a cold host after restart — otherwise a chat client, which has no
`/stop`/`/open`, cannot recover a wedged session. See
[INV-DEAD-BACKEND-REHYDRATE-001](08-invariants.md).

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
- **[OP-012] (MUST, 0.5.0)** On ACP-served backends a send that arrives while the session is
  **busy** is accepted (`204`) and **queued host-side** (FIFO, cap 8); prompts dispatch
  sequentially and `status` stays `busy` until the queue drains to `idle`. A queue-full send
  fails with a normal error envelope. Clients keep the OP-002 posture unchanged — user
  messages appear via the standard flow, and no steering/interjection semantics exist (the
  queued prompt starts a fresh turn). **Why:** ACP is one-prompt-at-a-time; queueing
  preserves today's send-while-busy UX without protocol tricks. **Golden:**
  `internal/agent/acp/backend_send.go` (`maxQueuedPrompts`, `runTurns`); regression
  `TestBackend_QueueWhileBusy`.

### Reply to permission — `POST /sessions/{id}/permissions/{permID}/reply`

- **[OP-003] (MUST)** Body is `{ "allow": bool, "message"?: string }`. `message` is the
  reason forwarded to the model when `allow=false` (e.g. plan-review comments) and is
  **ignored when `allow=true`**. A client MUST single-flight the reply per request ID. **Why:**
  double-replying races the backend; the deny-reason is the model's only feedback channel on
  a rejection. **Golden:** `internal/host/mux/sessions.go:252`, `internal/agent/claude_permissions.go:134`
  (`RespondPermission`), `internal/tui/sessionview.go:2168`.

### Reply to question — `POST /sessions/{id}/questions/{requestID}/reply`

- **[OP-011] (MUST)** Body is `{ "answers": [{ "selected"?: [labels], "custom"?: string }] }`
  (one entry per question, in order; an all-empty entry delegates that question) or
  `{ "reject": true }` to dismiss. `requestID` comes from the part's `question` tag
  ([QST-001](11-interactive-tools.md)). The backend translates answers into the provider
  transport — clients never format answer text. Single-flight per request ID. **Golden:**
  `internal/host/mux/sessions.go` (`handleQuestionReply`),
  `internal/agent/claude_permissions.go` (`RespondQuestion`),
  `internal/agent/opencode_questions.go` (`RespondQuestion`).

### Abort — `POST /sessions/{id}/abort`

- **[OP-004] (MUST)** Abort interrupts the current turn; it is best-effort and idempotent.
  The `204` means *the interrupt was delivered*, not that the agent stopped — the client
  observes the actual stop via `status` → `idle`/`error`. Aborting also **denies all parked
  permission prompts** server-side. Because the stop is *observed, not returned*, three
  client-side consequences follow on settle: (1) settle still-`running`/`pending` tool parts to
  a **terminal** status — the interrupted tool never returns
  ([INV-ABORT-SETTLE-TOOLS-001](08-invariants.md)); (2) the host may report `idle` **more than
  once** around an abort (a trailing `status` can follow the interrupt's own settle — observed
  on-device), so a "turn complete / done" affordance MUST stay suppressed until the user's
  **next send**, not just the first settle ([INV-ABORT-DONE-001](08-invariants.md)); and (3)
  clear parked permissions locally ([INV-ABORT-PERM-001](08-invariants.md)). **Why:** the abort
  UX must be driven by the status event, not the call return; pending permissions must be
  cleared, running tools must not spin forever, and a late idle must not masquerade as success. **Golden:**
  `internal/agent/claude_permissions.go:152` (`failPendingPermissions`),
  `internal/tui/sessionview.go:2190`, `:2203` (`startAbort`);
  `clank-mobile/…/PreviewOverlayContainer.kt` (settle-tools + `stoppedSinceLastSend`).

### Revert / fork — backend-specific

- **[OP-005] (MUST)** **Revert** is **Claude-only** since M2: the bespoke Claude backend
  keeps it (clank #68: file rollback + transcript truncation, non-empty `message_id`
  required); OpenCode lost it with the ACP cutover (approved cut — its bespoke
  revert-marker semantics went with the bespoke backend), and Codex never had it.
  **Fork** is capability-dependent: OpenCode supports it (ACP unstable `session/fork`,
  tip-only — a mid-history `message_id` is refused); Claude and Codex do not.
  The host does **not** gate by backend — `RevertSession`/`ForkSession` call straight
  through — but **(0.5.0)** an unsupported operation returns a **typed error**:
  `501 {code: "unsupported"}` instead of the old opaque 500 (mapped back to
  `agent.ErrUnsupported` for in-process clients). A client MUST handle `unsupported`
  gracefully — hide or soft-disable the affordance — never surface it as a generic failure;
  the TUI hides revert on non-Claude backends. Revert's effect is observed via the `revert`
  event + a messages refetch filtered by `revert_message_id`
  ([STATE-REVERT-001](06-state-model.md)); the call returns `204`. **Why:** an earlier draft of
  this rule wrongly said revert was Claude-only — it has worked on both since #68, yet the RN
  client still hides it on Claude (now stale); hiding revert where it works, or offering fork
  where it errors, both produce missing/confusing affordances. **Golden:**
  `internal/agent/claude.go:634` (Claude `Revert`), `:794` (Claude `Fork` errors),
  `internal/agent/acp/backend_history.go` (ACP `Fork` capability-gated tip-only;
  `Revert` → `ErrUnsupported`), `internal/host/service.go:1099` (`RevertSession` — no
  backend gate), `internal/tui/sessionview.go` (revert offered on Claude only; fork on any,
  501-tolerant).

### Messages — `GET /sessions/{id}/messages`

- **[OP-006] (MUST)** This is the **reconciliation source of truth**: a client MUST fetch it
  on open and after every (re)connection, and reconcile it monotonically against live stream
  state ([EVT-010](04-event-protocol.md), [INV-MONOTONIC-001](08-invariants.md)). The
  transcript is committed **asynchronously**, so a fetch fired mid-turn can lag the live
  stream — the client MUST NOT let the lagging snapshot erase streamed content. **Golden:**
  `internal/tui/sessionview.go:1521` (`handleSessionMessages`), `clank-mobile/src/lib/mergeMessages.ts`.

#### The messages GET is a pure read

- **[OP-010] (MUST, host + client)** Fetching messages is a **pure read**: it does not wake
  the agent, does not attach/open a backend for the session, and does not cause status
  transitions. A client MUST NOT rely on the fetch as a wake or "connect" mechanism, and MUST
  NOT expect opening a session screen to move `status` (a session at rest stays at its
  stored status — typically `idle` — until the user actually sends). Consequently a client
  MUST subscribe to the event stream *before or alongside* the snapshot fetch and reconcile
  the two by message/part IDs ([OP-006], [INV-CREATE-RACE-001](08-invariants.md)) — there is
  no event replay to paper over a late subscribe. **Why:** the eager-open behavior this
  replaces spawned the claude CLI on every history read (~25s on a cold Fly rootfs, measured
  in [clank#158](https://github.com/Acksell/clank/pull/158)) and registered a
  `starting`-status backend that clients rendered as a stuck "Starting…". Claude history is
  served straight from the on-disk transcript; OpenCode reads still boot its per-project
  server (an implementation detail, not a contract — do not rely on it). **Golden:**
  `internal/host/service.go` (`SessionMessages` — live backend, else `agent.TranscriptReader`),
  `internal/host/session_messages_test.go` (`TestSessionMessages_ClaudeColdReadDoesNotSpawnBackend`).

  **Considered and rejected — double-read to close the snapshot/stream race.** Reading
  messages twice (fetch, subscribe, fetch again) to catch events landing between snapshot
  and subscribe is unnecessary under this contract and stays rejected: when a turn is
  streaming, a **live backend exists** and serves the fetch exactly as before (the snapshot
  can lag the stream, and monotonic merge by IDs already handles that — [OP-006]); when **no
  live backend exists, nothing is producing events** (the agent process dies with the host
  daemon), so the on-disk transcript is complete and there is no gap for a second read to
  close. Subscribe-then-fetch plus ID-based reconciliation remains the whole protocol.

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
