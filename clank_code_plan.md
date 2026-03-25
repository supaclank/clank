Now I have everything I need. Here's the complete plan:

---

# `clank code` — Implementation Plan

## Architecture Overview

```
User CLI
├── clank              → Inbox TUI (session manager + backlog)
├── clank code [prompt]→ Launch new agent session (opens session detail, can navigate back to inbox)  
├── clank daemon start → Start background daemon (auto-started on first use)
├── clank daemon stop  → Stop daemon + all managed agents
├── clank daemon status→ Show running agents
└── (existing cmds)    → scan, triage, list, show, etc. unchanged

Daemon (long-lived background process)
├── Unix socket API    (~/.clank/daemon.sock)
├── Agent Manager
│   ├── OpenCode backends (one `opencode serve` per project)
│   └── Claude Code backends (one `claude -p` per session)
├── Event aggregation  (SSE from OpenCode, stdout from Claude)
└── State persistence  (writes to clank.db)

TUI ↔ Daemon communication: Unix domain socket (JSON-RPC or simple REST over HTTP)
```

## Component Breakdown

### Phase 1: Daemon Foundation

**1.1 `internal/daemon/daemon.go` — Daemon core**

The daemon is a background process that:
- Listens on `~/.clank/daemon.sock` (Unix domain socket)
- Manages child processes (OpenCode servers, Claude CLI)
- Aggregates events from all backends
- Writes status updates to `clank.db`
- Exposes an HTTP API over the socket for the TUI to consume

Lifecycle:
- `clank daemon start` — forks a background process, writes PID to `~/.clank/daemon.pid`
- `clank daemon stop` — sends shutdown signal, daemon gracefully kills children
- `clank daemon status` — prints running sessions
- Auto-start: when `clank` or `clank code` runs, if daemon isn't running, start it automatically

**1.2 `internal/daemon/client.go` — Daemon client**

A Go client that connects to the daemon socket. Used by the TUI and CLI commands.

```go
type Client struct { /* connects to ~/.clank/daemon.sock */ }

func (c *Client) CreateSession(req CreateSessionReq) (*Session, error)
func (c *Client) ListSessions() ([]Session, error)
func (c *Client) GetSession(id string) (*Session, error)
func (c *Client) SendMessage(sessionID, text string) error
func (c *Client) AbortSession(sessionID string) error
func (c *Client) SubscribeEvents() (<-chan Event, error)  // SSE-style stream
func (c *Client) ReplyPermission(reqID, reply string) error
func (c *Client) Ping() error  // health check
```

**1.3 Daemon socket API (HTTP over Unix socket)**

| Method | Path | Description |
|--------|------|-------------|
| GET | `/ping` | Health check |
| POST | `/session` | Create new agent session |
| GET | `/sessions` | List all managed sessions |
| GET | `/session/:id` | Get session detail |
| POST | `/session/:id/message` | Send message to agent |
| POST | `/session/:id/abort` | Abort running agent |
| DELETE | `/session/:id` | Stop and remove session |
| GET | `/events` | SSE stream of all events |
| POST | `/permission/:id/reply` | Reply to permission request |

### Phase 2: Agent Backends

**2.1 `internal/agent/agent.go` — Backend interface**

```go
type Backend interface {
    Start(ctx context.Context, req StartRequest) error
    SendMessage(ctx context.Context, text string) error
    Abort(ctx context.Context) error
    Stop() error
    Events() <-chan Event
    Status() SessionStatus
    SessionID() string
}

type StartRequest struct {
    ProjectDir  string
    Prompt      string
    SessionID   string   // empty = new, set = resume
    TicketID    string   // optional, link to backlog ticket
}

type Event struct {
    Type       string    // "status", "message", "part", "permission", "error"
    SessionID  string
    Timestamp  time.Time
    Data       any       // type-specific payload
}

type MessageEvent struct {
    Role    string   // "user", "assistant"
    Content string
    Parts   []Part
}

type Part struct {
    Type   string  // "text", "tool_call", "tool_result", "thinking"
    Text   string
    Tool   string  // tool name if tool_call/result
    Status string  // "pending", "running", "completed", "error"
}

type PermissionEvent struct {
    RequestID   string
    Tool        string
    Description string
}
```

**2.2 `internal/agent/opencode.go` — OpenCode backend**

- On first session for a project: spawn `opencode serve --port=0` in the project directory, parse stdout for the URL, store the port
- On subsequent sessions for same project: reuse existing server
- Create session: `POST /session`
- Send prompt: `POST /session/{id}/prompt_async`
- Stream events: `GET /event` (SSE) — parse into our `Event` type
- Handle permissions: watch for `permission.asked`, forward to TUI, relay reply
- Detect completion: `session.status` event with `type: "idle"`
- Server lifecycle: daemon manages server processes. Idle timeout (e.g., 30 min no sessions) kills the server.

Key details:
- SSE parsing: standard `data: {...}\n\n` format, JSON decode each line
- Map OpenCode events: `session.status → status event`, `message.part.delta → part event`, `permission.asked → permission event`
- The experimental `GET /experimental/session` endpoint lists sessions across projects — useful for the inbox

**2.3 `internal/agent/claude.go` — Claude Code backend**

- Spawn: `claude -p "<prompt>" --output-format stream-json --allowedTools "Read,Edit,Bash,Write" --verbose`
  - Run in the project directory (set `cmd.Dir`)
  - Detach into own process group (so it survives TUI exit — daemon is parent)
- Parse streaming JSON from stdout, line by line. Each line is a JSON object with a `type` field:
  - `type: "system"` with `subtype: "init"` → session started, extract `session_id`
  - `type: "assistant"` → assistant message (text + tool calls)
  - `type: "result"` → session complete. `subtype` tells us success/error. Has `session_id`, `total_cost_usd`
- Resume: `claude -p "<follow-up>" --output-format stream-json --resume <session_id>`
- Abort: send SIGINT to the process
- Read history: parse `~/.claude/projects/<encoded-cwd>/<session-id>.jsonl` or use `claude` CLI's list functionality
- Permissions: Pre-approve common tools via `--allowedTools`. For v1, we can use `--dangerously-skip-permissions` as an opt-in config, or prompt the user in the TUI when the process asks for confirmation (detect `[Y/n]` patterns in output, though this is fragile). The Agent SDK's `--output-format stream-json` should include permission events we can parse.

### Phase 3: Store Evolution

**3.1 Schema migration**

Expand `session_status` to support the daemon-managed sessions:

```sql
-- New columns (added via ALTER TABLE, idempotent)
ALTER TABLE session_status ADD COLUMN backend_type TEXT NOT NULL DEFAULT '';
ALTER TABLE session_status ADD COLUMN backend_meta TEXT NOT NULL DEFAULT '{}';
ALTER TABLE session_status ADD COLUMN last_read_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE session_status ADD COLUMN prompt TEXT NOT NULL DEFAULT '';
ALTER TABLE session_status ADD COLUMN ticket_id TEXT NOT NULL DEFAULT '';
ALTER TABLE session_status ADD COLUMN project_path TEXT NOT NULL DEFAULT '';
ALTER TABLE session_status ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0;
```

- `backend_type`: `"opencode-server"` or `"claude-cli"`
- `backend_meta`: JSON with `{"port": 4123, "pid": 12345, "serverPid": 6789}` etc.
- `last_read_at`: replaces the `unread` boolean — timestamp of last user view
- `prompt`: the initial prompt sent to the agent
- `ticket_id`: FK to `ticket.id` if launched from backlog
- `project_path`: working directory for this session

The existing `unread` boolean is kept for backward compat with the plugin but the TUI switches to using `last_read_at`.

### Phase 4: TUI

**4.1 `internal/tui/inbox.go` — Inbox view (replaces sessions.go)**

The main screen. Shows all sessions grouped by status, with backlog below.

```
┌─ CLANK ─────────────────────────────────────────┐
│ BUSY (1)                                         │
│   ● Fix auth bug in login.py         bezos  2m   │
│                                                   │
│ UNREAD (2)                                        │
│   * Refactor database queries        clank  15m   │
│   * Migrate to PostgreSQL            pocket 1h    │
│                                                   │
│ IDLE (1)                                          │
│     Add dark mode toggle             fuselage 3h  │
│                                                   │
│ ERROR (0)                                         │
│                                                   │
│ ── BACKLOG ──────────────────────────────────────│
│   #42  [quickwin]  Login timeout on slow conn.    │
│   #38  [quickwin]  Export metrics to CSV          │
│   #35  [valuebet]  Memory leak in scanner         │
└──────────────────────────────────────────────────┘
  [n]ew  [enter] open  [a]pprove  [f]ollowup  [x] archive  [?] help
```

Key behaviors:
- Groups: BUSY, UNREAD (idle + unread), IDLE (idle + read), ERROR, FOLLOW-UP. Approved/archived sessions hidden by default.
- Enter on a session → session detail view
- Enter on a backlog ticket → new session dialog pre-filled with the ticket
- `n` → new session dialog
- Auto-refreshes by polling daemon via socket (or SSE stream)
- Unread detection: session is unread if `last_read_at < last_message_time` (daemon tracks this)

**4.2 `internal/tui/sessionview.go` — Session detail view**

Shows a single agent session with streaming output.

```
┌─ Fix auth bug ──────────────── [busy] ● ────────┐
│                                                   │
│ You: Fix the authentication timeout bug in        │
│ login.py. The connection pool isn't being cleaned  │
│ up properly.                                      │
│                                                   │
│ Agent:                                            │
│ I'll look into the auth timeout issue.            │
│                                                   │
│ [Read] login.py                          ✓ done   │
│ [Edit] login.py:42-58                    ✓ done   │
│ [Bash] pytest tests/test_auth.py         ● running│
│                                                   │
│ ▊                                                 │
└──────────────────────────────────────────────────┘
  [m]essage  [a]pprove  [f]ollowup  [x]archive  [q] back
```

Key behaviors:
- Streams events from the daemon in real-time (tool calls, text output)
- `m` → text input at bottom, sends follow-up message to agent
- Tool calls shown as compact lines with status indicators
- Viewport scrollable (follows tail by default when agent is busy)
- Permission prompts shown inline: `[Permission] Allow Edit on login.py? [y/n]`
- `q` → back to inbox (agent keeps running in daemon)
- Opening this view sets `last_read_at = now()`

**4.3 `internal/tui/newsession.go` — New session dialog**

```
┌─ New Session ────────────────────────────────────┐
│                                                   │
│ Backend:  [OpenCode]  [Claude Code]               │
│ Project:  ~/github.com/acksell/clank              │
│ Ticket:   #42 Login timeout on slow connections   │
│                                                   │
│ Prompt:                                           │
│ ┌──────────────────────────────────────────────┐ │
│ │ Fix the login timeout bug. The connection    │ │
│ │ pool cleanup is missing a finally block.     │ │
│ └──────────────────────────────────────────────┘ │
│                                                   │
│              [Launch]    [Cancel]                  │
└──────────────────────────────────────────────────┘
```

- Backend selector: tab between OpenCode and Claude Code
- Project: defaults to CWD, can browse registered repos
- Ticket: optional, can pick from backlog
- Prompt: free text, pre-filled from ticket description if selected
- Launch → daemon creates session → navigates to session detail view

### Phase 5: CLI Command Wiring

**5.1 `clank` (root command, no subcommand)**

```
clank              → opens inbox TUI (ensures daemon is running)
```

Change root command to have a `RunE` that launches the inbox TUI. Existing subcommands (`scan`, `triage`, etc.) still work.

**5.2 `clank code [prompt]`**

```
clank code                        → opens new session dialog in TUI
clank code "fix the auth bug"     → launches session immediately with prompt, shows detail view
clank code --backend claude       → forces Claude Code backend
clank code --project ~/myrepo     → sets project directory
clank code --ticket 01JQXYZ       → links to backlog ticket, pre-fills prompt
```

**5.3 `clank daemon start|stop|status`**

```
clank daemon start    → starts daemon (if not running)
clank daemon stop     → graceful shutdown
clank daemon status   → shows PID, uptime, active sessions
```

### Implementation Order

| Step | What | Files | Dependencies |
|------|------|-------|-------------|
| **1** | Agent interface + event types | `internal/agent/agent.go` | None |
| **2** | Daemon core (socket server, process management, PID file) | `internal/daemon/daemon.go` | Step 1 |
| **3** | Daemon client | `internal/daemon/client.go` | Step 2 |
| **4** | `clank daemon start/stop/status` CLI commands | `cmd/clank/main.go` | Step 2, 3 |
| **5** | OpenCode backend | `internal/agent/opencode.go` | Step 1 |
| **6** | Claude Code backend | `internal/agent/claude.go` | Step 1 |
| **7** | Wire backends into daemon | `internal/daemon/daemon.go` | Step 2, 5, 6 |
| **8** | Schema migration (new columns) | `internal/store/store.go` | None |
| **9** | Inbox TUI | `internal/tui/inbox.go` | Step 3, 8 |
| **10** | Session detail TUI (streaming view) | `internal/tui/sessionview.go` | Step 3 |
| **11** | New session dialog | `internal/tui/newsession.go` | Step 3 |
| **12** | `clank` root command → inbox, `clank code` command | `cmd/clank/main.go` | Step 9, 10, 11 |
| **13** | Permission handling in TUI | `internal/tui/sessionview.go` | Step 10 |
| **14** | Unread detection (`last_read_at` approach) | daemon + inbox | Step 7, 9 |

### What Happens to Existing Code

- **`tui/sessions.go`** — Superseded by `tui/inbox.go`. Can be removed or kept as a legacy fallback.
- **`tui/actionmenu.go`** — Reused in inbox (approve/archive/followup actions).
- **`terminal/open.go`** — No longer needed for the daemon approach (agents run inside the daemon, viewed in the TUI). Could be kept as a fallback for "open in external terminal."
- **`plugin/index.ts`** — Still useful for detecting status of sessions run outside Clank (plain `opencode` in a terminal). But for `clank code` sessions, the daemon handles status directly.
- **`scanner/opencode/opencode.go`** — Still used for `clank scan` (ticket extraction from historical sessions). The inbox may also use it to show sessions not managed by the daemon.

### Open Questions for You

1. **Daemon auto-start**: Should `clank` and `clank code` auto-start the daemon if it's not running? (I'd say yes — the user shouldn't need to remember `clank daemon start`.)

2. **External sessions in inbox**: Sessions run via plain `opencode` or `claude` (not through `clank code`) — should they appear in the inbox too? The scanner can detect them, but we can't stream their output or send messages. They'd be read-only rows. I'd include them with a visual distinction (dimmed, or a "not managed" indicator).

3. **Which step to start with?** I'd suggest starting with steps 1-4 (daemon foundation) since everything else depends on it. Or we could stub the daemon and start with the TUI to get the visual feel right first. What's your preference?