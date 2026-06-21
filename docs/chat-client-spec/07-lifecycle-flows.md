# 07 · Lifecycle Flows

End-to-end sequences that tie the rules together. Each names the rules it exercises; each
maps to a conformance scenario in [10](10-conformance.md). `C`=client, `G`=gateway,
`H`=host, `B`=backend.

## 1. Create a new session

The ordering is load-bearing: **subscribe before create**, or the first events are lost.

```
C: open SSE  GET /events                         ── [STATE-CONNECT-001], [INV-CREATE-RACE-001]
H→C: event: connected {subscriber_id}            ── skipped [EVT-003]
C: POST /sessions {StartRequest}                 ── single-flight [OP-001]
H→C: 201 SessionInfo {id, status:starting}
C: render optimistic first user message          ── [STATE-SUBMIT-001]
H→C: status starting→busy → part… → status busy→idle
C: GET /sessions/{id}/messages → reconcile       ── [OP-006], monotonic [STATE-001]
```

If subscribe happened *after* create, the `starting→busy` and early `part` events fire into
the void (no replay, [EVT-010]). Golden: `internal/tui/sessionview_compose.go:284`.

## 2. Follow-up message

```
C: user submits text
C: append optimistic user msg (no id), submitting=true, follow=true, lock composer
     └─ also clears revert_message_id                       ── [STATE-SUBMIT-001]
C: POST /sessions/{id}/message {text, permission_mode:""}   ── "" = no mode change [DATA-040]
H→C: 204                                                    ── dispatched ≠ done [OP-002]
H→C: status idle→busy
H→C: message {role:user, id} → backfill id on optimistic msg ── [STATE-MSG-001]
H→C: part (is_delta=true)…  → append streaming text          ── [STATE-PART-001]
H→C: status busy→idle → settle parts, unlock composer        ── [STATE-STATUS-001]
```

The `204` is not completion; only `status→idle` is. Concurrent sends are forbidden
(`submitting` guard, [OP-002]).

## 3. Permission prompt (mode = default / acceptEdits)

A tool the current mode doesn't auto-approve pauses the turn:

```
H→C: part {id:T, type:tool_call, tool:bash, input, status:running}  ── context, pre-emitted
H→C: permission {request_id:P, tool:bash, tool_use_id:T}            ── [STATE-PERM-001]
C: enqueue P; LOCK composer                                          ── [VIEW-INPUT-LOCK-001]
C: render prompt with the T tool-call card (matched by tool_use_id)  ── [VIEW-PENDING-PERM-001]
   ── agent is BLOCKED here; nothing else is due until answered ──
C: user answers (y/n). set replyInFlight=P                           ── single-flight [STATE-PERM-ANSWER-001]
C: POST /sessions/{id}/permissions/P/reply {allow}                   ── [OP-003]
H→C: 204; clear replyInFlight; pop P; record outcome                 ── [STATE-PERM-RESULT-001]
   on deny: settle running tools to error + refetch messages/pending ── markRunningToolsFailed
H→C: (allow) part {id:T, …, status:completed/output…} → merge        ── [STATE-PART-001]
```

The blocking is real: the host parks the backend's reader, so the tool `input` is emitted
*before* the permission and no further events arrive until the answer. Golden:
`internal/agent/claude_permissions.go:29`–`:127`.

## 4. Plan mode + ExitPlanMode (interactive tool)

In `plan` mode the agent works read-only, then calls `ExitPlanMode` to ask permission to
proceed. `ExitPlanMode` is a normal `permission` prompt (its plan text rides in the
correlated tool-call part's `input`):

```
session started with permission_mode: plan
H→C: part {id:T, type:tool_call, tool:ExitPlanMode, input:{plan…}}
H→C: permission {request_id:P, tool:ExitPlanMode, tool_use_id:T}
C: render the plan from part T's input; LOCK composer        ── [VIEW-PENDING-PERM-001]
C: user approves → POST …/reply {allow:true}                 ── [OP-003]
   (backend auto-exits plan mode; it resets its tracked mode)
C: next send carries permission_mode:"" → backend re-asserts the user's chosen mode
```

- **[FLOW-PLAN-001] (MUST)** A client MUST present `ExitPlanMode` as an approve/revise
  decision rendered from the tool-call part's `input` (the plan), not as a normal tool —
  **Approve** switches to build mode (the agent implements), **Revise** stays in plan mode
  (the agent re-plans). The answer goes back per [11 · Interactive Tools](11-interactive-tools.md)
  via the **permission reply** (allow to approve, deny-with-notes to revise) — not a
  `permission_mode` send; ordinary follow-ups keep `permission_mode:""` ([DATA-040]).
  The **TUI does not render this** — the RN client is the reference. **Why:** treating it as
  opaque hides the plan; re-sending a non-empty default would re-flip the mode. **Golden:**
  `clank-mobile/src/lib/planReview.ts`, `internal/agent/claude_permissions.go:97` (mode reset
  on approval).

## 5. AskUserQuestion (interactive tool)

Same shape as ExitPlanMode: a tool-call part carries typed questions in `input`; a paired
`permission` gates it. The client renders an inline multiple-choice card and answers via the
permission reply.

- **[FLOW-ASK-001] (MUST)** A client MUST parse the question(s) from the `AskUserQuestion`
  tool-call part `input`, render a choice UI, and submit the answer per
  [11 · Interactive Tools](11-interactive-tools.md) (today: a formatted `SendMessage`; resolve
  the gating permission when one is pending). A **terminal** part `status` (completed/error)
  means it was already answered (possibly on another client) — the card MUST clear rather than
  reappear. The **TUI does not render this** — the RN client is the reference. **Why:**
  stop-and-wait tools render from the part; a terminal status is the "already handled" signal.
  **Golden:** `clank-mobile/src/lib/askQuestion.ts`, `…/session/SessionEventStream.kt:230`
  (clears on terminal status).

## 6. Revert

```
C: user picks "revert to this message" (id:M)
C: POST /sessions/{id}/revert {message_id:M}     ── Claude only [OP-005]
H: rolls files back + truncates transcript at M  ── internals: claude.go:612
H→C: 204
H→C: revert {message_id:M}                        ── [STATE-REVERT-001]
C: set revert_message_id=M; prefill composer with M's prompt; refetch messages
C: apply revert filter → drop M and everything after  ── [INV-REVERT-001]
   (next send clears revert_message_id → tail reappears) ── [STATE-SUBMIT-001]
```

## 7. Abort / cancel

```
agent is busy (status=busy), composer may be locked by a pending permission
C: user cancels (ctrl+c / stop)
C: aborting=true; show "Cancelling…"             ── [STATE-ABORT-RESULT-001], [VIEW-CANCELLING-001]
C: POST /sessions/{id}/abort                      ── [OP-004]
H: denies all parked permissions ("aborted")      ── failPendingPermissions
H→C: 204; (error events suppressed while aborting) ── [STATE-ERR-001]
H→C: status busy→idle → finalize "Cancelled"; aborting=false; clear pending perms
```

## 8. Reconnect / late-join (reconcile)

The transcript-recovery flow, run on first open and after any drop:

```
C: (re)open SSE stream                            ── single stream [EVT-001]
C: GET /sessions/{id}                             ── current status/metadata
C: GET /sessions/{id}/messages → monotonic merge  ── [OP-006], [STATE-001]
C: GET /sessions/{id}/pending-permission          ── ⚠ returns [] today [OP-007]
   → recovers transcript missed during the gap (no replay [EVT-010])
   → does NOT recover a permission the session is blocked on (gap [INV-PENDING-PERM-GAP-001])
```

- **[FLOW-RECONNECT-001] (MUST)** A client MUST run this reconcile after **every** stream
  (re)connection, not only the first, and MUST drive it from its own transport state — not
  from a `reconnected` event (that is the backend's link, [EVT-020]). **Golden:**
  `internal/tui/sessionview.go:511`, `clank-mobile/src/hooks/dispatch.ts:196`.
