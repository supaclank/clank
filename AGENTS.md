# Engineering guidelines

## Coding structure
1. Prefer multiple files instead of long files; consider per-method files for a struct, especially endpoints.
2. NEVER use "fallbacks" for missing parameters. Makes it harder to debug and reason about the code's true happy path. Prefer fast failures and clear API's.
3. No magic strings. Prefer constant global variables, and consider whether a type alias (enum) is necessary for preventing mistakes via better type-safety.

## Testing & debugging

0. Don't guess - add logs or TDD tests to verify your hypothesis before implementing.
1. Add regression tests for every bug fix.
2. If you think you found a bug, write a test to verify it before fixing it. This way we can use tests as documentation for why a feature needs to exist. This helps make it easier to delete things in the future.
3. Every functional change should include or update tests.
4. NEVER mock dependencies. Prefer integration tests, or refactor code to be unit-testable. I can't stress this enough.
5. Use `t.Parallel()` where safe.

## Comments And Documentation

Code should be self-documenting; comments are reader overhead. Optimize for short comments (1-2 lines) over multi-line paragraphs (>3 lines).

- Multi-line paragraphs are reserved for external-system constraints with no other home (third-party bug links, protocol quirks). If a rationale fits in code or a test, put it there instead.
- Avoid line-number or specific-implementation references in other files — they decay. Reference packages, types, or contracts instead.
- Comments on exported funcs/types describe **what** the code does. Skip rationale unless it's genuinely tricky.
- Inline `why` notes are fine when the code's *goal* is non-obvious — keep them goal-oriented and brief: "prevent race", "avoid leak", "Clone DefaultTransport so Proxy/Idle/TLS defaults aren't dropped".
- Don't restate the obvious. A comment that paraphrases the next line is overhead with no payoff.
- Don't narrate history. "Previously we…", "the old version…", "this used to…" belong in `git log`, not in code. Regression tests should document this instead.
- Keep prose aligned with current implementation. Mark deliberate forward-looking notes with TODO/Future.

## Feedback Expectations

Agents should proactively provide improvement feedback when they see opportunities.

- Include a short "Opportunities" section in final summaries when relevant.
- Prioritize concrete, high-impact suggestions: security hardening, correctness gaps, reliability risks, missing tests, and DX pain points.
- Include file references for suggested follow-up work.
- If no notable opportunities are found, state that explicitly.

## Feedback

- If you spot meaningful security, reliability, or DX improvements, include a brief `Opportunities` section with file references.