# Implementation Checklist — Web · greenfield

> No web chat client exists yet. Placeholder to be filled when one is built **from this spec**
> ([CONF-GATE-003](../10-conformance.md)). Copy the structure from [_template.md](_template.md).
> The preview overlay's minimal chat box is tracked separately in
> [web-preview-overlay.md](web-preview-overlay.md) — it is a partial consumer, not this client.

**Platform:** Browser (TypeScript) · **Client kind:** full chat client (planned) ·
**Spec version targeted:** 0.2.0 · **Status:** ⛔ not started

## Start here (platform-specific guidance for the agnostic rules)

- **SSE** ([04](../04-event-protocol.md)): the native `EventSource` is the obvious fit, **but**
  it cannot set an `Authorization` header — for a bearer-gated stream ([CONN-010]) you must
  either use a `fetch()` + `ReadableStream` SSE parser (sets headers) or a query-param/cookie
  token scheme the gateway accepts. Decide early; it shapes the whole transport. Skip
  `connected` ([EVT-003]); end on close, not on an `end` event ([INV-NO-END-001]).
- **One stream** ([INV-SSE-DOUBLE-001], [NFR-CONC-002]): beware React StrictMode/double-mount
  and multiple tabs opening duplicate streams; dedupe per scope.
- **Reducer** ([06](../06-state-model.md)): a pure reducer (e.g. a store action) runnable on
  the conformance fixtures with no network ([NFR-TEST-001]).
- **Token storage** ([CONN-013]): prefer httpOnly cookies or in-memory + refresh; avoid
  `localStorage` for refresh tokens. Refresh policy per [CONN-012].
- **Reconnect** ([NFR-REL-001], [INV-RECONCILE-001]): if using `fetch`-stream, implement
  capped-backoff reconnect yourself (native `EventSource` auto-reconnects but gives no resync
  hook — you still MUST refetch+reconcile on reopen, driven by your transport, not the
  `reconnected` event, [INV-RECONNECT-SEMANTICS-001]).
- **Visibility** ([STATE-BACKGROUND-001]): use the Page Visibility API to suspend/resume +
  reconcile.

Fill the invariant + conformance tables from [_template.md](_template.md) as you implement.
