---
name: coderabbit
description: Use this skill when the user asks to triage, address, or respond to CodeRabbit (CR) review comments on a GitHub PR. Triggers include "look at coderabbit's comments", "address the CR review", "did coderabbit comment", "what does CR say", or any mention of CR feedback that needs action.
---

# CodeRabbit review triage

Goal: turn a noisy stream of CR comments into a small set of high-value
commits + PR replies. Bias toward shipping; treat CR's severity labels
as suggestions, not gospel.

## Workflow

1. **Fetch comments** via `gh api`, filter for CR, strip noise.
2. **Verify each finding against the code** — CR sometimes references
   stale lines, misreads Go semantics, or overshoots a fix. Always
   read the actual file before forming a verdict.
3. **Triage** into Do / Defer / Won't-do, ranked by impact and LoC.
4. **Confirm scope** with the user if unsure (you can always present
   the table and ask which tier to act on).
5. **Implement in small thematic commits** (correctness batch,
   lifecycle, tests, etc.). Don't lump unrelated fixes.
6. **Reply on PR** for every Defer and Won't-do, so CR's learning
   store records the rationale and stops re-flagging. Don't reply on
   addressed items — the diff speaks for itself.

## Fetching comments

PR review comments live under `pulls/<n>/comments` (not `issues/<n>/comments`).

```bash
gh api repos/<owner>/<repo>/pulls/<n>/comments --paginate
```

Filter and clean with Python — CR comments are dense with
`<details>` blocks (analysis chains, AI prompts, learnings) that
aren't actionable:

```python
import json, sys, re
data = json.load(sys.stdin)
for c in data:
    if 'coderabbit' not in c.get('user', {}).get('login', '').lower():
        continue
    body = c.get('body', '')
    body = re.sub(r'<details>\s*<summary>🧩 Analysis chain</summary>.*?</details>', '[stripped]', body, flags=re.DOTALL)
    body = re.sub(r'<details>\s*<summary>🤖 Prompt for AI Agents</summary>.*?</details>', '', body, flags=re.DOTALL)
    body = re.sub(r'<details>\s*<summary>🧠 Learnings used</summary>.*?</details>', '', body, flags=re.DOTALL)
    body = re.sub(r'<details>\s*<summary>✏️ Learnings added</summary>.*?</details>', '', body, flags=re.DOTALL)
    print(f'\n=== id={c["id"]} {c.get("path","?")}:{c.get("line","?")} ===')
    print(body[:3500])
```

Track which IDs you've already triaged across rounds; CR amends
without deleting, so a multi-round review accumulates noise.

## Triage rubric

For each comment, assess in order:

1. **Is the claim correct?** Read the actual code at the line. CR
   misreads happen — common patterns:
   - Loop-var capture flagged on body-scoped `:=` (fresh per iteration in every Go version).
   - Suggests refifying SHAs (e.g. `^refs/heads/<sha>`) when the variable is a SHA, not a branch name.
   - References wrong line numbers from an earlier diff.
2. **Real-world impact, not severity label.** Examples:
   - "🔴 Critical" wire-format bug that we already fixed in a prior commit → ack via diff, no work.
   - "🟡 Minor" silent-corruption defense (e.g. `refs/heads/` qualification) → real money, do it.
3. **LoC cost.** ≤ 5 lines and well-targeted = always worth doing
   even if impact is theoretical. > 30 lines and theoretical = defer
   with a trigger.
4. **Does the fix overshoot?** CR sometimes proposes broader
   refactors than needed. Apply the minimum safe fix; mention the
   broader idea in a reply or follow-up issue if useful.

Rank with a small markdown table for the user:

```
| Rank | ID | File:line | Issue | LoC | Real-world impact | Verdict |
```

## Verdict categories

- **Do**: high-value-per-LoC. Ship it in this PR.
- **Defer**: real concern, but cost-benefit doesn't justify *now*.
  Always pair with a concrete `revisit when…` trigger (file size
  threshold, second host added, CI shipped). Reply on the comment.
- **Won't do**: CR is wrong, or the suggested fix is worse than the
  current code. Reply with the explicit reasoning so CR's learning
  store records the disagreement.

## Implementation patterns

- **Group by theme, not by item.** "Correctness batch (5 small fixes
  in unrelated files)" reads better than 5 micro-commits.
- **Tests over comments.** If you'd write a paragraph explaining
  why, write a test that pins the behavior instead. Tests stay
  accurate; comments rot. (See AGENTS.md.)
- **Refactor for testability when CR points at concurrency.** Race
  fixes in handler-test patterns (channel for handoff) and lifecycle
  guards (Stop without Start, double-Start) are best pinned with
  fast unit tests so they don't regress silently.
- **Run `go test -race`** when CR flags shared state in tests. Even
  if `go test` passes, the race detector is the right oracle.
- **Verify after each commit** with `make test` (or
  `go test ./...`).

## Replying on the PR

Use `gh api` with the `replies` endpoint to thread replies under the
original comment:

```bash
gh api -X POST repos/<owner>/<repo>/pulls/<n>/comments/<id>/replies \
  -f body="$(cat <<'EOF'
**Deferred** — short reason here.

Tracked tradeoff:
- bullet 1
- bullet 2

**Revisit when** <concrete trigger>.
EOF
)"
```

Reply shapes:

- **Defer**: lead with `**Deferred**`, list the trade-off in 2-4
  bullets, end with `**Revisit when** <trigger>`. Keep under 200
  words.
- **Won't do**: lead with `**Won't do**` and the *correct* reading.
  Don't be defensive — explain the Go semantic / runtime constraint
  CR missed.
- **Addressed**: skip. CR resolves itself when it scans the new diff.
  Posting "addressed in <sha>" replies is just noise next to the
  strikethrough that's about to appear.

## Comment style during fixes

Match AGENTS.md: code is self-documenting; comments are reader
overhead. When fixing a CR finding:

- One-line goal-oriented `why` notes ("prevent race", "avoid leak")
  next to the change. No "previously we did X" history.
- Pin the contract with a test instead of a paragraph when possible.
- Multi-paragraph reserved for external-system constraints (third-
  party bug links, protocol quirks).

## Common CR misses to watch for

| Symptom | Reality |
|---|---|
| "Loop-captured variable in closure" on body-scoped `:=` | Fresh per iteration in every Go version. |
| "Apply same fix to <variable>" when variable is a SHA, not a ref | SHAs are unambiguous; no `refs/heads/` needed. |
| "Token leak via `%+v`" when both fields are empty on the error path | Real principle, theoretical on the path. Often worth fixing anyway as a free defense. |
| "Add `t.Parallel()`" on tests that aren't actually independent | Verify independence (no shared `t.Setenv`, no temp-dir collisions, no global state) before applying. |
| Suggests `&http.Transport{}` is fine | Bare zero-value drops `ProxyFromEnvironment`, `IdleConnTimeout`, `TLSHandshakeTimeout`. Always `http.DefaultTransport.(*http.Transport).Clone()` instead. |

## Things to never do

- **Don't blindly apply CR's diff.** Read the surrounding code; CR
  often optimizes the wrong axis.
- **Don't post replies on items you fixed.** Adds noise.
- **Don't argue tone** in won't-do replies. State the technical
  reasoning, link to the Go spec / language version, move on.
- **Don't accumulate CR comments across rounds without acking.**
  Reply on each defer/wont-do once so CR's learning store captures
  the rationale and stops re-flagging in future PRs.
