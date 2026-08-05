# ACP adapter frame fixtures

Sanitized frames captured from **real ACP adapters** via the migration
spike driver, against the pinned versions in each filename. Personal
paths, identifiers, command inventories, timestamps, and usage values
are normalized. They are the parity instrument for the ACP backend: the
reducer replay test (`internal/agent/acp/fixtures_test.go`) feeds every
`session/update` line through the production reducer and asserts the
committed transcript — so an adapter behavior we depend on can't
silently change shape without a reviewed fixture update.

## Format

JSONL; each line is `{"kind": <string>, "payload": <json>}`.

- `kind: "session/update"` — an ACP `SessionNotification` (the payload
  the reducer consumes). Other kinds (`initialize_response`,
  `new_session_response`, `prompt_response`, `request_permission`,
  `list_response`) are conn-level context kept for reference.

## Files

- `opencode-1.17.18-turn.jsonl` — full live turn on `opencode acp`
  (thought chunks, message chunks, `available_commands_update`, a
  normalized `usage_update`).
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

Capture more with the spike driver pattern: spawn the adapter, log every
`SessionUpdate` notification verbatim. Redact anything account-specific
and normalize personal metadata before committing.
