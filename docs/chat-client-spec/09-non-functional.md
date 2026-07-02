# 09 · Non-Functional Requirements

Functional rules say *what* happens; these say *how well*. Several recurring chat bugs are
NFR violations in disguise — a doubled socket is a concurrency failure, a refetch erasing
text is a consistency failure, a sign-out on a transient 401 is a reliability failure — so
these are normative, not aspirational.

## Latency & perceived responsiveness

- **[NFR-LAT-001] (MUST)** A user action MUST get immediate local feedback: the sent message
  echoes optimistically ([INV-OPTIMISTIC-001]); the composer locks the instant a permission
  arrives ([VIEW-INPUT-LOCK-001]). Never block the UI on a network round-trip for feedback
  the client already knows.
- **[NFR-LAT-002] (MUST)** First open / cold send MUST tolerate sprite-wake latency with a
  "waking/starting" affordance and a generous timeout, not a fast failure ([CONN-030]).

## Consistency & convergence

- **[NFR-CONS-001] (MUST)** The rendered transcript MUST **converge** to the host's canonical
  transcript: for any interleaving of stream events and refetches, the client's state equals
  what monotonic-by-ID reduction of all inputs yields ([STATE-001], [INV-MONOTONIC-001]). This
  is the formal statement of "what the user sees corresponds to the spec," and it is exactly
  what conformance ([10](10-conformance.md)) checks.
- **[NFR-CONS-002] (MUST)** Metadata (status, title, revert, visibility) MUST converge to the
  host via `meta`/`title`/`status`/`revert` and `GET /sessions/{id}` — never drift on stale
  local edits ([INV-META-REPLACE-001]).

## Reliability & resilience

- **[NFR-REL-001] (MUST)** Reconnect the stream automatically with **capped backoff**, then
  reconcile ([INV-RECONCILE-001]). A single drop MUST NOT require user action. The wire-level
  contract — which termination paths this covers, and the foreground-resubscribe duty on
  suspendable platforms — is [EVT-006](04-event-protocol.md) /
  [INV-STREAM-SUPERVISE-001](08-invariants.md). (RN's old one-refresh-reconnect-then-give-up
  was the shipped counterexample: a session created after the stream died froze at
  "Working…" forever.) **Golden:** `clank-mobile/src/api/events.ts` (`scheduleReconnect`:
  1s→30s full-jitter backoff), `…/session/SessionEventStream.kt:121` (1s→15s backoff).
- **[NFR-REL-002] (SHOULD)** Detect half-open connections (no heartbeat exists, [INV-HEARTBEAT-GAP-001])
  via transport keepalive or a liveness timeout, then treat as a drop. Where neither is
  available (RN), resubscribe on app foreground instead ([EVT-006]).
- **[NFR-REL-003] (MUST)** Distinguish recoverable from terminal: transient network / 5xx /
  refresh-timeout keep the session ([CONN-012]); only `invalid_grant` signs out.

## Availability & degradation

- **[NFR-AVAIL-001] (SHOULD)** Reads SHOULD degrade gracefully when offline: show the last
  reconciled transcript read-only with a clear "disconnected" state, rather than a blank or
  error screen. Writes (send/abort/reply) require connectivity and SHOULD be disabled-with-reason
  when disconnected.
- **[NFR-AVAIL-002] (SHOULD NOT)** A client SHOULD NOT perform routine background polling that
  wakes the sprite; rely on the event stream. (Object-store/sync traffic belongs on the
  gateway, not the sandbox.)

## Security

- **[NFR-SEC-001] (MUST)** Store refresh tokens in the platform secure store; never log tokens
  or PII; use TLS for the gateway ([CONN-013]).
- **[NFR-SEC-002] (MUST)** Permission prompts are security-critical UX: a client MUST present
  exactly what is being authorized (tool + target, from the correlated part) and MUST NOT
  auto-approve or pre-select "allow." The decision is the user's. **Golden:**
  `internal/agent/claude_permissions.go:172` (`describeToolCall`).
- **[NFR-SEC-003] (forward)** User code is confidential to the servers (E2E-encryption is a
  forward requirement): a client MUST NOT assume it may send plaintext code/content to any
  endpoint that is specified as opaque/encrypted once that lands. Today informational; will
  become normative with E2E sync.

## Concurrency

- **[NFR-CONC-001] (MUST)** Single-flight the mutating operations: one send in flight
  ([OP-002]), one permission reply per request ([INV-PERM-SINGLEFLIGHT-001]), one create
  ([OP-001]).
- **[NFR-CONC-002] (MUST)** Exactly one SSE stream per scope; guard async connect against
  teardown ([INV-SSE-DOUBLE-001]).
- **[NFR-CONC-003] (MUST)** Keep the stream read path cheap; do heavy work off it, so the
  256-event server buffer doesn't overflow and drop events ([EVT-011]).

## Resource & mobile

- **[NFR-BAT-001] (SHOULD)** Prefer a single multiplexed stream; suspend it on background and
  reconnect+reconcile on foreground ([STATE-BACKGROUND-001]). Don't hold a live socket while
  backgrounded indefinitely.

## Observability

- **[NFR-OBS-001] (SHOULD)** Log event handling with correlation keys — `session_id`,
  `external_id`, `request_id`, `tool_use_id`, part id — so divergence is diagnosable.
  Redact per [NFR-SEC-001].
- **[NFR-OBS-002] (SHOULD)** Make the reducer pure and inspectable (state in, state out) so a
  fixture replay ([10](10-conformance.md)) can snapshot it. **Golden:**
  `clank-mobile/src/hooks/dispatch.ts` (pure, unit-tested against a mock cache).

## Forward compatibility & testability

- **[NFR-COMPAT-001] (MUST)** Tolerate unknown event types, enum values, and object fields
  ([DATA-001]). Schema changes are additive.
- **[NFR-TEST-001] (MUST)** The client's reducer MUST be drivable by a deterministic input
  trace with no real network/clock, so it can run the conformance fixtures ([10](10-conformance.md)).
