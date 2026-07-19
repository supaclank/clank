# Implementation Checklist — Swift (iOS, native) · overlay client

> The native Swift client exists as the clank-mobile **iOS overlay** (floating
> prompt box): `clank-mobile/modules/preview-launcher/ios/`. Like its Kotlin
> sibling ([android-kotlin.md](android-kotlin.md)) it is a constrained surface
> — attach-only, no session list — so the session-list and create-race rows
> are N/A by scope. The full-screen iOS chat experience remains the
> [React Native client](react-native-ts.md).

**Platform:** Swift / URLSession · **Client kind:** preview-overlay chat client ·
**Spec version targeted:** 0.4.0 · **Last updated:** 2026-07-19 · **Status:** 🚧 overlay
client built (clank-mobile PR #134); pending a real iOS build + on-device pass;
conformance fixtures not yet authored

## Where the pieces live (clank-mobile `modules/preview-launcher/ios/`)

| Spec area | File |
|---|---|
| Reducer + monotonic merge ([06](../06-state-model.md)) | `Session/ChatTranscript.swift` — 1:1 port of the Kotlin `ChatTranscript.kt` / TS `mergeMessages.ts` core |
| SSE stream + supervision ([04](../04-event-protocol.md), [EVT-006]) | `Session/SessionEventStream.swift` + `Session/SSEFrameParser.swift` |
| Operations + auth refresh ([05](../05-operations.md), [CONN-012]) | `Session/SessionClient.swift` |
| Question tags ([11](../11-interactive-tools.md), QST-001..003) | `Session/AskQuestion.swift`, card in `Overlay/AskQuestionCard.swift` |
| Wire decoding ([03](../03-data-model.md), DATA-001) | `Session/WireParsing.swift`, `Session/JSONValue.swift` |
| Overlay UI / modes | `Overlay/*.swift` (SwiftUI; light-mode-forever parity with the Compose box) |

## Platform-specific guidance (kept from the placeholder, now applied)

- **SSE**: `URLSession` has no SSE type — `SessionEventStream` parses
  `event:`/`data:`/blank-line frames **byte-level** over `bytes(for:)`
  (Foundation's `AsyncLineSequence` is not trusted with the empty-line frame
  boundary). Skips `connected` ([EVT-003]); ends on stream close — the
  per-session `end` frame is not relied upon ([INV-NO-END-001]); reconnects 1s→15s capped, forever
  ([INV-STREAM-SUPERVISE-001]) with stream-generation guards
  ([INV-STALE-STREAM-001]).
- **Reducer**: pure value-type `apply*` functions (no network/UI imports), so
  the suite runs on macOS with the plain CLT — `cd modules/preview-launcher
  && swift test` (42 Swift Testing cases) — the [NFR-TEST-001] harness shape.
  When `docs/chat-client-spec/fixtures/` is authored, the same functions are
  the fixture target ([CONF-GATE-003]).
- **Token storage** ([CONN-013]): the overlay receives tokens from the host
  app via `PreviewSessionContext` (it never mints them); Keychain persistence
  belongs to the future windowed-preview restore path.
- **Concurrency**: one stream consumed by a single `for await` loop hopping to
  `@MainActor`; single-flight refresh via an actor gate; send/reply
  single-flight in the controller ([NFR-CONC-*]).
- **Reconnect** ([NFR-REL-001], [INV-RECONCILE-001]): reconcile
  (messages+status refetch, monotonic merge) on every `.connected`, driven by
  own transport state — never by the `reconnected` event
  ([INV-RECONNECT-SEMANTICS-001]); foreground return resubscribes when the
  socket has been silent ([INV-HEARTBEAT-GAP-001], [STATE-BACKGROUND-001]).

## Invariant status (overlay scope — mirrors android-kotlin's N/A rows)

Implemented: INV-MONOTONIC-001, INV-DELTA-001, INV-TOOL-MERGE-001,
INV-TOOL-RESULT-CARRIER-001, INV-MSGID-001, INV-SHELL-001,
INV-OPTIMISTIC-001 (echo + content dedupe; id backfill via merge),
INV-ABORT-SETTLE-TOOLS-001, INV-ABORT-DONE-001, INV-REVERT-001,
INV-RECONCILE-001, INV-RECONNECT-SEMANTICS-001, INV-NO-END-001,
INV-STREAM-SUPERVISE-001, INV-STALE-STREAM-001, INV-SSE-DOUBLE-001
(generation counter), INV-HEARTBEAT-GAP-001 (liveness clock + foreground
resubscribe), INV-INTERACTIVE-001 + QST-001..003 (question tag path).

N/A on this surface (as android-kotlin): INV-CREATE-RACE-001 (attach-only;
lazy-create posts the first prompt), INV-META-REPLACE-001 /
INV-SIDEBAR-META-001 / LIST-* (no session list), INV-PERMMODE-* (no mode
picker), `ExitPlanMode` plan card + inline comments (deferred, as on the
Kotlin overlay).

Known gaps: INV-PENDING-PERM-GAP-001 (host limitation, same as every
client); generic `permission` events are not rendered (parity with the
Kotlin overlay — gated tools resolve via the app or the question tag).

Fill the full conformance table from [_template.md](_template.md) when the
fixtures land and an on-device pass has run.
