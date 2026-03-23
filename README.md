# Clank

Scans your AI coding sessions, extracts unfinished threads and improvement opportunities, and presents a backlog in an interactive TUI for triaging and delegation.

Clank helps you focus on the high impact & high complexity work, while taking care of the low complexity & high impact work in the background.

Developers using AI coding assistants generate many ideas, plans, and TODOs during sessions — but many are forgotten. Clank systematically recovers these lost threads and helps you prioritize them.

Clank runs overnight. Wake up to PRs for different low-hanging fruits, and a daily digest of new tickets to triage based on yesterday's coding sessions.

Clank understands your business and objective across repos. Configure this context to help Clank prioritise what needs your attention.

You can't do 5 different things at the same time. Clank helps parallelize work whilst preventing context switching between different terminals by taking some of the smaller easy workloads off of your shoulders.

## How it works

1. **Scan** — Reads your [opencode](https://opencode.ai) session history (SQLite) and sends transcripts to an LLM
2. **Extract** — The LLM identifies two kinds of items:
   - **Unfinished threads**: plans discussed but never executed, abandoned ideas, TODOs left incomplete
   - **Opportunities**: improvement suggestions, refactoring ideas, next steps mentioned by the assistant
3. **Triage** — Review extracted tickets in an interactive TUI, edit fields, and use AI-assisted triage to score and classify them

A central context store (`~/.clank/context/`) lets you provide product roadmap, strategy, and ideas as markdown files — the AI uses these during analysis for better relevance.

## Roadmap

**Done**

- [x] Opencode session scanning
- [x] LLM-powered ticket extraction (unfinished threads + opportunities)
- [x] Interactive TUI (list, detail, inline editing)
- [x] Central context store for product roadmap/strategy/ideas
- [x] Impact/complexity quadrant scoring and sorting
- [x] Pluggable scanner interface (ready for additional adapters)

**Now** — Local open-source tool for individual developers. Locally run, bring your own API keys.

- [ ] Good way to auto-close tickets.
- [ ] Semantic search over coding sessions/tickets
- [ ] Additional session source adapters (Claude Code, Cursor, etc.)
- [ ] Automated overnight scanning (cron/daemon) with daily digest
- [ ] Background agent that picks up low-complexity/high-impact tickets and opens PRs autonomously
- [ ] Optional cloud-hosted analysis (sign in and trigger scans remotely, useful for nightly runs)

**Later** — Team and organisation layer.

- [ ] Shared state across developers — aggregate conversations from across the org
- [ ] Hosted solution for orchestrating

## Install

```
go install github.com/acksell/clank/cmd/clank@latest
```

Or build from source:

```
git clone https://github.com/acksell/clank.git
cd clank
go build ./cmd/clank
```

Requires Go 1.25+. No CGO needed (uses pure-Go SQLite).

## Setup

```bash
# Set your OpenAI API key (or any OpenAI-compatible provider)
export OPENAI_API_KEY="sk-..."

# Or configure via clank
clank config set llm.api_key "sk-..."
clank config set llm.model "gpt-4o-mini"        # default
clank config set llm.base_url "https://api.openai.com/v1"  # default

# Register a repo
clank init          # registers current directory
# or
clank repo add /path/to/project
```

Configuration lives at `~/.clank/config.toml`.

## Usage

```bash
# Scan sessions and extract tickets
clank scan                    # auto-discovers projects from opencode DB
clank scan /path/to/repo      # scan specific repo

# Interactive triage TUI
clank triage

# List tickets (non-interactive)
clank list
clank list --status new --type unfinished_thread
clank list --quadrant quickwin

# Show a ticket (supports prefix matching on ID)
clank show 01J5

# Edit central context (roadmap, strategy, ideas)
clank context

# Backfill impact scores for unscored tickets
clank backfill
clank backfill --dry-run
```

## TUI keybindings

**List view**

| Key | Action |
|-----|--------|
| `enter` | Open ticket detail |
| `b` | Move to backlog |
| `x` | Discard |
| `d` | Cycle status |
| `a` | AI triage |
| `q` | Quit |

**Detail view**

| Key | Action |
|-----|--------|
| `e` | Edit mode |
| `tab` | Next field |
| `enter` | Save / confirm |
| `esc` | Cancel / back |

## Data model

Tickets are scored on two dimensions (1-5 each) and mapped to quadrants:

| Quadrant | Impact | Complexity | Meaning |
|----------|--------|------------|---------|
| Quick Win | >= 3 | < 3 | High value, easy to do |
| Value Bet | >= 3 | >= 3 | High value, significant effort |
| Tidy Up | < 3 | < 3 | Low stakes, easy to do |
| Distraction | < 3 | >= 3 | Low value, high effort |

Tickets flow through statuses: `new` -> `triaged` -> `backlog` -> `doing` -> `done` (or `discarded`).

## Architecture

```
cmd/clank/main.go          Cobra CLI, command wiring
internal/scanner/           Scanner interface + opencode adapter
internal/analyzer/          LLM-powered ticket extraction and triage
internal/store/             SQLite persistence (tickets, repos)
internal/tui/               Bubble Tea TUI (list, detail, triage views)
internal/llm/               OpenAI-compatible HTTP client
internal/config/            TOML config management
internal/context/           Central context file management
```

All state is stored locally in `~/.clank/`:
- `clank.db` — SQLite database (tickets + repos)
- `config.toml` — Configuration
- `context/` — Markdown files for product context (fed to AI during analysis)

