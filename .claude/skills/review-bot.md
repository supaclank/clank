---
name: review-bot
description: Use this skill when handling AI review-bot comments on a GitHub PR (CodeRabbit/CR, Cubic, Greptile, and similar). Triggers include "address the CR/cubic/greptile review", "look at coderabbit/cubic/greptile comments", "did the review bot comment", "triage PR bot feedback", or any mention of bot review feedback that needs action. Also used by an automated routine that fires once on PR open and runs this triage unattended.
---

# Review-bot triage

Goal: turn the noisy stream of AI review-bot comments (CodeRabbit, Cubic,
Greptile, future bots) into a small set of high-value commits + PR
replies. Bias toward shipping; treat severity labels as suggestions, not
gospel.

Two modes share the same rubric:

- **Interactive** — a user invokes the skill mid-PR. Present the triage
  table in chat, confirm scope, ship.
- **Autonomous** — a routine fires this skill **once on PR open**. Wait
  for the review bots to finish their pass, then post the triage table
  as a top-level PR comment, implement Do items, push to the PR branch,
  reply on Defer/Won't-do. No human in the loop. See "Autonomous mode"
  below.

## Supported bots

Recognized three ways: GitHub login (for fetching comments), check-run
app slug (for watching progress on a commit's CI status), and @-mention
handle (for nudging a bot that hasn't picked the PR up — see "Nudge the
bots when none respond").

| Bot | Login pattern | Check-run app slug | Mention handle |
|---|---|---|---|
| CodeRabbit | `coderabbit` | `coderabbitai` | `@coderabbitai` |
| Cubic | `cubic-dev`, `cubic[bot]` | `cubic-dev-ai` | `@cubic-dev-ai` |
| Greptile | `greptile` | `greptile-apps` | `@greptileai` |

**Greptile merge gate.** Greptile writes a `<h3>Confidence Score: N/5</h3>`
into the **PR description body** (not a comment) — read it from `pulls/<n>`
(`.body`). **Never merge — or mark the PR mergeable — while that score is
below 5/5.** Surface it in the triage table and the activity log; below
5/5 the verdict on the PR as a whole is "not mergeable yet", regardless
of how the individual findings triage.

If unsure of a bot's exact app slug, inspect a recent PR's checks:

```bash
gh api "repos/<owner>/<repo>/commits/<sha>/check-runs" \
  --jq '.check_runs[] | "\(.app.slug)\t\(.name)\t\(.status)"'
```

Adding a new bot:

1. Add its login substring to `BOT_LOGIN_PATTERNS` (fetch script below),
   its app slug to `BOT_CHECK_SLUGS` (watch loop below), and its
   @-mention handle to `BOT_MENTIONS` (nudge step below).
2. Add the summary text of any collapsible `<details>` noise blocks the
   bot emits to `NOISE_SUMMARIES`.
3. Nothing else changes — the rubric, verdicts, replies, and TODO marker
   are bot-agnostic by design.

## Shared workflow

1. **Fetch comments** via `gh api`, filter to known review bots, strip
   noise.
2. **Verify each finding against the code** — bots reference stale
   lines, misread Go semantics, or overshoot a fix. Always read the
   actual file before forming a verdict.
3. **Triage** into Do / Defer / Won't-do / Skip, ranked by impact and LoC.
4. **(Interactive) confirm scope** with the user. Watch for redirects
   that demote items to Skip — "already rotated", "single-user concern,
   already fixed" — and update the table before coding.
   **(Autonomous) choose conservatively**: when in doubt, prefer Defer
   over Do. A TODO + reply is cheap; a wrong fix isn't.
5. **Implement in small thematic commits** (correctness batch, lifecycle,
   tests). Don't lump unrelated fixes.
6. **Anchor every Defer with a `TODO(ai-review)` comment** at the call
   site (see "TODO markers" below). Won't-do and Skip get no marker.
7. **Reply on the PR** for every Defer and Won't-do, so the bot's
   learning store records the rationale and stops re-flagging. Skip
   items need no per-comment reply. Don't reply on addressed items — the
   diff speaks for itself.

## Autonomous mode

The routine fires this skill on `pull_request` events — `opened`,
`ready_for_review`, and `synchronize`. Re-runs on `synchronize` are
**intentional, not a bug**: every time the agent (or a human) pushes
new code, the bots re-review it, and the skill should triage whatever
new findings come back.

The loop terminator is the "Should I run?" gate: once the bots have
nothing new to say since our last triage, the skill exits silently and
the conversation converges.

Per fire, post a fresh **activity log** comment at the start and
live-update it (see "Activity log") as you move through these steps, so a
human watching the PR sees the unattended routine progress in real time:

1. **Watch for bot completion** on the current HEAD — let any
   in-progress review pass finish (grace window + hard cap). If no bot
   check ever appears, **nudge the bots** by @-mention (see "Nudge the
   bots when none respond").
2. **Should I run?** — exit silently when the bots produced nothing
   new since the prior triage comment (the activity log still records
   the no-op).
3. **Triage** the new findings per the rubric, **fix**, **push**.
4. The push fires another `synchronize`, restarting the loop. The
   bots eventually stop producing new findings, step 2 exits, and the
   loop converges.

### Activity log

So a human watching the PR can follow the unattended routine **in real
time**, each run posts its own activity comment at the start and
**live-updates it in place** as the run moves through the phases below.
One comment per fire — a new trigger is a new comment, so distinct runs
stay legible in the timeline. Don't search for or reuse a prior run's
comment.

Post once at the start, hold the returned id, and PATCH that same comment
as each phase *begins* — before doing the phase's work — so the trailing
line always reflects what's happening **now** (present tense,
in-progress), never a recap written after the fact:

```bash
# Start of run: create the comment, capture its id.
act_id=$(gh api -X POST "repos/$PR_OWNER/$PR_REPO/issues/$PR_NUM/comments" \
  -f body="$(cat activity.md)" --jq .id)

# Each phase boundary: rewrite activity.md with the new line, then PATCH.
gh api -X PATCH "repos/$PR_OWNER/$PR_REPO/issues/comments/$act_id" \
  -f body="$(cat activity.md)"
```

A mid-run snapshot — completed phases marked ✅, the current one ⏳:

```markdown
### Automated review-bot activity

_HEAD `a1b2c3d` · started 13:58 · updated 14:02 UTC_

- 13:58 ✅ Watched bots: greptile done (confidence 4/5); coderabbit, cubic no check — nudged.
- 14:01 ✅ Triaged 6 findings (4 do, 1 defer, 1 won't-do).
- 14:02 ⏳ Implementing correctness batch (3 fixes) + 1 defer; pushing…
```

Write the ⏳ line *before* the phase runs, then flip it to ✅ (or ⚠️) and
append the next ⏳ line when it finishes. Phases worth a line: watch start
("⏳ Waiting on bot reviews…"), bot completion or absence (note Greptile's
confidence score), any nudge sent, the triage decision — including a
no-op ("nothing new since last triage"), implement + push, replies
posted, and a terminal ✅ done / ⚠️ timed-out line. One sentence each; the
triage table and the diff carry the detail.

Comment edits don't notify subscribers, so after the first comment a run
goes quiet — refresh the PR to watch it move. Every fire gets exactly one
such comment, including a no-op that exits at "Should I run?".

### Should I run?

Find the most recent prior triage comment by the bot identity in
`issues/<n>/comments`, recognized by its leading header
`### Automated review-bot triage`.

After the watch loop completes, exit silently when **any** of:

- The bots posted no review comments at all on this PR (nothing to
  triage; don't post an empty table).
- A prior triage comment exists AND no bot review comment has
  `created_at > prior_triage.created_at` (bots had nothing new on this
  round).

Otherwise proceed.

This is the only state carried between runs — no hidden markers, no
JSON. The triage-comment header acts as the implicit cursor.

### Race conditions

Two `synchronize` events firing close together (two pushes in quick
succession, or a push and a bot comment back-to-back) can spawn
parallel runs. Defenses are layered but soft:

- Both runs read the same comments and produce the same triage.
- `git push` is the serializer — only one push wins. The loser sees
  non-fast-forward, fetches, re-enters "Should I run?", and exits
  because the work is done.

No hard lock. No `--force` push.

### Watch for bot completion

**Primary signal: the PR's check-runs on the current HEAD.** CodeRabbit,
Cubic, and Greptile publish their progress as check runs, the same place
CI status lives. Status transitions `queued` → `in_progress` →
`completed` are unambiguous and earlier than any comment, so we don't
have to guess from quiescence.

Polling logic:

- **What we poll**: `gh api repos/<o>/<r>/commits/<head_sha>/check-runs`,
  filtered to runs whose `app.slug` is in `BOT_CHECK_SLUGS`.
- **Interval**: 30s.
- **Grace period**: 3 minutes for any bot check to appear. A *single*
  bot whose check never shows up is assumed not running on this PR (repo
  doesn't have it enabled, draft PR, etc.) and is dropped from the wait
  set. If *no* bot check appears at all, nudge them once by @-mention and
  extend the window (see "Nudge the bots when none respond").
- **Done condition**: every bot check in the wait set has
  `status == "completed"`.
- **Hard cap**: 25 minutes total. If reached, proceed with whatever has
  been posted.

Sketch:

```bash
PR_OWNER=…; PR_REPO=…; PR_NUM=…
HEAD_SHA=$(gh api "repos/$PR_OWNER/$PR_REPO/pulls/$PR_NUM" --jq .head.sha)
BOT_CHECK_SLUGS=(coderabbitai cubic-dev-ai greptile-apps)

start=$(date +%s); grace=$((start + 180)); cap=$((start + 25*60))

while :; do
  now=$(date +%s)
  runs=$(gh api "repos/$PR_OWNER/$PR_REPO/commits/$HEAD_SHA/check-runs" --paginate)

  # Lines: <slug>\t<status>  — one per bot check.
  bot_rows=$(echo "$runs" | jq -r --argjson slugs "$(printf '%s\n' "${BOT_CHECK_SLUGS[@]}" | jq -R . | jq -s .)" '
    .check_runs[]
    | select(.app.slug as $s | $slugs | index($s))
    | "\(.app.slug)\t\(.status)"
  ')

  if [ -z "$bot_rows" ]; then
    # No bot check present. Wait through grace, then nudge once.
    if [ "$now" -lt "$grace" ]; then sleep 30; continue; fi
    if review_warranted && ! nudged_this_head; then
      nudge_bots             # @-mention all bots; see "Nudge the bots…"
      grace=$((now + 180))   # fresh window for them to publish checks
      sleep 30; continue
    fi
    break  # nudged already / no review needed; "Should I run?" handles it
  fi

  pending=$(echo "$bot_rows" | awk -F'\t' '$2 != "completed"' | wc -l)
  if [ "$pending" -eq 0 ]; then break; fi

  if [ "$now" -ge "$cap" ]; then break; fi
  sleep 30
done
```

**Fallback for bots without check runs.** If a new bot only posts
comments and never publishes a check run, fall back to the older
heuristic: watch `pulls/<n>/reviews` for a submitted review from that
bot's login, plus a 60s quiescence tail. Add the bot to
`BOT_LOGIN_PATTERNS` so its comments still get triaged.

### Nudge the bots when none respond

Bots don't always pick a PR up on their own — a draft was marked ready
late, a webhook was missed, or they're mid-rate-limit. The signal is the
grace window expiring with **zero** bot checks present (not one bot
missing — *none*).

When that happens, decide whether a review is actually warranted: is
there substantive, unreviewed code on the current HEAD? If yes, post a
single comment that @-mentions every bot so they pick it up. Each bot
answers in its own comment — starting the review, declining, or
reporting a rate limit:

```bash
BOT_MENTIONS=(@coderabbitai @greptileai @cubic-dev-ai)
gh api -X POST "repos/$PR_OWNER/$PR_REPO/issues/$PR_NUM/comments" -f body="$(cat <<EOF
<!-- review-bot:nudge $HEAD_SHA -->
${BOT_MENTIONS[*]} — no automated review detected on \`$HEAD_SHA\`. Please review when you can.
EOF
)"
```

Then **reset the grace window and re-enter the watch loop** so their
freshly-published checks get awaited; read their replies on the next
pass and let the normal flow triage whatever review lands.

Constraints:

- **Only when *no* bot check is present.** If even one bot is in
  progress, wait for it and let the absent ones drop per the grace rule
  — a partial set means the PR was picked up.
- **At most once per HEAD.** The `<!-- review-bot:nudge <sha> -->` marker
  makes a prior nudge greppable in `issues/<n>/comments`; if it's already
  there for this SHA, just keep waiting (and past the hard cap, fall
  through to "Should I run?", which no-ops when nothing landed).
- **Log it** in the activity comment ("coderabbit, cubic no check —
  nudged").

### Post the triage

Fetch comments (see "Fetching comments"), triage per the rubric, then
post a single top-level PR comment with the table:

```markdown
### Automated review-bot triage

| Rank | Source | ID | File:line | Issue | LoC | Real-world impact | Verdict |
|---|---|---|---|---|---|---|---|
| 1 | coderabbit | 12345 | foo.go:42 | … | 3 | … | Do |
| 2 | cubic | 678 | bar.go:88 | … | 12 | … | Defer |
| 3 | greptile | 901 | baz.go:5 | … | 2 | … | Do |
…

**Greptile confidence:** 4/5 — **not mergeable until 5/5.**
**Plan:** correctness batch (3 fixes), 1 defer with TODO, 1 won't-do.
```

Post via the issue comments endpoint:

```bash
gh api -X POST "repos/$PR_OWNER/$PR_REPO/issues/$PR_NUM/comments" \
  -f body="$(cat triage.md)"
```

### Implement and push

- Use a recognizable bot Git identity for the commits so reviewers can
  see which work came from this routine. Set `user.name` /
  `user.email` for the worktree before committing if the routine
  harness hasn't already.
- Run `make test` (or the project's verify recipe) before each commit.
- Commit Do items in thematic batches (correctness, lifecycle, tests),
  not one per finding.
- Anchor each Defer with a `TODO(ai-review)` marker at the call site
  (see "TODO markers").
- Push with plain `git push`.
- Reply on every Defer and Won't-do per "Replying on the PR".

### Inputs the routine must provide

- `owner/repo` and PR number, OR a checked-out branch whose upstream
  resolves to a known PR via `gh pr view --json number,headRepository`.
- A clean worktree at the PR's HEAD (the routine harness handles
  checkout/pull).
- `gh` authenticated as the bot identity, with push access to the PR
  branch.

If any are missing, exit with an error rather than guessing — no
fallbacks (see AGENTS.md).

## Fetching comments

A few sources carry bot output; usually you only need the first for
triage, with the others covering review summaries, walkthroughs, and
Greptile's confidence score:

```bash
# Line-anchored review comments (the bulk of CR/Cubic/Greptile findings).
gh api repos/<owner>/<repo>/pulls/<n>/comments --paginate

# Review-level submissions (summary bodies, "request changes" etc.).
gh api repos/<owner>/<repo>/pulls/<n>/reviews --paginate

# PR-level issue comments (walkthroughs, our triage comment, etc.).
gh api repos/<owner>/<repo>/issues/<n>/comments --paginate

# PR description body — Greptile writes `<h3>Confidence Score: N/5</h3>` here.
# Parse it for the merge gate (see "Greptile merge gate").
gh api repos/<owner>/<repo>/pulls/<n> --jq .body
```

Filter and clean with Python — bot comments are dense with `<details>`
blocks (analysis chains, AI prompts, learnings) that aren't actionable:

```python
import json, sys, re

BOT_LOGIN_PATTERNS = ('coderabbit', 'cubic-dev', 'cubic[bot]', 'greptile')

# Extend per bot when adding a new vendor.
NOISE_SUMMARIES = (
    r'🧩 Analysis chain',
    r'🤖 Prompt for AI Agents',
    r'🧠 Learnings used',
    r'✏️ Learnings added',
    # Cubic and others — add their summary text here as encountered.
)

def is_bot(login: str) -> bool:
    l = login.lower()
    return any(p in l for p in BOT_LOGIN_PATTERNS)

def strip_noise(body: str) -> str:
    for s in NOISE_SUMMARIES:
        body = re.sub(
            rf'<details>\s*<summary>{s}</summary>.*?</details>',
            '', body, flags=re.DOTALL,
        )
    return body

data = json.load(sys.stdin)
for c in data:
    if not is_bot(c.get('user', {}).get('login', '')):
        continue
    body = strip_noise(c.get('body', ''))
    print(f'\n=== bot={c["user"]["login"]} id={c["id"]} '
          f'{c.get("path","?")}:{c.get("line","?")} ===')
    print(body[:3500])
```

## Triage rubric

For each comment, assess in order:

1. **Is the claim correct?** Read the actual code at the line. Bot
   misreads happen — common patterns:
   - Loop-var capture flagged on body-scoped `:=` (fresh per iteration
     in every Go version).
   - Suggests refifying SHAs (e.g. `^refs/heads/<sha>`) when the
     variable is a SHA, not a branch name.
   - References wrong line numbers from an earlier diff.
2. **Real-world impact, not severity label.** Examples:
   - "🔴 Critical" wire-format bug that we already fixed in a prior
     commit → ack via diff, no work.
   - "🟡 Minor" silent-corruption defense (e.g. `refs/heads/`
     qualification) → real money, do it.
3. **LoC cost.** ≤ 5 lines and well-targeted = always worth doing even
   if impact is theoretical. > 30 lines and theoretical = defer with a
   trigger.
4. **Does the fix overshoot?** Bots sometimes propose broader refactors
   than needed. Apply the minimum safe fix; mention the broader idea in
   a reply or follow-up issue if useful.

Rank with the table format used in both modes:

```
| Rank | Source | ID | File:line | Issue | LoC | Real-world impact | Verdict |
```

`Source` is the bot login (`coderabbit`, `cubic`, `greptile`, …) so
multi-bot threads stay legible.

## Verdict categories

- **Do**: high-value-per-LoC. Ship it in this PR.
- **Defer**: real concern, cost-benefit doesn't justify *now*. Always
  pair with a concrete `revisit when…` trigger (file size threshold,
  second host added, CI shipped) AND a `TODO(ai-review)` marker at the
  relevant location. Reply on the comment.
- **Won't do**: the bot is wrong, or the suggested fix is worse than
  the current code. Reply with the explicit reasoning so the bot's
  learning store records the disagreement. No in-code marker — the
  reply is the closure.
- **Skip**: out of scope for this triage (already fixed externally,
  single-user concern, deliberate accept-the-risk). No code change, no
  per-comment PR reply, no in-code marker. Note the rationale in the
  triage table so a future reviewer doesn't re-litigate it.

## Implementation patterns

- **Group by theme, not by item.** "Correctness batch (5 small fixes in
  unrelated files)" reads better than 5 micro-commits.
- **Tests over comments.** If you'd write a paragraph explaining why,
  write a test that pins the behavior instead. Tests stay accurate;
  comments rot. (See AGENTS.md.)
- **Refactor for testability when the bot points at concurrency.** Race
  fixes in handler-test patterns (channel for handoff) and lifecycle
  guards (Stop without Start, double-Start) are best pinned with fast
  unit tests so they don't regress silently.
- **Run `go test -race`** when shared state in tests is flagged. Even
  if `go test` passes, the race detector is the right oracle.
- **Verify after each commit** with `make test` (or `go test ./...`).

## Replying on the PR

Use `gh api` with the `replies` endpoint to thread replies under the
original line comment:

```bash
gh api -X POST repos/<owner>/<repo>/pulls/<n>/comments/<id>/replies \
  -f body="$(cat <<'EOF'
**Deferred** — short reason here.

Tracked tradeoff:
- bullet 1
- bullet 2

**Revisit when** <concrete trigger>. Marked with TODO(ai-review) at <function>.
EOF
)"
```

Reply shapes:

- **Defer**: lead with `**Deferred**`, list the trade-off in 2-4
  bullets, end with `**Revisit when** <trigger>`. Reference the in-code
  anchor: `Marked with TODO(ai-review) at <function>`. Keep under 200
  words.
- **Won't do**: lead with `**Won't do**` and the *correct* reading.
  Don't be defensive — explain the Go semantic / runtime constraint the
  bot missed.
- **Addressed**: skip. The bot resolves itself when it scans the new
  diff. Posting "addressed in <sha>" replies is just noise next to the
  strikethrough that's about to appear.

## TODO(ai-review) markers

For every Defer verdict, leave a one-line marker at the place the
deferred work would land:

```go
// TODO(ai-review): <short issue> https://github.com/<owner>/<repo>/pull/<n>#discussion_r<id>
```

The marker name is intentionally bot-agnostic — `git grep
"TODO(ai-review)"` finds every deferred review-bot suggestion across
vendors.

Conventions:

- **Body is one short sentence**, no rationale (the PR thread carries
  that). Match AGENTS.md's "code is self-documenting".
- **Anchor at the call site**, not at the package top. The signal is
  "edit here, remember this".
- **Outside-the-diff defers** (review-summary items with no
  `discussion_r` anchor) link to the PR root: `.../pull/<n>` — no
  comment ID. Bots can't auto-resolve these even when fixed, so a human
  will eventually need to grep for `TODO(ai-review)` and match them up.
- **Won't-do and Skip get no marker.** A TODO would invite a future
  reader to re-litigate a closed thread.
- **Multiple defers in the same function** can share one TODO block,
  one line per item, each with its own discussion link.

## Comment style during fixes

**Don't over-explain yourself in new comments or docstrings.** Code
should be self-explanatory. Short comments are encouraged when they
genuinely help; skip them when the code already says it. Follow
AGENTS.md.

Concretely:

- One-line goal-oriented `why` notes ("prevent race", "avoid leak")
  next to the change. No "previously we did X" history — that's what
  `git log` is for.
- Pin the contract with a test instead of a paragraph when possible.
  Tests stay accurate; prose rots.
- Don't restate the obvious. A comment that paraphrases the next line
  is overhead with no payoff.
- Multi-paragraph reserved for external-system constraints (third-party
  bug links, protocol quirks). If a rationale fits in code or a test,
  put it there instead.
- The same rule applies to commit messages and PR replies: lead with
  the point, drop the throat-clearing.

## Common review-bot misses to watch for

| Symptom | Reality |
|---|---|
| "Loop-captured variable in closure" on body-scoped `:=` | Fresh per iteration in every Go version. |
| "Apply same fix to <variable>" when variable is a SHA, not a ref | SHAs are unambiguous; no `refs/heads/` needed. |
| "Token leak via `%+v`" when both fields are empty on the error path | Real principle, theoretical on the path. Often worth fixing anyway as a free defense. |
| "Add `t.Parallel()`" on tests that aren't actually independent | Verify independence (no shared `t.Setenv`, no temp-dir collisions, no global state) before applying. |
| Suggests `&http.Transport{}` is fine | Bare zero-value drops `ProxyFromEnvironment`, `IdleConnTimeout`, `TLSHandshakeTimeout`. Always `http.DefaultTransport.(*http.Transport).Clone()` instead. |

## Things to never do

- **Don't blindly apply the bot's diff.** Read the surrounding code; the
  bot often optimizes the wrong axis.
- **Don't post replies on items you fixed.** Adds noise.
- **Don't argue tone** in won't-do replies. State the technical
  reasoning, link to the Go spec / language version, move on.
- **Don't ship a Defer without a `TODO(ai-review)` marker.** Reply
  alone is forgettable; the in-code anchor is what survives. Conversely:
  never add a TODO marker for a Won't-do or Skip — the signal would
  invite re-litigation of a closed thread.
- **Don't over-explain in new code.** No multi-line docstrings or
  paragraphs that restate what the code already does. Short `why`
  notes only when genuinely helpful. Follow AGENTS.md.
- **(Autonomous)** Don't triage before the watch loop says the bots are
  done — half-finished bot output produces wrong triage.
- **(Autonomous)** Don't proceed past "Should I run?" if it says no.
  Exit silently — no triage table, no commits, no replies. (The run's
  own activity comment still records the no-op; that's the sole
  exception.) That gate *is* the loop terminator.
- **(Autonomous)** Don't post an empty triage *table* just because the
  routine fired. If the bots produced nothing new, the run's activity
  comment ending at its no-op line is the only thing posted.
- **Never merge — or enable auto-merge / mark the PR mergeable — while
  Greptile's confidence score is below 5/5.** It's a hard gate,
  independent of how the individual findings triage.
- **(Autonomous)** Don't nudge bots that are already working. The
  @-mention fallback fires only when *no* bot check is present, and at
  most once per HEAD (the `review-bot:nudge <sha>` marker enforces it).
- **(Autonomous)** Don't `--force` push. If the branch has moved under
  you, fetch, rebase, and re-run the verify step before pushing again.
