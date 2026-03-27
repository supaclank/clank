# Engineering guidelines

## Coding structure
1. Prefer multiple files instead of long files; consider per-method files for a struct, especially endpoints.
2. Add comments where helpful, especially for non-obvious "why" decisions. Do not add redundant comments.
3. NEVER use "fallbacks" for missing parameters. Makes it harder to debug and reason about the code's true happy path. Prefer fast failures.

## Testing & debugging

0. Don't guess - add logs or TDD tests to verify your hypothesis before implementing.
1. Add regression tests for every bug fix.
2. If you think you found a bug, write a test to verify it before fixing it. This way we can use tests as documentation for why a feature needs to exist. This helps make it easier to delete things in the future.
3. Every functional change should include or update tests.
4. NEVER mock dependencies. Prefer integration tests, or refactor code to be unit-testable. I can't stress this enough.
5. Use `t.Parallel()` where safe.

## Comments And Documentation

- Add brief comments ahead of tricky blocks
- Keep documentation aligned with current implementation, not planned behavior unless explicitly marked.

## Feedback Expectations

Agents should proactively provide improvement feedback when they see opportunities.

- Include a short "Opportunities" section in final summaries when relevant.
- Prioritize concrete, high-impact suggestions: security hardening, correctness gaps, reliability risks, missing tests, and DX pain points.
- Include file references for suggested follow-up work.
- If no notable opportunities are found, state that explicitly.

## Feedback

- If you spot meaningful security, reliability, or DX improvements, include a brief `Opportunities` section with file references.