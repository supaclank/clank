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
C: POST /sessions/{id}/message {text}                       ── no config = no mode change [DATA-040]
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

## ~~4. Plan mode + ExitPlanMode~~ / ~~5. Questions~~ — retired 0.6.0 (M3)

Plan review (approve / request-changes / deny) and structured question prompts went with
the ACP migration. `plan` **mode** survives where the agent advertises it — the agent's
`ExitPlanMode` request now arrives as an ordinary permission prompt (flow 3), with the plan
text rendered from the tool part. See the retire register in
[README](README.md#retired-in-060-the-acp-migration-m3).

## ~~6. Revert~~ — retired 0.6.0 (M3)

No backend implements revert; the endpoint and its `revert` event are gone.
See the retire register in [README](README.md#retired-in-060-the-acp-migration-m3).

## 7. Abort / cancel

```
agent is busy (status=busy), composer may be locked by a pending permission
C: user cancels (ctrl+c / stop)
C: aborting=true; stoppedSinceLastSend=true; show "Cancelling…" ── [STATE-ABORT-RESULT-001], [VIEW-CANCELLING-001]
C: POST /sessions/{id}/abort                      ── [OP-004]
H: denies all parked permissions ("aborted")      ── failPendingPermissions
H→C: 204; (error events suppressed while aborting) ── [STATE-ERR-001]
H→C: status busy→idle → finalize "Cancelled"; aborting=false; clear pending perms
C: settle still-running tool parts → terminal     ── [INV-ABORT-SETTLE-TOOLS-001]
H→C: (maybe) a DELAYED status→idle arrives later  ── still no "Done": stoppedSinceLastSend stays set [INV-ABORT-DONE-001]
C: user's NEXT send clears stoppedSinceLastSend    ── [STATE-SUBMIT-001]
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

The GETs are pure reads: they do not wake the agent or flip `status`, so a session at rest
stays at its stored status until the user sends ([OP-010]). The stream (step 1) is the only
liveness channel — subscribe before/alongside the fetches, never instead of them.

- **[FLOW-RECONNECT-001] (MUST)** A client MUST run this reconcile after **every** stream
  (re)connection, not only the first, and MUST drive it from its own transport state — not
  from a `reconnected` event (that is the backend's link, [EVT-020]). Getting *back* to a
  connected stream in the first place is [EVT-006]'s supervised-reconnect duty. **Golden:**
  `internal/tui/sessionview.go:511`, `clank-mobile/src/hooks/useEventStream.ts`
  (`onReconnect` → `resyncAfterStreamGap`).
