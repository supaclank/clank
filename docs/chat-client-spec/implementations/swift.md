# Implementation Checklist — Swift (iOS, native) · greenfield

> No native Swift chat client exists yet. This is a placeholder to be filled when one is
> built **from this spec** (the acceptance test, [CONF-GATE-003](../10-conformance.md)).
> Copy the structure from [_template.md](_template.md). Until then, the iOS chat experience is
> the [React Native client](react-native-ts.md).

**Platform:** Swift / URLSession · **Client kind:** full chat client (planned) ·
**Spec version targeted:** 0.2.0 · **Status:** ⛔ not started

## Start here (platform-specific guidance for the agnostic rules)

- **SSE** ([04](../04-event-protocol.md)): `URLSession` has no SSE type; use a streaming
  `URLSessionDataTask` / `bytes(for:)` and parse `event:`/`data:`/blank-line frames yourself.
  Skip `connected` ([EVT-003]); end on stream close, never on an `end` event ([INV-NO-END-001]).
- **Reducer** ([06](../06-state-model.md)): implement as a pure `apply(state, input) -> state`
  value-type reducer so it can run the conformance fixtures with no network ([NFR-TEST-001]).
- **Token storage** ([CONN-013]): Keychain; refresh policy per [CONN-012].
- **Concurrency** ([NFR-CONC-*]): one stream; single-flight create/send/reply; structured
  concurrency (an actor) to serialize state mutation.
- **Reconnect** ([NFR-REL-001], [INV-RECONCILE-001]): capped-backoff reconnect driven by your
  own transport state, then refetch+reconcile — do **not** key off the `reconnected` event
  ([INV-RECONNECT-SEMANTICS-001]).

Fill the invariant + conformance tables from [_template.md](_template.md) as you implement.
