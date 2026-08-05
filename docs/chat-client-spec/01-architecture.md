# 01 · Architecture

A client never talks to one thing. It talks to a **gateway**, which proxies to a
**host** (`clank-host`), which drives a **backend** (Claude Code or OpenCode). Almost all
chat semantics are the host's; the gateway is a thin, mostly-stateless edge. A client
implementer must know the split because the failure modes differ at each layer.

```
┌────────┐   HTTPS + Bearer    ┌──────────────┐   proxied + host token   ┌────────────┐   stdio / control   ┌──────────────┐
│ client │ ──────────────────▶ │ clank-gateway│ ───────────────────────▶ │ clank-host │ ──────────────────▶ │ backend      │
│ (this  │ ◀────────────────── │ (edge: auth, │ ◀─────────────────────── │ (sessions, │ ◀────────────────── │ Claude Code  │
│  spec) │   JSON / SSE        │  wake, proxy)│         JSON / SSE       │  events)   │      events         │ / OpenCode   │
└────────┘                     └──────────────┘                          └────────────┘                     └──────────────┘
```

## The gateway (`pkg/gateway/`)

A per-user reverse proxy and the only endpoint a client addresses. It is **stateless with
respect to chat**: it does not store sessions, messages, or events. Its jobs:

- **[ARCH-001] (MUST)** Authenticate the client. The client authenticates to the *gateway*
  with a bearer token obtained via OAuth2 + PKCE (see [02](02-connection-and-auth.md)); it
  never holds the host's token. The gateway verifies the bearer and injects the host's own
  token on the proxied request. **Why:** clients must never be given host credentials, and
  the auth scheme is pluggable per deployment. **Golden:** `pkg/gateway/gateway.go`,
  `pkg/auth/`.
- **Sprite/host lifecycle.** The gateway wakes or provisions the user's host on demand.
  A request to a sleeping host blocks while it wakes — this is the source of first-request
  latency (see [CONN-030](02-connection-and-auth.md)).
- **Transparent proxy.** Every chat path (`/sessions*`, `/events`, …) is proxied verbatim
  to the host. From the client's perspective the gateway *is* the host API, plus auth and
  wake latency. The endpoint catalog in [05](05-operations.md) is the host's, reached
  through the gateway.
- **Object store / image presigning (forward-looking).** Image attachments will use
  gateway-owned buckets with presigned upload URLs minted at the gateway; the host is
  minimally involved. Not yet shipped — see [03](03-data-model.md) for the placeholder.

> Reference gateway deployments: `docker/docker-compose.yml` (local dev stack) and the
> multi-tenant `supaclank` control plane. They differ in provisioning and auth backend but
> expose the **same** chat contract.

## The host (`clank-host`, `internal/host/`)

Owns everything a chat client cares about:

- **Session state** — create, list, get, delete, search; metadata (title, visibility,
  draft, follow-up, revert marker). Persisted across restarts.
- **The transcript** — ordered messages and parts, readable via `GET /sessions/{id}/messages`.
- **The event stream** — a single SSE fan-out (`GET /events`) plus a per-session variant
  (`GET /sessions/{id}/events`). See [04](04-event-protocol.md).
- **Permission brokering** — bridges the backend's synchronous tool-permission callback to
  the asynchronous `permission` event + reply endpoint.

- **[ARCH-002] (MUST)** A client MUST treat the host as the **single source of truth** for
  session state and transcript. Optimistic local state (see [STATE](06-state-model.md)) is
  permitted, but it MUST converge to what the host reports. **Why:** divergence between
  rendered state and host state is the root class of chat bugs. **Golden:**
  `internal/host/mux/sessions.go`, `internal/tui/sessionview.go` (`handleSessionMessages`).

## The backend (`internal/agent/`, Claude Code / OpenCode / Codex)

Behind the host; a client never addresses it directly. **Since 0.5.0 clank is migrating
backends onto the Agent Client Protocol** (`internal/agent/acp/`: the host spawns and
supervises ACP adapter processes — Codex today, OpenCode and Claude Code in later
milestones). This is deliberately client-invisible: the wire contract in this spec is the
compatibility gate for each migration step. Runtime note: ACP adapters distributed as npm
packages run as plain JS under the pinned **bun**; provisioning is lazy per host
(`internal/agent/acptools`). It mostly does not leak — but the following behaviors do, and
the spec calls each out where relevant:

1. **Session modes are agent-owned** (0.5.0): the agent advertises `{id, name, description}`
   modes on runtime session info and clients render the list as-is — permission presets for
   Claude (`default`/`acceptEdits`/`plan`/`bypassPermissions`, plus `auto`/`dontAsk` via
   ACP) and Codex (`read-only`/`agent`/`agent-full-access`); agents for OpenCode. See
   [03](03-data-model.md), [07](07-lifecycle-flows.md).
2. **Message-ID timing differs.** OpenCode assigns IDs synchronously; Claude assigns the
   session/external ID asynchronously and streams text/thinking parts whose `message_id` is
   *empty* (the owning message is encoded in the part ID). See [EVT](04-event-protocol.md),
   [INV](08-invariants.md).
3. **Revert vs. fork.** Claude supports `revert` (file rollback + transcript truncation);
   OpenCode supports `fork`. Calling the unsupported one errors. See [05](05-operations.md).
4. **Interactive ("stop-and-wait") tools** — `ExitPlanMode`, `AskUserQuestion` (and
   OpenCode's `question`) — pause the turn and need a *structured* reply. Question tool parts
   carry a backend-normalized `part.question` tag (answered on the questions endpoint); plans
   surface through a `tool_call` part + a gating permission, identified by tool name (a known
   hack). See [11](11-interactive-tools.md).

- **[ARCH-003] (SHOULD)** A client SHOULD be backend-agnostic: branch on observed wire
  shape (e.g. empty `message_id`), not on `backend == "claude-code"`. **Why:** backend
  detection scatters special-cases and rots when a third backend appears. **Conformance:**
  `CONF-MSGID-OWNER`.

## What a client connects to, concretely

- One **base URL** (the gateway). See [CONN-001](02-connection-and-auth.md).
- One **bearer token**, refreshed on a schedule and on 401. See [CONN-010](02-connection-and-auth.md).
- One **SSE stream** for realtime events. See [EVT-001](04-event-protocol.md).
- A set of **JSON request/response endpoints** for everything else. See [05](05-operations.md).

Everything else in this spec is about reacting to that stream and those endpoints
correctly.
