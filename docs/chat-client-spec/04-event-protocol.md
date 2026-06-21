# 04 · Event Protocol

Realtime updates flow over **Server-Sent Events**. This document defines the transport, the
envelope, the **delivery guarantees** (the part clients get wrong), and the full event
catalog. How a client *reacts* to each event is in [06-state-model.md](06-state-model.md);
this is the wire.

## Transport

- **[EVT-001] (MUST)** A client subscribes to events with a long-lived
  `GET /events` (global: all sessions) or `GET /sessions/{id}/events` (one session). It MUST
  maintain **exactly one** stream for a given scope at a time. **Why:** duplicate streams
  deliver every event twice; this was a shipped mobile bug. See [INV-SSE-DOUBLE-001](08-invariants.md).
  **Golden:** `internal/host/mux/mux.go:78` (global), `:142` (per-session);
  `internal/daemonclient/sessions.go` (`Subscribe`).
- **[EVT-002] (MUST)** Frames use the SSE wire format `event: <type>\ndata: <json>\n\n`. A
  blank line terminates a frame and is the signal to dispatch the accumulated `data` as one
  JSON object. The response carries `Content-Type: text/event-stream`. **Golden:**
  `internal/host/mux/events.go:86` (`writeSSE`), `internal/daemonclient/transport.go:157`
  (`parseSSEStream`).
- **[EVT-003] (MUST)** The first frame is `event: connected` with
  `data: {"subscriber_id": "<id>"}`. It is a transport acknowledgement, **not** an agent
  event; a client MUST skip it (do not feed it to the reducer). **Golden:**
  `internal/host/mux/events.go:58` (emit), `internal/daemonclient/transport.go:169` (skip).
- **[EVT-004] (MUST)** There is **no `end` frame and no heartbeat frame.** Stream
  termination is signaled solely by the connection closing (the read returns EOF / null). A
  client MUST detect end-of-stream from the closed connection and MUST NOT wait for an
  application-level `end` event. **Why:** a client that waits for `end` never tears down on
  a silent close; a Kotlin consumer ships a dead `"end"` branch that the host never emits.
  See [INV-NO-END-001](08-invariants.md). **Golden:** `internal/host/mux/events.go:64`
  (loop returns on channel close — no terminal frame).
- **[EVT-005] (MUST)** On the **global** stream a client MUST filter events to the session(s)
  it cares about by `session_id`, **except** `session.create` and `session.delete`, which are
  session-list events and MUST be accepted regardless of the currently-open session. **Why:**
  the global stream multiplexes all sessions; an inbox needs create/delete even for sessions
  it isn't viewing. **Golden:** `internal/tui/sessionview.go:847` (filter + create/delete
  exception).

## Envelope

Every frame's `data` is an `Event` (`internal/agent/agent.go:71`):

```json
{
  "type": "part",
  "session_id": "01J...",
  "external_id": "sess_abc",
  "timestamp": "2026-06-21T12:00:00Z",
  "data": { "...": "type-specific payload" }
}
```

`type` selects the concrete `data` shape (the catalog below). `session_id` is the host
session ID. `external_id` is the backend-native ID (may be empty). `data` may be `null` for
payload-less events (e.g. `session.delete`); `session.create` carries the new `SessionInfo`,
though over SSE it may decode generically (refetch if unreadable — see [12](12-session-list.md)).

## Delivery guarantees (the dangerous part)

The stream is **best-effort, at-most-once, with no replay**. These four rules are the
single biggest source of cross-client divergence.

- **[EVT-010] (MUST)** Delivery is **at-most-once with no replay.** Events emitted before a
  client subscribes, or while it is disconnected, are **gone** — the server does not buffer
  history for a new or reconnecting subscriber. A client MUST therefore reconcile state from
  the transcript (`GET /sessions/{id}/messages`) after **every** (re)connection, not just the
  first. See [INV-RECONCILE-001](08-invariants.md). **Golden:**
  `internal/host/events.go` (no per-subscriber backlog), `clank-mobile/src/hooks/dispatch.ts:196`.
- **[EVT-011] (MUST)** Each subscriber has a bounded server-side buffer (256 events); when
  it fills, the server **silently drops** events for that subscriber rather than blocking. A
  client MUST drain its socket promptly (do no heavy work on the read path) and MUST rely on
  reconciliation ([EVT-010]) to recover anything dropped. **Why:** a slow consumer loses
  events with no error — including, potentially, a `permission` prompt. **Golden:**
  `internal/host/events.go:13` (`eventBufferSize = 256`), `:58` (drop-on-full select/default).
- **[EVT-012] (SHOULD)** Because there is no heartbeat ([EVT-004]), a client SHOULD enable
  transport-level keepalive or a liveness timeout to detect a half-open connection, then
  reconnect + reconcile. **Why:** with no read timeout a silently-dropped TCP connection can
  leave the UI frozen on stale state indefinitely. Cross-ref [NFR-REL-002](09-non-functional.md).
  **Golden:** `internal/daemonclient/transport.go:122` (no request timeout on the SSE client,
  by design — so liveness is the client's responsibility).
- **[EVT-013] (MUST)** Ordering is **per-connection FIFO only.** A client MUST apply events
  idempotently keyed by ID (message ID, part ID) so that re-applying a snapshot, or applying
  a refetch that overlaps the stream, is a no-op. There is **no** global ordering across a
  reconnect. **Why:** idempotent-by-ID application is what makes stream + refetch reconciliation
  safe. **Golden:** `internal/tui/sessionview.go:1638` (`upsertPartEntry` idempotent by part
  ID), `clank-mobile/src/lib/mergeMessages.ts:139` (`mergeMessageLists`).

## Event catalog

`internal/agent/agent.go:46`. Payload structs at `:199`–`:337`.

| `type` | Payload (`data`) | Meaning | Reducer rule |
|---|---|---|---|
| `status` | `{ old_status, new_status }` | Session work-state changed. | [STATE-STATUS-001](06-state-model.md) |
| `message` | `MessageData` | A user or assistant message (often a streaming "shell"). | [STATE-MSG-001](06-state-model.md) |
| `part` | `{ message_id?, part, is_delta? }` | A part appeared or advanced (text delta, tool progress). | [STATE-PART-001](06-state-model.md) |
| `permission` | `{ request_id, tool, description, tool_use_id? }` | Agent is **blocked** awaiting a tool decision. | [STATE-PERM-001](06-state-model.md) |
| `error` | `{ message }` | An error occurred on the session. | [STATE-ERR-001](06-state-model.md) |
| `title` | `{ title }` | AI-generated title set/changed. | [STATE-META-001](06-state-model.md) |
| `revert` | `{ message_id }` | Revert marker changed; empty = un-revert. | [STATE-REVERT-001](06-state-model.md) |
| `meta` | `{ session: SessionInfo }` | Session metadata changed; carries the **whole** new `SessionInfo`. The row-sync signal for the list. | [STATE-META-002](06-state-model.md) · [12](12-session-list.md) |
| `session.create` | `SessionInfo`* | A session was created (*may decode generically over SSE — refetch if unreadable). | [12](12-session-list.md) |
| `session.delete` | _none_ (keyed by `session_id`) | A session was deleted. | [12](12-session-list.md) |
| `reconnecting` | `{ attempt, delay, error, gave_up }` | **The backend** is reconnecting to its server. | [EVT-020] |
| `reconnected` | `{ attempts, url_changed }` | **The backend** reconnected. | [EVT-020] |
| `voice.transcript` / `voice.status` / `voice.tool_call` | (voice) | Voice agent; **out of scope** for chat clients. | — ignore |

### Field-level events (`status`/`title`) vs `meta`

- **[EVT-022] (MUST)** The host emits a field-level `status`/`title` event **first** (the
  transition, for an open session view) and then a consolidated `meta` event (the full row,
  with the bumped `updated_at`). An **open session view** uses the field-level events
  ([STATE-STATUS-001](06-state-model.md)); a **session list/sidebar** uses `meta`
  ([12](12-session-list.md), [LIST-003](12-session-list.md)). Driving list rows from
  `status`/`title` updates a field but misses the bumped `updated_at`, leaving the sort stale.
  **Golden:** `internal/tui/inbox_sse.go:23`.

### Backend reconnection events are not your socket

- **[EVT-020] (MUST)** `reconnecting` / `reconnected` describe the **host↔backend** link
  (e.g. an OpenCode server restarting on a new port), delivered *over* the client's SSE
  stream. They are **not** a signal about the client's own SSE connection. A client MUST NOT
  treat `reconnected` as "my stream came back," and MUST NOT rely on it to trigger
  reconciliation after its *own* transport drop — that case is detected by [EVT-004]/[EVT-012]
  and handled by [EVT-010]. A client MAY additionally reconcile on `reconnected`, since the
  backend may have changed state. **Why:** conflating the two means a real client-side
  transport blip (which emits no event, since the socket was down) never triggers a resync,
  leaving a stale transcript. See [INV-RECONNECT-SEMANTICS-001](08-invariants.md). **Golden:**
  `internal/agent/agent.go:297`–`:309` (payloads describe the backend), `clank-mobile/src/hooks/dispatch.ts:196`
  (current mobile behavior — reconciles on this event but does not cover its own transport reconnect).

### Voice events

- **[EVT-021] (MUST)** A chat client MUST ignore `voice.*` events. They are emitted only by
  the on-host voice agent and carry no chat-transcript state. **Golden:** `internal/agent/agent.go:63`.
