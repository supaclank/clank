# ACP backend generalization — decision record

**Decision (2026-07-29): keep six hand-written first-class adapter
profiles. Do not build a config-driven ACP adapter.** Evaluated after
gemini, hermes, and pi landed first-class (PRs #198, #199, #200),
against this gate:

> A generalization is only worth it if all six agents run through it
> with **no per-agent Go conditionals** leaking back in, AND it nets
> out to **less code and less complexity** than six hand-written
> profiles.

Both halves fail. This doc records what blocked it, so the question
doesn't get re-litigated from scratch on backend seven.

## What is already generic

The supervisor, conn, reducer, and `ACPBackendManager` contain zero
backend conditionals. All variance flows through one seam,
`acp.AdapterProfile`, built by a per-backend constructor in
`internal/host`. The six profiles total ~200 lines, the constructors
~230 — mostly comments and closures. That is the consolidation; a YAML
layer would sit on top of it, not replace it.

## What blocked a config-driven adapter

A config schema comfortably expresses argv, scope, and a static env
map. Four of the six backends need more than that, and the "more" is
per-agent Go *behavior*, not data:

| Backend | Go-coded variance a schema can't carry |
|---|---|
| opencode | guidance materialized into the worktree's git dir + injected via inline-config env, keyed on file existence; verified-surface version floor (`opencode --version` gate) |
| claude-code | `IS_SANDBOX=1` when running as root; guidance via `session/new` `_meta.systemPrompt.append`; Anthropic credential env resolved from the AuthManager at spawn |
| codex | pinned npm pair provisioning; OpenAI credential env + restart-on-credential-write callback; device-auth login ceremony sharing the pinned codex binary (`LoginArgv`) |
| pi | pinned npm pair + a materialized `pi-under-bun.sh` shim for `PI_ACP_PI_COMMAND`, whose path must be derivable *before* provisioning runs (the supervisor fingerprints env pre-spawn) |
| hermes | version floor with a different probe (`hermes acp --version`) and floor semantics |
| gemini | the only near-trivial one: pinned package + `--acp` (and even it leans on the capability-gated discovery fix) |

Expressing those in config means either config-referenced Go hooks (a
per-agent dispatch table — the same conditionals, indirected) or a
templating/plugin DSL that costs more than the ~430 lines it replaces.
Loader + schema + validation + argv/env templating + tests would exceed
that before the first quirk is handled. Per-agent quirks dominate;
that's the STOP condition.

## Relationship to `clank.yaml`

The sibling "custom dev-server commands via clank.yaml" task had not
landed a schema as of this decision (one forward-looking comment in
`internal/host/preview/detect.go`). If a `clank.yaml` `agents:` surface
ships later, it is an **additive product feature** — user-defined
agents at runtime — not a consolidation of the six built-ins. It should
**construct an `AdapterProfile`** (argv + scope + static env is exactly
the trivial subset a user-defined agent needs) and leave the built-ins
as Go. The profile struct is the extension point; that schema decision
is joint work with the clank.yaml task owner.

## Revisit triggers

- Several new backends in a row land with hermes/gemini-shaped profiles
  (pure argv + scope, no hooks) — a tiny "static profile from config"
  helper could then serve *that subset* without touching the four
  hook-carrying backends.
- ACP standardizes agent-owned provisioning/auth ceremonies to the
  point where the hook closures above disappear upstream.
