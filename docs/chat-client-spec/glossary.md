# Glossary

Terms as used in this spec.

- **Gateway** — the per-user reverse proxy (`pkg/gateway/`) a client connects to. Owns auth,
  host wake/lifecycle, and (soon) image buckets. Stateless w.r.t. chat. See [01](01-architecture.md).
- **Host / clank-host** — the server (`internal/host/`) that owns session state, the
  transcript, and the event stream. Reached only through the gateway.
- **Backend** — the coding agent behind the host: **Claude Code**, **OpenCode**, or
  **Codex** (`internal/agent/`). A client never addresses it directly.
- **ACP** — the Agent Client Protocol (agentclientprotocol.com). Codex is served through an
  ACP adapter today (`internal/agent/acp/`); OpenCode and Claude Code migrate in later
  milestones. Client-invisible by design — the wire contract in this spec is unchanged —
  except where rules note ACP-served behavior (agent-owned modes, typed `unsupported`).
- **Session mode** — an **agent-owned** mode id (`available_modes` on runtime session info):
  permission presets for Claude/Codex, agents for OpenCode. Clients render the advertised
  list as-is; the legacy four Claude mode ids remain valid because the Claude adapter uses
  the same strings.
- **Sprite** — the user's host instance/sandbox; may be asleep and woken by the gateway on
  demand (source of cold-start latency).
- **Session** — one chat conversation, identified by host-assigned `id`. `external_id` is the
  backend's own id for it.
- **Transcript** — the ordered messages (each with ordered parts) of a session, read via
  `GET /sessions/{id}/messages`.
- **Message** — one turn entry (`role: user|assistant`) with `content` and/or `parts`.
- **Part** — a piece of a message: `text`, `thinking`, `tool_call`, or `tool_result`.
- **Tool call / tool result** — a tool invocation (`tool_call`, carrying `tool` + `input`) and
  its outcome (`tool_result`, carrying `output`, with `tool` **empty**), carried as two parts
  that share the **same part id** but arrive in **separate messages** (the result in a following
  `role=user` carrier). A client merges them into one card — see
  [INV-TOOL-RESULT-CARRIER-001](08-invariants.md).
- **Tool-result carrier** — the `role=user` message that carries only a `tool_result` part
  (empty content, empty `tool`); folded into the matching `tool_call` card and never rendered as
  a user turn. See [INV-TOOL-RESULT-CARRIER-001](08-invariants.md).
- **Permission prompt** — a `permission` event meaning the agent is **blocked** awaiting the
  user's approval to run a tool.
- **Permission mode** — Claude's posture: `default` / `acceptEdits` / `plan` /
  `bypassPermissions`. `""` on the wire means "no change".
- **Plan mode** — the Plan preset (`config: {"mode": "plan"}` on claude); the agent plans read-only and calls **ExitPlanMode**
  to ask to proceed.
- **ExitPlanMode / AskUserQuestion** — **interactive ("stop-and-wait") tools**: a tool-call
  part carries their UI, gated by a paired permission in prompting modes. Question parts
  carry the backend-normalized `part.question` tag and are answered on the questions
  endpoint; the plan decision rides the permission reply.
- **Revert** — roll files back + truncate the transcript at a message; supported on **both**
  backends (Claude: durable rollback, requires a message id, since #68; OpenCode: a toggleable
  session marker whose empty id clears it). Surfaced via `revert_message_id`. See
  [OP-005](05-operations.md).
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
- **Abort** — interrupt the current turn; best-effort; observed via `status → idle` (which may
  arrive **delayed**). On settle a client settles still-running tools terminally and suppresses
  any "done" affordance until the next send. See [INV-ABORT-SETTLE-TOOLS-001](08-invariants.md),
  [INV-ABORT-DONE-001](08-invariants.md).
- **GitRef** — repo identity (`local_path`/`worktree_id`/`display_name`/`worktree_branch`),
  plus `subdir` — the working directory relative to the repo root (`""` = root); identity
  stays root-level, the session runs in the subdir.
- **Reducer / projection** — the canonical state transition function and the pure view
  derived from state; the [core model](06-state-model.md).
