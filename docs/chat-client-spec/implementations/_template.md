# Implementation Checklist — &lt;PLATFORM&gt;

> Copy this file to `<platform>.md` and fill it in. This is the **"how"** companion to the
> language-agnostic core ([../README.md](../README.md)): for each rule and conformance
> scenario, record *how this platform satisfies it* and its status. Keep it in sync as the
> client evolves — a green checklist with a failing fixture is a lie.

**Platform:** … · **Client kind:** full chat client | partial consumer (describe) ·
**Spec version targeted:** 0.1.0 · **Last updated:** YYYY-MM-DD

**Status legend:** ✅ done · 🟡 partial · ⛔ not started · N/A (justify)

## Where the pieces live

| Concern | This platform's component |
|---|---|
| HTTP client / bearer injection | … |
| SSE stream | … (library / API) |
| Reducer (apply event → state) | … |
| Transcript merge / reconcile | … |
| Permission UI + reply | … |
| Token storage / refresh | … |
| Conformance harness (`applyInput`/`project`) | … |

## Invariants ([08](../08-invariants.md)) — the firewall

| Rule | Status | How on this platform | Notes |
|---|---|---|---|
| INV-CREATE-RACE-001 (subscribe before create) | | | |
| INV-STALE-STREAM-001 (stream identity) | | | |
| INV-SSE-DOUBLE-001 (exactly one stream) | | | |
| INV-NO-END-001 (no `end` frame) | | | |
| INV-DELTA-001 (is_delta append/replace) | | | |
| INV-TOOL-MERGE-001 (merge input+output) | | | |
| INV-MONOTONIC-001 (refetch never shrinks) | | | |
| INV-MSGID-001 (resolve part owner) | | | |
| INV-SHELL-001 (drop shell / dedup) | | | |
| INV-OPTIMISTIC-001 (echo + backfill + dedup) | | | |
| INV-PERM-SINGLEFLIGHT-001 (lock + single reply) | | | |
| INV-DENY-SETTLE-001 (settle tools on deny) | | | |
| INV-ABORT-PERM-001 (abort clears perms, survives) | | | |
| INV-PERMMODE-001 (`""` = no change) | | | |
| INV-PERMMODE-EXITPLAN-001 (ExitPlanMode) | | | |
| INV-META-REPLACE-001 (meta wholesale) | | | |
| INV-REVERT-001 (revert filter) | | | |
| INV-RECONCILE-001 (reconcile on every reconnect) | | | |
| INV-RECONNECT-SEMANTICS-001 (`reconnected` ≠ socket) | | | |
| INV-PENDING-PERM-GAP-001 (honest blocked-state) | | | |
| INV-HEARTBEAT-GAP-001 (liveness detection) | | | |

## Conformance scenarios ([10](../10-conformance.md)) — the gate

| Scenario | Pass? | Fixture wired | Notes |
|---|---|---|---|
| CONF-CREATE-RACE | | | |
| CONF-STALE-STREAM | | | |
| CONF-SINGLE-STREAM | | | |
| CONF-NO-END | | | |
| CONF-STREAM-DELTA | | | |
| CONF-TOOL-MERGE | | | |
| CONF-MONOTONIC | | | |
| CONF-MSGID-OWNER | | | |
| CONF-SHELL-DROP | | | |
| CONF-OPTIMISTIC-BACKFILL | | | |
| CONF-PERM-LOCK | | | |
| CONF-PERM-SINGLEFLIGHT | | | |
| CONF-DENY-SETTLE | | | |
| CONF-ABORT-PERM | | | |
| CONF-ABORT-NOISE | | | |
| CONF-PERMMODE-NOCHANGE | | | |
| CONF-PLAN-EXIT | | | |
| CONF-META-REPLACE | | | |
| CONF-REVERT-FILTER | | | |
| CONF-RECONCILE | | | |
| CONF-RECONNECT-SEMANTICS | | | |
| CONF-PENDING-PERM-GAP | | | |

## Supporting layers (quick check)

- [ ] Connection & auth ([02](../02-connection-and-auth.md)): base URL, PKCE, bearer on every
  request incl. SSE, proactive refresh, 401→refresh-once, sign out only on `invalid_grant`,
  secure token store, generous cold-start timeout.
- [ ] Data model ([03](../03-data-model.md)): tolerate unknown fields/enums; runtime-only
  fields not persisted as identity.
- [ ] Operations ([05](../05-operations.md)): single-flight create/send/reply; send is
  fire-and-forget; revert vs fork per backend; messages refetch as reconcile source.
- [ ] View projections ([06 §projections](../06-state-model.md)): composer lock, streaming vs
  settled, cancelling, error banner, revert prefill, pending-permission card.

## Platform gotchas

Record platform-specific traps discovered here (so the next maintainer doesn't relearn them).
Examples of the *kind* of thing: SSE library quirks, async-teardown ordering, background/
foreground lifecycle, secure-storage APIs, JSON decoding of unknown enums.

- …

## Open gaps / deviations

List any rule not satisfied and why, with a tracking link.

- …
