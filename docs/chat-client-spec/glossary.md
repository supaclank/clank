# Glossary

Terms as used in this spec.

- **Gateway** — the per-user reverse proxy (`pkg/gateway/`) a client connects to. Owns auth,
  host wake/lifecycle, and (soon) image buckets. Stateless w.r.t. chat. See [01](01-architecture.md).
- **Host / clank-host** — the server (`internal/host/`) that owns session state, the
  transcript, and the event stream. Reached only through the gateway.
- **Backend** — the coding agent behind the host: **Claude Code** or **OpenCode**
  (`internal/agent/`). A client never addresses it directly.
- **Sprite** — the user's host instance/sandbox; may be asleep and woken by the gateway on
  demand (source of cold-start latency).
- **Session** — one chat conversation, identified by host-assigned `id`. `external_id` is the
  backend's own id for it.
- **Transcript** — the ordered messages (each with ordered parts) of a session, read via
  `GET /sessions/{id}/messages`.
- **Message** — one turn entry (`role: user|assistant`) with `content` and/or `parts`.
- **Part** — a piece of a message: `text`, `thinking`, `tool_call`, or `tool_result`.
- **Tool call / tool result** — a tool invocation (`input`) and its outcome (`output`),
  carried as two updates to the **same part id**.
- **Permission prompt** — a `permission` event meaning the agent is **blocked** awaiting the
  user's approval to run a tool.
- **Permission mode** — Claude's posture: `default` / `acceptEdits` / `plan` /
  `bypassPermissions`. `""` on the wire means "no change".
- **Plan mode** — `permission_mode: plan`; the agent plans read-only and calls **ExitPlanMode**
  to ask to proceed.
- **ExitPlanMode / AskUserQuestion** — **interactive ("stop-and-wait") tools**: a tool-call
  part carries their UI (`input`), gated by a paired permission; the answer is the permission
  reply.
- **Revert** — Claude operation: roll files back + truncate the transcript at a message.
  Surfaced via `revert_message_id`.
- **Fork** — OpenCode operation: branch a new session from a message.
- **SSE** — Server-Sent Events; the realtime transport (`event: T\ndata: JSON\n\n`).
- **Event** — one SSE frame's typed payload; see the [catalog](04-event-protocol.md).
- **Delta vs snapshot** — `is_delta=true` is an incremental text chunk to **append**;
  `is_delta=false` is a full **snapshot** to replace.
- **Monotonic merge** — reconciliation that may only add/grow/advance, never remove/shrink/
  regress; how stream + refetch combine safely.
- **Reconcile** — refetch the transcript and monotonic-merge it into live state; run on every
  (re)connection because there is no event replay.
- **Optimistic echo** — rendering the user's message locally before the server confirms.
- **Assistant shell** — an empty assistant `message` (no id/content/parts) marking a turn
  boundary; rendered as nothing.
- **Follow** — the auto-scroll intent that keeps the viewport pinned to the latest content.
- **Abort** — interrupt the current turn; best-effort; observed via `status → idle`.
- **GitRef** — repo identity (`local_path`/`worktree_id`/`remote_url`/`worktree_branch`).
- **Reducer / projection** — the canonical state transition function and the pure view
  derived from state; the [core model](06-state-model.md).
