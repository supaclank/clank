# Clank Chat-Client Specification

**Spec version:** 0.5.0 · **Last updated:** 2026-07-23 · **Status:** Draft

This is the normative contract for any client that drives a clank **chat session** — the
TUI, the React Native app, the native Android/iOS overlays, a future Swift or web client.
Its purpose is narrow and load-bearing: **a client built to this spec produces the same
user-observable behavior as every other client, with no re-discovery of bugs already
fixed elsewhere.**

The Go TUI (`internal/tui/`) is the **golden reference**. Every rule here is derived from
the TUI and the `clank-host` server it talks to, and cites the source (`file:line`) it
was derived from. When a bug is fixed in the golden client, the corresponding rule and its
conformance scenario are updated here in the same change (see [Maintenance](#maintenance)).

**Golden-reference is per behavior, not blanket.** The TUI is golden for the core
event/state machine. For a few surfaces it is *not*: **interactive-tool rendering**
(AskUserQuestion / ExitPlanMode / inline comments, [11](11-interactive-tools.md)) is led by
the React Native client, and the TUI is deficient there. The spec records the **best-known
behavior across clients**; each exception is flagged at its rule with the actual reference.

## Who this is for

- Anyone implementing or maintaining a clank chat client on any platform.
- Anyone changing the `clank-host` chat API or event protocol (this spec is the
  blast-radius map: a wire change that breaks a rule here breaks every client).

## How to read it

The spec is layered. Read top-to-bottom for a full understanding; jump to a layer when
you have a specific question.

| # | Document | What it answers |
|---|----------|-----------------|
| — | [README.md](README.md) | Conventions, rule IDs, maintenance (this file) |
| 01 | [01-architecture.md](01-architecture.md) | Gateway vs. host vs. backend; who owns what; the trust boundary |
| 02 | [02-connection-and-auth.md](02-connection-and-auth.md) | Base URL, OAuth2+PKCE, bearer, 401-refresh, sprite-wake latency, error envelope |
| 03 | [03-data-model.md](03-data-model.md) | Canonical wire types, enums, JSON shapes, forward-compatibility |
| 04 | [04-event-protocol.md](04-event-protocol.md) | SSE transport, the event catalog + payloads, delivery guarantees |
| 05 | [05-operations.md](05-operations.md) | Every endpoint: method/path/request/response/status/idempotency |
| 06 | [06-state-model.md](06-state-model.md) | **The core.** Canonical client state, the reducer, derived view-projections |
| 07 | [07-lifecycle-flows.md](07-lifecycle-flows.md) | Sequence flows: create, follow-up, permission, plan mode, revert, abort, reconnect |
| 08 | [08-invariants.md](08-invariants.md) | **The regression firewall.** Every hard-won invariant as a normative rule |
| 09 | [09-non-functional.md](09-non-functional.md) | Latency, consistency, resilience, security, concurrency, battery, observability |
| 10 | [10-conformance.md](10-conformance.md) | Scenario matrix, trace-fixture format, how each client proves conformance |
| 11 | [11-interactive-tools.md](11-interactive-tools.md) | AskUserQuestion / ExitPlanMode / inline comments; structured replies; backend-abstraction open questions |
| 12 | [12-session-list.md](12-session-list.md) | Sidebar / inbox live sync from `meta` + create/delete |
| — | [glossary.md](glossary.md) | Terms used throughout |
| — | [implementations/](implementations/) | Per-language **Implementation Checklists** — the "how" for each platform |

## The split: "what" vs. "how"

This core spec (`01`–`10`) is **language-agnostic** and defines *what* a client must do —
the observable behavior. It deliberately contains **no** platform-specific code or notes,
so it never splinters into three competing specs.

The *how* lives in [implementations/](implementations/): one **Implementation Checklist**
per platform that maps every rule and conformance scenario to the concrete mechanism that
platform uses to satisfy it, plus a status. The checklist is both the porting guide and
the living record of where each client stands.

## Conventions

### Requirement levels (RFC 2119 / RFC 8174)

The key words **MUST**, **MUST NOT**, **REQUIRED**, **SHALL**, **SHALL NOT**, **SHOULD**,
**SHOULD NOT**, **RECOMMENDED**, **MAY**, and **OPTIONAL** are to be interpreted as
described in RFC 2119 and RFC 8174 — and only when in **bold**.

### Rule format

Every normative statement is a numbered rule with a stable ID:

> **[EVT-012] (MUST)** A client MUST treat `is_delta=true` as *append to the existing
> part text* and `is_delta=false` as *replace the part text with the snapshot*.
> **Why:** a snapshot after dropped deltas is the protocol's only self-heal; treating it
> as an append duplicates text, treating a delta as a replace loses text.
> **Golden:** `internal/tui/sessionview.go:1665` (`upsertPartEntry`).
> **Conformance:** `CONF-STREAM-DELTA`.

Fields: **ID**, **requirement level**, the statement, **Why** (the concrete bug it
prevents — never omit this; it is what makes the rule deletable when the bug is gone),
**Golden** (the source the rule is derived from), **Conformance** (the scenario that
checks it).

### Rule-ID prefixes

`ARCH` architecture · `CONN` connection/auth · `DATA` data model · `EVT` event protocol ·
`OP` operations · `STATE` reducer state · `VIEW` derived view-projection · `FLOW` lifecycle flows ·
`INV` invariant / known-bug · `NFR` non-functional · `CONF` conformance scenario ·
`ITOOL` interactive tool · `ICOMMENT` inline comment · `LIST` session list.

IDs are **append-only and never reused**. A retired rule is struck through and kept, so
external references (commit messages, checklists) never dangle.

### Golden references

`file:line` pointers default to the **clank** repo; cross-repo paths are prefixed with the
repo name (e.g. `clank-mobile/src/…`). Line numbers drift; the **symbol name** in each
reference is the durable anchor. If a reference doesn't resolve, find the named symbol and
update the line — and the rule, if behavior changed.

## Maintenance

This spec is only useful while it is true. The loop, on every behavior change:

1. **Fix the bug / change the behavior** in the golden client (`internal/tui/`) and/or the
   host.
2. **Add or amend the rule** here: statement, **Why**, **Golden** ref, **Conformance** id.
   New behavior → new rule (next ID). Removed behavior → strike the rule through, don't
   delete.
3. **Add or amend the conformance scenario** in [10-conformance.md](10-conformance.md) and
   its trace fixture.
4. **Bump the spec version** (top of this file) — patch for clarifications, minor for new
   rules, major for a breaking wire change.
5. **Update each affected Implementation Checklist** status in [implementations/](implementations/).

A change that touches client-observable behavior but skips steps 2–3 is incomplete.

## Out of scope

The deprecated **sync API**; visual/layout/theming/accessibility design (each client owns
its presentation; the spec constrains observable *state*, not pixels); whether a client
hand-writes or generates its transport; backend internals (Claude Code / OpenCode) except
where they leak into client-visible behavior.
