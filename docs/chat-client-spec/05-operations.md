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
| POST | `/sessions/{id}/fork` | `{ message_id? }` | `200` `SessionInfo` | **No** | Capability-gated; `501 unsupported` where the agent has no fork. See [OP-005]. |
| GET | `/sessions/{id}/messages` | — | `200` `MessageData[]` | Yes | The reconciliation source. Pure read — does not wake the agent ([OP-010]). |
| GET | `/sessions/{id}/pending-permission` | — | `200` `PermissionData[]` | Yes | Parked prompts on the live backend, oldest first; in-memory only — see [OP-007]. |
| POST | `/sessions/{id}/permissions/{permID}/reply` | `{ allow, message? }` | `204` | Yes* | Answer a permission prompt. |
| POST | `/sessions/{id}/read` | — | `204` | Yes | Mark read; does not bump `updated_at`. |
| POST | `/sessions/{id}/visibility` | `{ visibility }` | `204` | Yes | `done`/`archived`/`""`. |
| POST | `/sessions/{id}/draft` | `{ draft }` | `204` | Yes | Save composer draft. |
| POST | `/sessions/{id}/followup` | — | `200` `SessionInfo` | Yes | Toggle follow-up flag. |
| GET | `/events`, `/sessions/{id}/events` | — | `200` SSE | — | See [04](04-event-protocol.md). |
| GET | `/backends` | — | `200` `BackendInfo[]` | Yes | Capability lookup — see [OP-009]. |
| GET | `/config-options` | query | `200` `ConfigOption[]` | Yes | Live agent config options, probed on demand (slow by design) — see [OP-013]. |
| GET | `/agents` | query | `200` `[]` | Yes | **Deprecated 0.6.0**, always empty — see [OP-014]. |
| GET | `/presets` | `backend?` | `200` `Preset[]` | Yes | Built-in + user presets; built-ins first — see [OP-016]. |
| POST | `/presets` | `Preset` | `200` | Yes | Create/replace a USER preset; built-in ids reserved. |
| DELETE | `/presets/{id}` | — | `204` | Yes | Delete a USER preset; unknown id `400`. |

`*` reply is idempotent at the host (an unknown/duplicate permID errors fast); clients
still single-flight it in the UI ([INV-PERM-SINGLEFLIGHT-001](08-invariants.md)).

**Intentionally omitted.** The host exposes session-lifecycle/host-management routes a chat
client driving Create + `/message` + SSE does not need: `POST /sessions/{id}/open` and
`POST /sessions/{id}/open-and-send` (backend rehydration/orchestration — `POST /sessions` already opens-and-sends
internally, and every op that **dispatches into the backend** — `/message`, `/abort`,
`/fork`, permission replies — lazily rehydrates it), and `POST /sessions/{id}/stop`
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

### Presets — `GET/POST /presets`, `DELETE /presets/{id}`

- **[OP-016] (MUST, 0.6.2)** Presets are named config bundles the HOST stores and serves but
  **never applies** — the client is the only thing that sends `config` on requests. Built-ins
  are declared per host by its provisioner (`$CLANK_BUILTIN_PRESETS`, serialized from
  `internal/agent/presets`; sandboxes ship the permissive set, everything else the
  conservative Workstation set) and are immutable — duplicate to customize. Each backend's
  **Default** preset doubles as the create contract: its config keys are the REQUIRED keys
  for `POST /sessions` ([DATA-040](03-data-model.md)). A client's create flow is: `GET
  /presets` → chosen preset's config → overlay explicit picks → send. **Why:** per-host
  storage is what lets every client of a host (mobile, TUI, the web preview overlay) share
  one preset set, and keeps a sandbox's permissive defaults from ever leaking onto a bridged
  laptop — each host serves its own. **Golden:** `internal/host/presetstore.go`,
  `internal/host/mux/presets.go`; client side, headless flows apply the Default preset:
  `defaultPresetConfig` in `internal/webpreview/overlay/chat.js` (web overlay) and
  `internal/cli/clankcli/session_create.go` (`please`); regressions
  `TestPresets_CRUDOverHTTP`, `TestRunPlease_CreatesSessionAndRecordsLastSession`.

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
  a rejection. **Golden:** `internal/host/mux/sessions.go`,
  `internal/agent/acp/backend_permission.go` (`RespondPermission`), `internal/tui/sessionview.go`.
- **[OP-015] (MUST, 0.6.1)** On ACP-served backends the deny `message` is delivered as the
  session's **next user prompt**, not as part of the permission outcome — ACP outcomes carry
  an option id and nothing else. Two consequences a client MUST expect: the text appears in
  the transcript as an ordinary **user message** (it is not an invisible side channel), and
  the session goes **busy** again as the follow-up turn runs. A `5xx` on the reply can mean
  the denial landed but the message did not; the reply is still single-flight, and the user
  can resend the text as a normal message. **Why:** this is what makes plan revision work —
  rejecting `ExitPlanMode` keeps the session in plan mode and ends the turn, and the queued
  message is what asks for the changes. **Golden:**
  `internal/agent/acp/backend_permission.go`; regression
  `TestBackend_DenyMessageBecomesFollowUpPrompt`.

### ~~Reply to question~~ — retired 0.6.0 (endpoint removed)

- **~~[OP-011]~~ (retired 0.6.0 — M3) (MUST)** Body is `{ "answers": [{ "selected"?: [labels], "custom"?: string }] }`
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

- **[OP-005] (MUST)** **Revert is retired** (0.6.0): no backend implements it, the endpoint
  is gone, and `revert_message_id` is a permanently-empty field until 1.0. **Fork** is
  capability-gated: the host asks the agent, and OpenCode advertises it today while Claude
  and Codex do not; ACP fork has no message anchor, so only a **tip** fork is honest — a
  mid-history `message_id` is refused rather than silently forking the tip. Both an
  unsupported backend and a mid-history request return the typed
  `501 {code: "unsupported"}` (mapped back to `agent.ErrUnsupported` in-process); a client
  MUST handle it gracefully — hide or soft-disable the affordance — never surface it as a
  generic failure. **Why:** offering an action the host will refuse, or surfacing a refusal
  as a crash, both read as breakage. **Golden:**
  `internal/agent/acp/backend_history.go` (`Fork` capability gate + tip-only check),
  `internal/host/mux/mux.go` (`writeError` → 501 `unsupported`),
  `internal/tui/sessionview.go` (fork-only action menu, 501-tolerant banner).

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

- **[OP-007] (MUST, rewritten 0.6.3)** Returns the permission requests currently **parked on
  the session's live backend** (`PermissionData[]`, oldest first) — the same payloads the SSE
  `permission` event carried. The SSE event is not replayed, so this endpoint is the only way
  a client that opens or reconnects to a **blocked** session recovers the prompt. A client
  MUST call it on open and after every reconnect (alongside the [OP-006] snapshot) and
  replace its local pending queue with the result
  ([VIEW-PENDING-PERM-001](06-state-model.md)). The queue is **in-memory only, never
  persisted**: a host restart kills the agent process and its parked requests together, so
  `[]` from a restarted host is the honest answer, not a lost prompt. The read follows
  [OP-010] discipline — it never wakes/rehydrates a backend; with no live backend it returns
  `[]` (unknown session ids still 404). `[]` therefore means "nothing is parked *now*," not
  "no permission was ever pending." **Golden:** `internal/host/mux/sessions.go`
  (`handlePendingPermissions`), `internal/host/service.go` (`PendingPermissions` — live
  registry only), `internal/agent/acp/backend_permission.go` (`PendingPermissions`),
  `internal/tui/sessionview.go:574` (restore path). **Conformance:** `CONF-PENDING-PERM-RESTORE`
  (`internal/cli/daemoncli/sessions_wire_e2e_test.go`,
  `TestWire_PendingPermission_RoundTrip` / `TestWire_PendingPermission_EmptyAfterRestart`).

### Metadata mutations

- **[OP-008] (MUST)** `read`, `visibility`, `draft`, and `followup` are user-driven and MUST
  NOT be expected to bump `updated_at` (so the inbox doesn't reorder under the user). They
  may be reflected to other clients via the `meta` event. **Golden:**
  `internal/host/sessions_meta.go`, [DATA-011](03-data-model.md).

### Capability lookups

- **[OP-009] (SHOULD)** `GET /backends` and `GET /presets` populate pickers; both are pure
  host-store reads a client can fetch eagerly. `GET /config-options` is NOT in that class —
  see [OP-013]. **Golden:** `internal/host/mux`.

- **[OP-013] (MUST, rewritten 0.6.2)** `GET /config-options?backend=&git_local_path=|
  git_worktree_id=` returns the agent's **live advertised config options**
  ([DATA-041](03-data-model.md)) for that project by opening one short-lived agent session
  server-side — ACP advertises options only on session open, so this is the only
  pre-session source. It is **blocking, on-demand, and uncached**: expect seconds, call it
  when a knob editor opens (behind a spinner), never eagerly on compose open or per render.
  The host single-flights concurrent requests (per backend, per project dir for per-dir
  backends like opencode; host-wide otherwise) and surfaces probe failures as errors — an
  empty `200` list means the agent really advertises nothing. Everyday flows never need this
  endpoint: presets carry the create config ([OP-016]), and a live session's options ride
  `config_options` on runtime session info. **Retired with this rewrite:** `GET /modes`,
  `GET /models`, and the prewarm/refine contract (serve-then-refine, `catalogRefineDelay`
  re-reads, per-dir background probes, the persisted catalog). **Golden:**
  `internal/host/backends_acp.go` (`ConfigOptions`), `internal/host/mux/catalog.go`.

- **[OP-014] (SHOULD)** `GET /agents` is **deprecated in 0.6.0** and returns `[]` on every
  ACP-served backend. OpenCode agents are now agent-advertised session modes: clients
  SHOULD read `available_modes`/`config_options` on runtime session info (or
  `/config-options` pre-session) instead.
