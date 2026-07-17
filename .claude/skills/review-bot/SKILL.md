---
name: review-bot
description: Use this skill when handling AI review-bot comments on a GitHub PR (CodeRabbit/CR, Cubic, Greptile, Gemini Code Assist, GitHub Copilot, and similar). Triggers include "address the CR/cubic/greptile/gemini/copilot review", "look at coderabbit/cubic/greptile/gemini/copilot comments", "did the review bot comment", "triage PR bot feedback", or any mention of bot review feedback that needs action. Also used by an automated routine that fires once on PR open and runs this triage unattended.
---

# Review-bot triage

Goal: turn the noisy stream of AI review-bot comments (CodeRabbit, Cubic,
Greptile, Gemini Code Assist, GitHub Copilot, future bots) into a small
set of high-value commits + PR
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
| Gemini Code Assist | `gemini-code-assist` | _(none — posts a PR review, not a check-run)_ | `@gemini-code-assist` |
| GitHub Copilot | `copilot` (review author is `copilot-pull-request-reviewer[bot]`, but its **line comments** post under login `Copilot` — match on the `copilot` substring to catch both) | _(none — posts a PR review, not a check-run)_ | _(none — re-request as a reviewer)_ |

**Gemini and Copilot are review-based, not check-based.** On this repo they
publish a submitted PR **review** (in `pulls/<n>/reviews`), not a CI check-run
— Gemini's body opens with `## Code Review`, Copilot's with `## Pull request
overview`. Watch and triage them via the review/login path (see "Fallback for
bots without check runs"), not `BOT_CHECK_SLUGS`. **Copilot is lower-signal**
(often shallow or duplicative): triage it conservatively, and it never counts
toward mergeability on its own.

**Merge gate — surface only; the decision lives in the `auto-merge` skill.**
Greptile writes a `<h3>Confidence Score: N/5</h3>` into the **PR description
body** (not a comment) — read it from `pulls/<n>` (`.body`); surface that score
when present. review-bot only triages and surfaces signals; it does NOT decide
mergeability. Report what each trusted reviewer did (Greptile score, whether
Gemini or Copilot reviewed) and leave the verdict to `auto-merge` — don't
declare a PR "not mergeable" from this skill.

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

Per fire, the **very first action — before the watch loop, before the
"Should I run?" gate, before anything else — is to post a fresh
acknowledgment comment** saying the routine is running. Then narrate
each phase as a new small comment (see "Activity stream") as you move
through the steps, and mirror every state change in a `review-bot`
commit status on the PR's HEAD (see "Merge-box status check"), so a
human watching the PR sees the unattended routine progress in real time
— and the merge box says "running" until the loop converges:

1. **Post the acknowledgment** — immediately and unconditionally, a
   comment saying the routine has started and is waiting on the bots.
   This opens the run's activity stream and goes up even on a run that
   turns out to be a no-op. Right behind it, post the `pending` status
   on HEAD.
2. **Watch for bot completion** on the current HEAD — let any
   in-progress review pass finish (grace window + hard cap),
   heartbeating the activity stream and status every ~2 minutes. If
   no bot check ever appears, **nudge the bots** by @-mention (see
   "Nudge the bots when none respond").
3. **Should I run?** — exit when the bots produced nothing new since
   the prior triage comment: no triage table — the stream ends with
   the prominent "converged" comment and the status flips to
   `success`.
4. **Triage** the new findings per the rubric, **fix**, **push**, then
   post `pending` on the freshly pushed HEAD — the successor run owns
   flipping it.
5. The push fires another `synchronize`, restarting the loop. The
   bots eventually stop producing new findings, step 3 exits green,
   and the loop converges.

### Activity stream

So a human watching the PR can follow the unattended routine **in real
time, interleaved chronologically with the bots' own comments**, the run
narrates itself as a stream of **small new comments — one per phase
event** — never by editing one pinned comment. An edited comment keeps
its original timeline position, so the moment a bot posts below it the
run looks stale, and "has it reacted to that yet?" becomes guesswork; a
fresh comment per event makes the chronology legible at a glance. That
is the point of the stream: new comments are cheap, so never batch or
skip them to look tidy. Prefix each with the run's identity so
successive (or racing) runs stay distinguishable:

```markdown
🤖 **review-bot** _(run 13:58 · HEAD `a1b2c3d`)_ — ⏳ waiting on bot reviews (up to 25m)…
```

**The acknowledgment comment is the routine's very first action** —
before the watch loop, before the "Should I run?" gate, even on a run
that ends up a no-op. Each comment is one or two lines, present-tense
about what is starting *now* (never a recap written after the fact),
posted as the phase *begins*. Events worth a comment: the acknowledgment
("waiting on bot reviews, up to 25m"), bot completion or absence (note
Greptile's confidence score; any nudge sent), the triage verdict — the
triage-table comment itself serves as this event — the implement/push
start ("fixing 3 do's + 1 defer; pushing shortly"), and a terminal
comment (two shapes, below). One sentence each; the triage table and
the diff carry the detail.

**Terminal comments — two shapes, and the difference is the point.** A
run that pushed fixes ends *mid-loop*: its terminal line is an ordinary
one-liner ("✅ done — pushed 3 fixes; bots re-reviewing, next run
triages"), because a successor run is expected and the status stays
`pending`. But when the **loop** ends — no successor coming, status
flipping to `success` (converged, or triage produced nothing to push)
or `error` (aborted) — the terminal comment gets a prominent header so
a scan of the timeline can't mistake it for one more status line:

```markdown
# ✅ Review-bot converged

_run 20:11 · HEAD `d875222` · status → success_

No bot comment postdates the prior triage ([table](…#issuecomment-4996111980), 20:07 UTC) —
this HEAD is that triage's own fix-push. Nothing left to triage.
Loop totals: 2 rounds, 7 findings (5 do, 1 defer, 1 won't-do).
```

The "Loop totals" recap is cheap — the prior triage tables were already
fetched for the "Should I run?" gate; sum their verdicts rather than
re-fetching. An aborted run swaps the header for `# ⚠️ Review-bot
aborted` (status → `error`) plus the one-line reason and what a later
run must pick up. A no-op run posts no separate "nothing new" one-liner
— its converged comment *is* that event. The prominence rule mirrors
the status rule exactly: header-format terminal ⇔ terminal status
(`success`/`error`); one-liner terminal ⇔ `pending` lives on.

```bash
# Phase event (or heartbeat while buried): post a NEW comment; keep the
# newest comment's id (for elapsed bumps) and html_url (the status
# check's target_url).
resp=$(gh api -X POST "repos/$PR_OWNER/$PR_REPO/issues/$PR_NUM/comments" \
  -f body="$line")
act_id=$(jq -r .id <<<"$resp"); act_url=$(jq -r .html_url <<<"$resp")

# Heartbeat while our comment is still the PR's newest item: bump
# elapsed in place.
gh api -X PATCH "repos/$PR_OWNER/$PR_REPO/issues/comments/$act_id" \
  -f body="$line"

# Buried check — did anything land after our newest comment? Compare its
# created_at against the newest of issues/<n>/comments,
# pulls/<n>/reviews, and pulls/<n>/comments.
```

**Heartbeat — a live run must never look dead or buried.** Any step that
can run long — the bot watch loop, a nudge grace window, and equally
local work like `npm ci` or a slow test suite — first gets a comment
stating its worst case ("up to 25m"), then every ~2 minutes:

- Still the newest item on the PR (no comment, review, or commit landed
  after ours)? **Edit it in place**, bumping the elapsed time ("⏳
  waiting on bots — greptile in_progress · 9m/25m"). Edits don't move a
  comment or notify anyone, which is fine while nothing is newer.
- Buried under new activity? **Post a fresh one-liner instead** ("still
  running — waiting on cubic · 12m/25m"), so the newest review-bot
  comment is never more than ~2–3 minutes behind any other actor while a
  run is live.

Either way, refresh the `pending` status description with the same short
text (next section). A stream whose newest entry is 20 minutes old reads
as a dead run and invites a premature human merge — that staleness is
exactly the bug the heartbeat exists to prevent.

The merge-box status check (next section) stays the glanceable "is it
still running?" signal between comments; the stream carries the
chronology. Every fire opens with its own acknowledgment, including a
no-op that exits at "Should I run?".

### Merge-box status check

The activity stream is the narrative, but a human deciding whether to
merge looks at the **merge box** — so the routine also maintains a
[commit status](https://docs.github.com/en/rest/commits/statuses)
(`context: review-bot`) on the PR's HEAD. While it's `pending`, the PR
shows "Some checks haven't completed yet" right next to the merge
button, and the `auto-merge` skill's all-checks-green gate holds off
automatically. This is the Statuses API, not the Checks API — check
*runs* (what CodeRabbit/Greptile publish) can only be created by GitHub
Apps, while the plain `repo`-scoped token this routine already pushes
with can post statuses.

```bash
# $act_url: the newest activity comment's html_url — "Details" links there.
set_status() {  # set_status <pending|success|error> <description ≤140 chars>
  gh api -X POST "repos/$PR_OWNER/$PR_REPO/statuses/$HEAD_SHA" \
    -f state="$1" -f context=review-bot \
    -f description="$2" -f target_url="$act_url" \
    >/dev/null || true  # a failed status post must never kill the run
}
```

State machine — `pending` means "a run is active on this HEAD, or fixes
were just pushed and the successor run hasn't converged"; `success`
means "the loop converged: a run saw the latest bot output and had
nothing left to do"; `error` means the run aborted. The status describes
the *routine*, never code quality:

- **Run start** (right after the acknowledgment comment):
  `set_status pending "run started — waiting on bot reviews"`.
- **Every phase boundary and every heartbeat**: re-post `pending` with a
  fresh description mirroring the newest activity comment ("waiting on
  bots — 9m/25m", "triaging 6 findings", "implementing fixes").
  Re-posting the same context supersedes the previous description in
  the merge box.
- **Converged exit** — the "Should I run?" gate said no, or the run
  finished with nothing to push (all verdicts Won't-do/Skip):
  `set_status success "converged — nothing new to triage"` (or "triage
  complete — replies only, nothing pushed"), paired with the prominent
  converged terminal comment (see "Activity stream").
- **After pushing fixes**: re-resolve `HEAD_SHA` to the new sha and post
  `pending "fixes pushed — bots re-reviewing; next run triages"` as the
  run's final status act. **Never post `success` on a sha this run just
  pushed** — the `synchronize` successor owns that sha's terminal state.
  The old sha's status needs no cleanup; the merge box only shows HEAD.
- **Abnormal exit** (missing inputs, unrecoverable error):
  `set_status error "<one-line reason>"`, then exit.

**Per-commit reconciliation.** A status binds to one sha, and the merge
box only ever shows the *current* HEAD's statuses — nothing carries
forward automatically. That's the point: a green from an old sha can
never vouch for code it hasn't seen. New commits reconcile like this:

- **Our own push** — the pushing run re-marks the new sha `pending`
  before it exits (above), so the box never shows a gap.
- **Anyone else's push** — fires `synchronize`, and that run's first
  action re-marks the new HEAD. Until it does, the new sha simply has no
  `review-bot` entry: the box shows *absent*, never a stale green. (With
  a required status check, absent renders as "Expected — waiting for
  status" and blocks merge, closing even that gap.)
- **Superseded shas** need no cleanup — their statuses drop out of view
  the moment HEAD moves, and a racing run's late post onto an old sha is
  invisible for the same reason.

Liveness convention: heartbeats refresh the status every ~2 minutes, so
a `review-bot` `pending` whose most recent update is older than ~45
minutes means that run died — a later run (or a human) may supersede it
rather than wait forever.

If the *first* status post fails (HTTP 403 — a fine-grained token
missing "Commit statuses: write"; classic `repo` scope always has it),
post one ⚠️ comment to the activity stream ("can't post commit statuses
— merge-box signal disabled this run") and skip status calls for the
rest of the run. Statuses are a signal, never a reason to abort triage.

To turn the signal into a hard gate, make `review-bot` a **required
status check** in the repo's branch protection / ruleset — GitHub then
disables the merge button while it's pending. That's a one-time repo
setting; the routine needs no change.

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

A "silent" exit still settles the run's bookkeeping: post the prominent
`# ✅ Review-bot converged` terminal comment (see "Activity stream") and
`set_status success` — silent means no triage table, not a vanished run.

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

No hard lock. No `--force` push. Statuses inherit the same softness — a
losing run's terminal post lands on a superseded sha, which the merge
box no longer shows.

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

start=$(date +%s); grace=$((start + 180)); cap=$((start + 25*60)); i=0

while :; do
  now=$(date +%s)
  # Every 4th poll (~2m): heartbeat — bump elapsed on the newest stream
  # comment (post a fresh one if buried) + re-post the pending status.
  (( i++ % 4 == 0 )) && heartbeat
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

**Fallback for bots without check runs.** If a bot only posts
comments/reviews and never publishes a check run, fall back to the older
heuristic: watch `pulls/<n>/reviews` for a submitted review from that
bot's login, plus a 60s quiescence tail. Add the bot to
`BOT_LOGIN_PATTERNS` so its comments still get triaged. **Gemini Code
Assist (`gemini-code-assist`) and GitHub Copilot
(`copilot-pull-request-reviewer`) are exactly this case** — they submit
reviews, not check-runs, so wait on their reviews here rather than in the
`BOT_CHECK_SLUGS` loop. To avoid unnecessary 3–6 minute grace-period
delays when no check-run bots are active, poll `pulls/<n>/reviews` for
completed reviews from these logins inside the grace-period wait and
break early as soon as all expected review-based bots have a submitted
review, rather than waiting out the full window.

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
BOT_MENTIONS=(@coderabbitai @greptileai @cubic-dev-ai @gemini-code-assist)  # Copilot isn't @-nudgeable — re-request it as a reviewer instead
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
- **Log it** in the activity stream ("coderabbit, cubic no check —
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

**Trusted review (current HEAD):** Gemini ✓; Greptile absent (capped). Mergeability is the `auto-merge` skill's call, not review-bot's.
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
- The push moved HEAD: re-resolve `HEAD_SHA` and post the `pending`
  status on the new sha ("fixes pushed — bots re-reviewing; next run
  triages") — the `synchronize` successor run owns flipping it (see
  "Merge-box status check").
- Reply on every Defer and Won't-do per "Replying on the PR".

### Inputs the routine must provide

- `owner/repo` and PR number, OR a checked-out branch whose upstream
  resolves to a known PR via `gh pr view --json number,headRepository`.
- A clean worktree at the PR's HEAD (the routine harness handles
  checkout/pull).
- `gh` authenticated as the bot identity, with push access to the PR
  branch. (Classic `repo` scope also covers the commit-status posts; a
  fine-grained token additionally needs "Commit statuses: write".)

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

# 'copilot' (not 'copilot-pull-request-reviewer') — review summaries post under
# `copilot-pull-request-reviewer[bot]` but line comments post under `Copilot`;
# the bare substring catches both (is_bot lowercases before matching).
BOT_LOGIN_PATTERNS = ('coderabbit', 'cubic-dev', 'cubic[bot]', 'greptile', 'gemini-code-assist', 'copilot')

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

`Source` is the bot login (`coderabbit`, `cubic`, `greptile`, `gemini-code-assist`, `copilot-pull-request-reviewer`, …) so
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
- **(Autonomous)** Don't start the watch loop — or any other work —
  before the acknowledgment comment and the `pending` status are up.
  The "process is running" signals are always the first action of a
  run, even one that no-ops at "Should I run?".
- **(Autonomous)** Don't exit on any path without settling the current
  HEAD's `review-bot` status: converged → `success`, aborted → `error`,
  just-pushed → `pending` on the new sha. And never post `success` on a
  sha this run pushed — green means "latest bot output triaged, nothing
  left", not "we pushed something".
- **(Autonomous)** Don't run any long step — bot watch, nudge window,
  `npm ci`, a slow test suite — for more than ~2–3 minutes without a
  heartbeat: bump the newest stream comment, or post a fresh one once
  buried, plus a status refresh. A 20-minute silent gap reads as a dead
  run and invites a premature manual merge.
- **(Autonomous)** Don't triage before the watch loop says the bots are
  done — half-finished bot output produces wrong triage.
- **(Autonomous)** Don't proceed past "Should I run?" if it says no.
  Exit silently — no triage table, no commits, no replies. (The run's
  own activity stream still records the no-op, and the status still
  flips to `success`; those are the sole exceptions.) That gate *is*
  the loop terminator.
- **(Autonomous)** Don't post an empty triage *table* just because the
  routine fired. If the bots produced nothing new, the run's activity
  stream ending in the prominent converged comment is the only thing
  posted.
- **Don't merge, enable auto-merge, or declare a PR mergeable from this
  skill** — that verdict belongs to the `auto-merge` skill. Surface
  Greptile's score and which trusted reviewers ran; never call a PR
  un-mergeable from here.
- **(Autonomous)** Don't nudge bots that are already working. The
  @-mention fallback fires only when *no* bot check is present, and at
  most once per HEAD (the `review-bot:nudge <sha>` marker enforces it).
- **(Autonomous)** Don't `--force` push. If the branch has moved under
  you, fetch, rebase, and re-run the verify step before pushing again.
