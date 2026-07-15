# 11 · Interactive Tools & Structured Replies

Some agent tool calls are **interactive**: they pause the turn to ask the user something
that needs a *structured* response, not just allow/deny. Today these are **AskUserQuestion**
and **ExitPlanMode** (Claude); OpenCode has an analogous `question` tool and emits plans as
plain assistant messages (no `ExitPlanMode`). This document also covers **inline comments**,
the client-side mechanism for replying to specific parts of a message.

> **Golden-reference note.** For the **question** flow the backend-tagged tool part below is
> the contract, and the **TUI** implements it (`internal/tui/sessionview_question.go`).
> For plan review and inline comments the **React Native client** remains the reference.

## The question tag (normalized path)

The backend normalizes provider question tools by tagging the question's own **tool-call
part** with the structured prompt: `part.question = { request_id, questions[] }`
(`agent.QuestionPrompt`; each question `{ text, header?, multi_select?, allow_custom?,
options[{label, description?}] }`). Claude `AskUserQuestion` (gated and bypass modes) and
OpenCode's `question` API produce the same tag. There is deliberately **no separate wire
event**: the part is the one object clients already reconcile across the live stream AND the
`Messages()` history refetch, so the prompt has a single source of truth, needs no
correlation or dedup, and survives reopening the session (and, on Claude, daemon restarts —
the tag is re-derived from the transcript).

- **[QST-001] (MUST — tag rendering & reply)** A client MUST render a tagged part as a
  structured prompt (options per question, multi-select where flagged, free-text where
  `allow_custom`) and reply via `POST /sessions/{id}/questions/{request_id}/reply` with
  `{ answers: [{selected?[], custom?}] }` (one per question, in order; an all-empty answer
  delegates that question) or `{ reject: true }` to dismiss. The **backend** owns translating
  answers to the provider transport (Claude: permission-deny message while parked, follow-up
  send after auto-run; OpenCode: its structured question API) — clients never format answer
  text themselves. **Golden:** `internal/agent/claude_permissions.go` (`RespondQuestion`),
  `internal/agent/opencode_questions.go`, `internal/tui/sessionview_question.go`.
  **Conformance:** `CONF-QUESTION-TAG`.

- **[QST-002] (MUST — answerability is positional)** A tagged part is **awaiting an answer**
  iff it is the conversation's **last content** (no later assistant text/tool part and no
  later user message; a paired `tool_result` on the same part id does not count) and its
  status is not `error`. Anything after it means the conversation moved on — render the tag
  read-only. This is [ITOOL-002] generalized: no resolved event exists; answering elsewhere
  is observed as ordinary transcript movement (the gated part completes; a bypass answer
  arrives as the user's follow-up message).
  **Golden:** `internal/tui/sessionview_question.go` (`activeQuestionPart`),
  `clank-mobile/src/lib/activeToolCall.ts` (same heuristic, client-parsed).

- **[QST-003] (MUST — permission suppression)** For a gated Claude question (default/plan
  mode) the backend still emits the `permission` event, with the **same `request_id`** as the
  tag, so pre-tag clients keep working; the tagged part is emitted first. A tag-aware client
  MUST suppress that permission prompt (match `request_id` or `tool_use_id`) — the card
  supersedes it, and the question reply resolves the parked prompt server-side.
  **Golden:** `internal/tui/sessionview_question.go` (`questionSuppressesPermission`); test
  `TestSessionView_QuestionSuppressesMatchingPermission`.

The rules below remain normative for **plan review**, for **legacy clients** that predate the
tag, and as the fallback when a question input fails to parse (no tag is stamped; the generic
permission flow applies).

## How interactive tools arrive (legacy part-sniffing path)

- **[ITOOL-001] (MUST)** An interactive tool has **no dedicated wire type**. It arrives as an
  ordinary `tool_call` part whose payload lives in `part.input`:
  - `AskUserQuestion` → `input.questions[]` (each `{ question, header?, multiSelect?, options[] }`).
  - `ExitPlanMode` → `input.plan` (a markdown string).
  A client identifies them by **matching the literal tool name** and parsing `input`; if the
  shape is unexpected, fall back to the generic tool-call card.
  **Why:** question parts now also carry the pre-parsed `part.question` tag ([QST-001]) —
  prefer that. This name-matching path remains the contract for **plans** and for clients
  that predate the tag.
  **Golden:** `clank-mobile/src/lib/askQuestion.ts:12` (`parseAskUserQuestion`),
  `clank-mobile/src/lib/planReview.ts:35` (`parsePlan`).

- **[ITOOL-002] (MUST)** A client MUST detect whether the prompt is **still awaiting an
  answer** versus the conversation having moved on. A **terminal** part `status`
  (`completed`/`error`) on the tool call means it was already answered (possibly on another
  client) — the interactive card MUST clear, not reappear. The same tool re-emits as
  `completed` when the turn finishes.
  **Why:** without this the card flickers back after answering or after a refetch.
  **Golden:** `clank-mobile/src/lib/activeToolCall.ts` (`findActiveToolCallPartId`),
  `clank-mobile/src/lib/askQuestion.ts:52`, `clank-mobile/src/lib/planReview.ts:86`. **Conformance:** `CONF-INTERACTIVE-ASK`,
  `CONF-INTERACTIVE-PLAN`.

- **[ITOOL-003] (MUST — gating)** When the session is in a prompting mode (`default`/`plan`),
  a `permission` prompt gates the interactive tool, correlated by `tool_use_id` == the part
  id (fall back to the lone prompt of that tool name when the id is absent). In bypass/build
  mode there is **no** permission — the tool auto-runs and the only channel is the answer
  itself. A client MUST handle both: present from the part, resolve the permission when one
  exists.
  **Golden:** `clank-mobile/src/lib/askQuestion.ts:66` (`findQuestionPermission`), `clank-mobile/src/lib/planReview.ts:98`
  (`findPlanPermission`).

## How the answer goes back (current mechanism — flagged)

- **[ITOOL-004] (MUST — current mechanism)** The structured answer is delivered as a
  **formatted `SendMessage`** (a normal follow-up whose text encodes the answer), **not** via
  a dedicated structured-response API:
  - `AskUserQuestion` → `formatAnswers` builds `Answers to your questions:` with one
    `**header**: answer` line per question; a fully-unselected set collapses to a single
    "use your best judgment" line (delegation is first-class).
  - `ExitPlanMode` → the decision rides the **permission reply**, not a `SendMessage`:
    **Approve** = reply `allow` (the backend then exits plan mode and implements; optional
    review notes may ride a follow-up `SendMessage`); **Revise** = reply `deny` with
    `formatPlanReview` as the deny-reason, so the agent re-plans in plan mode. Inline comments
    (below) fold into either. (`AskUserQuestion`, by contrast, cannot accommodate allow/deny, so its
    answer is the formatted `SendMessage` above, with the gating permission resolved separately.)
  When a gating permission is also pending ([ITOOL-003]), it must be resolved as well.
  **Why:** clank's permission-reply bridge can only allow/deny + a deny-reason string; it
  cannot return a structured tool result, so the answer rides a follow-up message instead.
  **This is the part whose robustness is uncertain** — see [Open questions](#open-design-questions-non-normative).
  **Golden:** `clank-mobile/src/lib/askQuestion.ts:84` (`formatAnswers`),
  `clank-mobile/src/lib/planReview.ts:78` (`formatPlanApproval`), `:69` (`formatPlanReview`),
  `:30` (`APPROVAL_MESSAGE`). **Conformance:** `CONF-INTERACTIVE-ASK`, `CONF-INTERACTIVE-PLAN`.

- **[ITOOL-005] (MUST)** Plan-mode posture follows [INV-PERMMODE-EXITPLAN-001](08-invariants.md):
  Approve = **reply `allow`** to the parked `ExitPlanMode` permission; the **backend** then
  exits plan mode and resets its tracked mode (`internal/agent/claude_permissions.go:97`). The
  client does **not** send a `permission_mode` to switch to build — it simply keeps sending
  `permission_mode:""` on ordinary follow-ups ([INV-PERMMODE-001](08-invariants.md)) until the
  user explicitly changes the mode. Revise = **reply `deny`** with the revision notes as the
  deny-reason. **Golden:** `clank-mobile/src/lib/planReview.ts` (`formatPlanApproval`/`formatPlanReview`),
  `internal/agent/claude_permissions.go:97` (backend mode reset on approval).

## Inline comments (client-side structured reply)

A reply mechanism with **no wire support** — it is a compose-time text convention. The user
selects a block of an assistant message's rendered markdown and attaches a comment; the
client sends a GitHub-review-style message: each commented block quoted, followed by the
comment.

- **[ICOMMENT-001] (SHOULD)** A client SHOULD let the user comment on specific blocks of an
  assistant message and send those as quoted-comment text via `SendMessage` (optionally
  alongside free-text, which leads). This is the **cross-backend** structured-reply path: it
  works on any message, so it covers OpenCode plans (plain messages, no `ExitPlanMode`) and
  general per-section feedback alike. **Why:** structured, located feedback without a
  protocol; the same splitter backs both the plan reviewer and general message comments.
  **Golden:** `clank-mobile/src/lib/chatReview.ts` (`formatChatReply`),
  `clank-mobile/src/lib/markdownBlocks.ts` (`splitMarkdownIntoBlocks`, `buildQuotedComments`, `quoteLines`),
  `clank-mobile/src/lib/planReview.ts:48` (shared with plan reviewer). **Conformance:** `CONF-INLINE-COMMENT`.

## Cross-backend mapping

| Concept | Claude | OpenCode | Client surface |
|---|---|---|---|
| Ask the user a question | `AskUserQuestion` tool call | `question` tool call | one interactive question card |
| Propose a plan for approval | `ExitPlanMode` tool call (plan in `input`) | plain assistant message | plan card (Claude) / inline comments on the message (OpenCode) |
| Reply to a specific section | inline comments | inline comments | inline comments ([ICOMMENT-001]) |

- **[ITOOL-006] (SHOULD)** A client SHOULD present `AskUserQuestion` and OpenCode's
  `question` through the **same** question UI (the `part.question` tag delivers both
  pre-normalized, [QST-001]), and OpenCode plans through inline comments on the message.
  **Why:** one consistent interaction across backends.

## TUI status

The TUI implements the question-tag path ([QST-001..003]) and renders ExitPlanMode
permission prompts with the plan text plus approve / request-changes (deny with notes) / deny
choices (`internal/tui/sessionview_question.go`, `sessionview.go` prompt card). It still has
**no inline comments** ([ICOMMENT-001]) — for those the RN client remains the reference.

## Open design questions (non-normative)

These capture intended direction; **none is normative yet.**

1. **Plan as a first-class tag.** Whether to tag `ExitPlanMode` parts the way questions are
   tagged (vs. clients parsing `input.plan` / OpenCode plain messages) so plan-review UI is
   backend-uniform.
2. **Gated mid-park reopen — resolved.** The host now serves the parked permission queue
   from backend memory ([OP-007](05-operations.md)) and `Messages()` synthesizes the parked
   tool part (same part id and question tag as the live emit), so a reopen mid-park
   restores the card and the reply routes through the parked prompt. Remaining slice of
   [INV-PENDING-PERM-GAP-001](08-invariants.md): the park is in-memory, so a daemon
   restart still loses it.
3. **OpenCode cross-restart tags.** OpenCode request-id ↔ call-id correlation lives in
   backend memory; hydrating it from `question.list` on Open would keep transcript reloads
   tagged across daemon restarts (Claude already is — its tag derives from the transcript).

When these are decided, add normative rules + conformance scenarios here and supersede the
flagged mechanisms above per the [maintenance loop](README.md#maintenance).
