# ACP adapter frame fixtures

Verbatim frames captured from **real ACP adapters** (2026-07-22/23) via
the migration spike driver, against the pinned versions in each
filename. They are the parity instrument for the ACP backend: the
reducer replay test (`internal/agent/acp/fixtures_test.go`) feeds every
`session/update` line through the production reducer and asserts the
committed transcript — so an adapter behavior we depend on can't
silently change shape without a reviewed fixture update.

## Format

JSONL; each line is `{"t": <rfc3339>, "kind": <string>, "payload": <json>}`.

- `kind: "session/update"` — an ACP `SessionNotification` (the payload
  the reducer consumes). Other kinds (`initialize_response`,
  `new_session_response`, `prompt_response`, `request_permission`,
  `list_response`) are conn-level context kept for reference.

## Files

- `opencode-1.17.18-turn.jsonl` — full live turn on `opencode acp`
  (thought chunks, message chunks, `available_commands_update`, a
  stable `usage_update` with cost + context size).
- `opencode-1.17.18-load-replay.jsonl` — the same session replayed via
  `session/load` in a fresh process: whole-message chunks, no deltas
  (the "pre-merged transcript" shape DATA-022 allows).
- `claude-agent-acp-0.61.0-turn.jsonl` — live claude turn incl. the
  late-update class: a `session_info_update` (title) arriving AFTER the
  prompt response resolved, plus `_meta`-only update variants
  (`_claude/rateLimit`) the reducer must drop without wedging.
- `claude-agent-acp-0.61.0-load-replay.jsonl` — claude `session/load`
  replay of that session.
- `claude-agent-acp-0.61.0-tool-turn.jsonl` — a turn with a real Bash
  tool call: `tool_call` + several `tool_call_update`s carrying
  `_meta.claudeCode.toolName`, kind/rawInput/rawOutput.
- `hermes-agent-0.19.0-turn.jsonl` — full live turn on `hermes acp`
  (2026-07-29, local OpenAI-compatible model): text deltas, a
  hermes-shaped `usage_update` variant the reducer must drop
  harmlessly, and the late-title class (`session_info_update` after
  the prompt resolved).
- `hermes-agent-0.19.0-load-replay.jsonl` — the same session replayed
  via `session/load` in a fresh process: pre-merged whole-message
  chunks (DATA-022 shape).
- `hermes-agent-0.19.0-tool-turn.jsonl` — an edit-tool turn: one
  `tool_call` (diff content, title as the only tool-name channel), a
  real `session/request_permission` exchange (conn-level frames, kept
  for reference), and NO `tool_call_update` — the call stays
  result-less by adapter design.

Capture more with the spike driver pattern: spawn the adapter, log every
`SessionUpdate` notification verbatim. Redact anything account-specific
before committing (these captures contain none).

TODO: `gemini` (pinned `@google/gemini-cli`) has no turn fixtures yet —
capturing one needs Google credentials (`GEMINI_API_KEY` or a cached
`gemini` OAuth login), which the capture machine lacked. The handshake
and the unauthenticated session/new error surface are pinned by the
gated integration test instead
(`TestIntegration_GeminiACP_SpawnInitializeNewSession`).
