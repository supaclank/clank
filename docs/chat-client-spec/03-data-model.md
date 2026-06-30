# 03 · Data Model

The canonical wire types, language-agnostic. JSON field names are the contract; Go struct
tags in `internal/agent/agent.go` are the source. A client maps these to its own types but
MUST preserve the field names and semantics below.

## Forward compatibility (read this first)

- **[DATA-001] (MUST)** A client MUST ignore unknown JSON fields and unknown enum values
  rather than erroring. An unknown event `type`, `status`, `part.type`, or extra object
  field MUST be tolerated (skipped or passed through), not fatal. **Why:** the wire is
  additive; the server adds fields/variants without a major version, and a strict decoder
  bricks every old client on the next feature. **Golden:** `internal/agent/agent.go:186`
  (`UnmarshalJSON` default branch falls back to generic decode for unknown event types).

## `SessionInfo`

The session metadata snapshot, returned by create/get/list/search and embedded in the
`meta` event. Source: `internal/agent/agent.go:407`.

| Field | JSON | Type | Notes |
|---|---|---|---|
| ID | `id` | string | Host-assigned session ID (ULID). The identity a client uses everywhere. |
| ExternalID | `external_id` | string | Backend-native session ID. May be empty until the backend assigns it (async for Claude). |
| Backend | `backend` | enum | `opencode` \| `claude-code`. |
| Status | `status` | enum | Runtime status; see [SessionStatus](#enums). |
| Visibility | `visibility` | enum | `""` \| `done` \| `archived`; user-set. |
| FollowUp | `follow_up` | bool | User-set flag. |
| Hostname | `hostname` | string | Canonical identity: host. |
| GitRef | `git_ref` | object | Canonical identity: repo. See [GitRef](#gitref). |
| Prompt | `prompt` | string | The initial prompt. |
| Title | `title` | string | AI-generated; arrives later via the `title` event. |
| TicketID | `ticket_id` | string | Optional backlog link. |
| Agent | `agent` | string | Current OpenCode agent name. |
| Draft | `draft` | string | Unsent follow-up text the user was composing. |
| RevertMessageID | `revert_message_id` | string | When set, messages from this ID onward are reverted (hidden). See [DATA-012]. |
| ServerURL | `server_url` | string | **Runtime-only, not persisted.** |
| IsRemote | `is_remote` | bool | **Runtime-only**, stamped by gateway routing. |
| CreatedAt / UpdatedAt / LastReadAt | `created_at` / `updated_at` / `last_read_at` | timestamp | RFC 3339. |

- **[DATA-010] (MUST)** A client MUST NOT persist `server_url` or `is_remote` as session
  identity; they are runtime decorations that change between responses. **Why:** caching
  them as identity produces stale routing. **Golden:** `internal/agent/agent.go:422`.
- **[DATA-011] (SHOULD)** A client that sorts an inbox by recency SHOULD sort on
  `updated_at`, and rely on the fact that **only agent-driven activity bumps `updated_at`** —
  user-driven metadata mutations (mark-read, draft, visibility, follow-up) deliberately do
  not. **Why:** otherwise marking a session read or saving a draft would reorder the inbox
  under the user. **Golden:** `internal/host/sessions_meta.go`.
- **[DATA-012] (MUST)** `revert_message_id`, when non-empty, means *the message with that ID
  and everything after it is reverted and MUST NOT be displayed*. Empty means no revert. See
  the history filter in [STATE](06-state-model.md) and [INV-REVERT-001](08-invariants.md).
  **Golden:** `internal/agent/agent.go:421`, `internal/tui/sessionview.go:1535`.

## `MessageData`

One transcript message. Returned in `GET /sessions/{id}/messages` and carried by the
`message` event. Source: `internal/agent/agent.go:214`.

| Field | JSON | Type | Notes |
|---|---|---|---|
| ID | `id` | string | Backend-assigned. **May be empty** — e.g. Claude user-echo messages and the streaming assistant "shell". |
| Role | `role` | string | `user` \| `assistant`. |
| Content | `content` | string | Flattened text; may be empty when content is carried in `parts`. |
| Parts | `parts` | Part[] | Ordered; see [Part](#part). |
| ModelID | `model_id` | string | Assistant only; the model that produced the message. |
| ProviderID | `provider_id` | string | Assistant only. |

- **[DATA-020] (MUST)** A client MUST handle `id == ""` on a message. User messages sent to
  the Claude backend echo back without an ID; the streaming assistant shell has none either.
  See [INV-MSGID-001](08-invariants.md). **Golden:** `internal/agent/agent.go:215`,
  `clank-mobile/src/hooks/dispatch.ts:51` (shell drop), `:73` (id-less user echo).

## `Part`

A piece of a message — a text block, a thinking block, a tool call, or a tool result.
Source: `internal/agent/agent.go:224`.

| Field | JSON | Type | Notes |
|---|---|---|---|
| ID | `id` | string | Stable per part. For Claude text/thinking parts, encoded as `{assistantMsgID}-{blockIdx}`; tool parts use the raw `tool_use` id. |
| Type | `type` | enum | `text` \| `tool_call` \| `tool_result` \| `thinking`. |
| Text | `text` | string | For text/thinking. |
| Tool | `tool` | string | Tool name for tool_call/tool_result. |
| Status | `status` | enum | `pending` \| `running` \| `completed` \| `error`. Tool lifecycle. |
| Input | `input` | object | Tool-call arguments (e.g. `command`, `file_path`). |
| Output | `output` | string | Tool result text. |

- **[DATA-021] (MUST)** The part `status` lifecycle is **monotonic**: `pending` → `running`
  → `completed`/`error`. A client MUST NOT let a late or re-delivered lower-ranked status
  regress a part that already reached a terminal state. A client that introduces a
  **client-only terminal status** — e.g. marking a tool the user *canceled* via abort, since
  the wire has no `canceled` value — MUST rank that status **terminal** as well; otherwise a
  post-abort transcript refetch (which still carries the interrupted tool as `running`)
  monotonically "advances" it back to `running` and the spinner resumes. See
  [INV-ABORT-SETTLE-TOOLS-001](08-invariants.md). **Why:** out-of-order or re-fetched snapshots
  otherwise flip a finished tool back to "running". **Golden:**
  `clank-mobile/src/lib/mergeMessages.ts:25` (`STATUS_RANK`, `preferStatus`).
- **[DATA-022] (MUST)** A tool is **two parts that share one part ID** (the `tool_use` id):
  the **call** (`type=tool_call`, carrying `tool` + `input`) and the **result**
  (`type=tool_result`, carrying `output`, with `tool` **empty**). A client MUST merge them by
  id — never replace — so neither the arguments nor the tool name is lost. **In the committed
  transcript** (`GET /sessions/{id}/messages`) and the `message` events, the two parts land in
  **separate messages**: the call in the assistant message, the result in a **following
  `role=user` message** whose only payload is `tool_result` part(s) — one per parallel tool
  call. So the merge is **cross-message**, and the empty user-role carrier MUST be folded away
  rather than rendered as a user turn or a second, nameless "tool" card. See
  [INV-TOOL-MERGE-001](08-invariants.md) and [INV-TOOL-RESULT-CARRIER-001](08-invariants.md).
  **Verified in source** (`coalesceSessionMessages` coalesces an assistant turn, but a user
  record — where tool results live — breaks the run) **and** against the live gateway
  (`assistant{tool_call toolu_X}` then `user{tool_result toolu_X}`). **Golden:**
  `internal/agent/claude.go:1167` (`coalesceSessionMessages`), `:1254` (`sessionBlockToPart`:
  tool_result `ID=ToolUseID`, no `tool`); `internal/tui/sessionview.go:1698` (`upsertPartEntry`,
  merges by part id);
  `clank-mobile/modules/preview-launcher/android/…/session/ChatTranscript.kt` (`foldToolResults`
  — cross-message fold + carrier drop); `clank-mobile/src/lib/mergeMessages.ts:46`.

## Enums

- **[DATA-030]** The closed-today, append-only-tomorrow enums. A client MUST tolerate
  unknown values per [DATA-001]. **Golden:** `internal/agent/agent.go`.

| Enum | Values | Source |
|---|---|---|
| `SessionStatus` | `starting`, `busy`, `idle`, `error`, `dead` | `agent.go:26` |
| `SessionVisibility` | `""` (visible), `done`, `archived`, `all` (query-only pseudo-value) | `agent.go:39` |
| `BackendType` | `opencode`, `claude-code` | `agent.go:18` |
| `ClaudePermissionMode` | `default`, `acceptEdits`, `plan`, `bypassPermissions`; `""` = "no change" | `agent.go:526` |
| `PartType` | `text`, `tool_call`, `tool_result`, `thinking` | `agent.go:237` |
| `PartStatus` | `pending`, `running`, `completed`, `error` | `agent.go:247` |

Note `status` (the session-level work state) is distinct from `visibility` (user-set inbox
state): a session can be `idle` yet `done`.

## Request bodies

### `StartRequest` (`POST /sessions`)

Source: `internal/agent/agent.go:370`. Required: `backend`, `git_ref`, `prompt`.

| Field | JSON | Notes |
|---|---|---|
| Backend | `backend` | Required. |
| GitRef | `git_ref` | Required repo identity. |
| Prompt | `prompt` | Required initial message. |
| Hostname | `hostname` | Target host; empty = `local`. |
| Agent | `agent` | OpenCode agent name. |
| Model | `model` | `ModelOverride`; omit for default. |
| PermissionMode | `permission_mode` | Initial Claude mode; `""` = backend default. |
| TicketID / SessionID | `ticket_id` / `session_id` | Optional. |

### `SendMessageOpts` (`POST /sessions/{id}/message`)

Source: `internal/agent/agent.go:565`.

| Field | JSON | Notes |
|---|---|---|
| Text | `text` | The message. |
| Agent | `agent` | OpenCode agent; empty = session default. |
| Model | `model` | Per-message override; omit = default. |
| PermissionMode | `permission_mode` | Claude mode change; **`""` = no change**. See [INV-PERMMODE-001](08-invariants.md). |

- **[DATA-040] (MUST)** A client MUST send `permission_mode: ""` (or omit it) when the user
  has not changed the mode on this send. A client MUST NOT send a default value (e.g.
  `"default"`) to mean "unchanged". **Why:** any non-empty value *re-asserts* the mode on the
  backend; sending a default silently flips a `plan`/`acceptEdits` session. **Golden:**
  `internal/agent/agent.go:571`, `internal/tui/sessionview.go:2162` (sends only the selected
  mode).

### `ModelOverride` / `GitRef`

`ModelOverride`: `{ "model_id": "...", "provider_id": "..." }` (`agent.go:516`).

`GitRef`: repo identity carrying `local_path` and/or `worktree_id`, optional
`display_name`, optional `worktree_branch`. A client treats it as an opaque object it
echoes from list/get responses, except at create time where it sets the target repo.
Source: `internal/gitref.go`.

## Image attachments (forward-looking — not yet shipped)

- **[DATA-050] (reserved)** Image support will add attachments uploaded to gateway-owned
  buckets via presigned URLs minted at the gateway; the message will reference uploaded
  blobs. This section is a placeholder so clients reserve the shape; no rule is normative
  until the wire lands. **Golden:** TBD (gateway object store).
