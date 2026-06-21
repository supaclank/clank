# 11 · Interactive Tools & Structured Replies

Some agent tool calls are **interactive**: they pause the turn to ask the user something
that needs a *structured* response, not just allow/deny. Today these are **AskUserQuestion**
and **ExitPlanMode** (Claude); OpenCode has an analogous `question` tool and emits plans as
plain assistant messages (no `ExitPlanMode`). This document also covers **inline comments**,
the client-side mechanism for replying to specific parts of a message.

> **Golden-reference note.** For interactive tools the **React Native client is the
> reference**, not the TUI. The TUI handles the underlying *permission* but renders **no**
> interactive UI for these tools (it shows a generic y/n prompt). New clients should follow
> the RN behavior described here, with the caveats flagged as known limitations.

## How interactive tools arrive (current reality)

- **[ITOOL-001] (MUST)** An interactive tool has **no dedicated wire type**. It arrives as an
  ordinary `tool_call` part whose payload lives in `part.input`:
  - `AskUserQuestion` → `input.questions[]` (each `{ question, header?, multiSelect?, options[] }`).
  - `ExitPlanMode` → `input.plan` (a markdown string).
  A client identifies them by **matching the literal tool name** and parsing `input`; if the
  shape is unexpected, fall back to the generic tool-call card.
  **Why:** there is no semantic event for "interactive prompt" yet, so the tool name + input
  shape is the only signal. This name-matching is a **known hack** — see [Open questions](#open-design-questions-non-normative).
  **Golden:** `clank-mobile/src/lib/askQuestion.ts:12` (`parseAskUserQuestion`),
  `clank-mobile/src/lib/planReview.ts:35` (`parsePlan`).

- **[ITOOL-002] (MUST)** A client MUST detect whether the prompt is **still awaiting an
  answer** versus the conversation having moved on. A **terminal** part `status`
  (`completed`/`error`) on the tool call means it was already answered (possibly on another
  client) — the interactive card MUST clear, not reappear. The same tool re-emits as
  `completed` when the turn finishes.
  **Why:** without this the card flickers back after answering or after a refetch.
  **Golden:** `clank-mobile/src/lib/activeToolCall.ts` (`findActiveToolCallPartId`),
  `askQuestion.ts:52`, `planReview.ts:86`. **Conformance:** `CONF-INTERACTIVE-ASK`,
  `CONF-INTERACTIVE-PLAN`.

- **[ITOOL-003] (MUST — gating)** When the session is in a prompting mode (`default`/`plan`),
  a `permission` prompt gates the interactive tool, correlated by `tool_use_id` == the part
  id (fall back to the lone prompt of that tool name when the id is absent). In bypass/build
  mode there is **no** permission — the tool auto-runs and the only channel is the answer
  itself. A client MUST handle both: present from the part, resolve the permission when one
  exists.
  **Golden:** `askQuestion.ts:66` (`findQuestionPermission`), `planReview.ts:98`
  (`findPlanPermission`).

## How the answer goes back (current mechanism — flagged)

- **[ITOOL-004] (MUST — current mechanism)** The structured answer is delivered as a
  **formatted `SendMessage`** (a normal follow-up whose text encodes the answer), **not** via
  a dedicated structured-response API:
  - `AskUserQuestion` → `formatAnswers` builds `Answers to your questions:` with one
    `**header**: answer` line per question; a fully-unselected set collapses to a single
    "use your best judgment" line (delegation is first-class).
  - `ExitPlanMode` → **Approve** sends `formatPlanApproval` **and switches to build mode** so
    the agent implements; **Revise** sends `formatPlanReview` **in plan mode** so the agent
    re-plans. Inline comments (below) fold into either.
  When a gating permission is also pending ([ITOOL-003]), it must be resolved as well.
  **Why:** clank's permission-reply bridge can only allow/deny + a deny-reason string; it
  cannot return a structured tool result, so the answer rides a follow-up message instead.
  **This is the part whose robustness is uncertain** — see [Open questions](#open-design-questions-non-normative).
  **Golden:** `clank-mobile/src/lib/askQuestion.ts:84` (`formatAnswers`),
  `clank-mobile/src/lib/planReview.ts:78` (`formatPlanApproval`), `:69` (`formatPlanReview`),
  `:30` (`APPROVAL_MESSAGE`). **Conformance:** `CONF-INTERACTIVE-ASK`, `CONF-INTERACTIVE-PLAN`.

- **[ITOOL-005] (MUST)** Plan-mode posture follows [INV-PERMMODE-001](08-invariants.md):
  Approve transitions to build mode by sending `permission_mode` accordingly on that message;
  ordinary follow-ups keep `permission_mode:""`. **Golden:** `planReview.ts:78`/`:69`.

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
  `src/lib/markdownBlocks.ts` (`splitMarkdownIntoBlocks`, `buildQuotedComments`, `quoteLines`),
  `src/lib/planReview.ts:48` (shared with plan reviewer). **Conformance:** `CONF-INLINE-COMMENT`.

## Cross-backend mapping

| Concept | Claude | OpenCode | Client surface |
|---|---|---|---|
| Ask the user a question | `AskUserQuestion` tool call | `question` tool call | one interactive question card |
| Propose a plan for approval | `ExitPlanMode` tool call (plan in `input`) | plain assistant message | plan card (Claude) / inline comments on the message (OpenCode) |
| Reply to a specific section | inline comments | inline comments | inline comments ([ICOMMENT-001]) |

- **[ITOOL-006] (SHOULD)** A client SHOULD present `AskUserQuestion` and OpenCode's
  `question` through the **same** question UI, and OpenCode plans through inline comments on
  the message. **Why:** one consistent interaction across backends.

## TUI gap (recorded, not golden)

The TUI resolves the *permission* for these tools but does **not** render the question/plan
UI or support inline comments. It is therefore **not** the reference for this document. A new
client MUST NOT take the TUI's lack of interactive UI as the spec. **Golden (the gap):**
`internal/tui/sessionview.go` handles `permission` generically with no `AskUserQuestion`/
`ExitPlanMode` special-casing.

## Open design questions (non-normative)

These capture intended direction; **none is normative yet.** Until they land, the mechanisms
above ([ITOOL-001]/[ITOOL-004]) are the contract.

1. **Backend abstraction events.** Clients hardcoding tool names ([ITOOL-001]) is fragile.
   The backend could emit a semantic *interactive-prompt* event (typed question/plan payload)
   so clients stop sniffing tool names, and **unify** OpenCode `question` with Claude
   `AskUserQuestion` behind one event.
2. **Structured-answer API.** A way to submit a structured response **together with** the
   permission reply (rather than a separate `SendMessage`, [ITOOL-004]) would make answering
   robust and atomic, and let the answer return as the tool's actual result.
3. **Plan as a first-class object.** Whether to give plans a typed event (vs. `ExitPlanMode`
   input on Claude / plain message on OpenCode) so plan-review UI is backend-uniform.

When these are decided, add normative rules + conformance scenarios here and supersede the
flagged mechanisms above per the [maintenance loop](README.md#maintenance).
