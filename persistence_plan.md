Now I have everything I need. Here's the final plan:

---

## Final Plan: Load Historical OpenCode Sessions into `clank inbox`

**Tool: todowrite**

**Input:**
```json
{
  "todos": [
    {
      "content": "Phase 1: Delete legacy code (scanner, store, analyzer, llm, context, plugin, legacy TUI views, legacy CLI commands)",
      "status": "pending",
      "priority": "high"
    },
    {
      "content": "Phase 2: Clean up config — remove ScanConfig, simplify configCmd",
      "status": "pending",
      "priority": "high"
    },
    {
      "content": "Phase 3: Add ExternalID field to SessionInfo",
      "status": "pending",
      "priority": "high"
    },
    {
      "content": "Phase 4: Add ListProjects and ListSessions to OpenCodeServerManager",
      "status": "pending",
      "priority": "high"
    },
    {
      "content": "Phase 5: Add POST /sessions/discover endpoint and daemon session loading",
      "status": "pending",
      "priority": "high"
    },
    {
      "content": "Phase 6: Lazy backend activation for historical sessions (messages + resume)",
      "status": "pending",
      "priority": "high"
    },
    {
      "content": "Phase 7: TUI calls discover on init",
      "status": "pending",
      "priority": "high"
    },
    {
      "content": "Phase 8: Register daemon subcommand on clank binary (bug fix)",
      "status": "pending",
      "priority": "medium"
    },
    {
      "content": "Phase 9: Tests",
      "status": "pending",
      "priority": "high"
    },
    {
      "content": "Phase 10: go mod tidy + verify build",
      "status": "pending",
      "priority": "high"
    }
  ]
}
```

**Output:**
```
[
  {
    "content": "Phase 1: Delete legacy code (scanner, store, analyzer, llm, context, plugin, legacy TUI views, legacy CLI commands)",
    "status": "pending",
    "priority": "high"
  },
  {
    "content": "Phase 2: Clean up config — remove ScanConfig, simplify configCmd",
    "status": "pending",
    "priority": "high"
  },
  {
    "content": "Phase 3: Add ExternalID field to SessionInfo",
    "status": "pending",
    "priority": "high"
  },
  {
    "content": "Phase 4: Add ListProjects and ListSessions to OpenCodeServerManager",
    "status": "pending",
    "priority": "high"
  },
  {
    "content": "Phase 5: Add POST /sessions/discover endpoint and daemon session loading",
    "status": "pending",
    "priority": "high"
  },
  {
    "content": "Phase 6: Lazy backend activation for historical sessions (messages + resume)",
    "status": "pending",
    "priority": "high"
  },
  {
    "content": "Phase 7: TUI calls discover on init",
    "status": "pending",
    "priority": "high"
  },
  {
    "content": "Phase 8: Register daemon subcommand on clank binary (bug fix)",
    "status": "pending",
    "priority": "medium"
  },
  {
    "content": "Phase 9: Tests",
    "status": "pending",
    "priority": "high"
  },
  {
    "content": "Phase 10: go mod tidy + verify build",
    "status": "pending",
    "priority": "high"
  }
]
```

---

## Assistant (Plan · claude-opus-4.6 · 92.8s)

## Final Plan

### Phase 1: Delete Legacy Code

**Delete entire directories:**
- `internal/scanner/` (scanner.go, opencode/opencode.go)
- `internal/store/` (store.go)
- `internal/analyzer/` (analyzer.go)
- `internal/llm/` (client.go)
- `internal/context/` (context.go)
- `plugin/` (index.ts, package.json)

**Delete legacy TUI views:**
- `internal/tui/sessions.go`
- `internal/tui/app.go`
- `internal/tui/list.go`
- `internal/tui/detail.go`
- `internal/tui/triage.go`

**Edit `internal/cli/clankcli/clankcli.go`:**
- Remove commands: `scanCmd`, `triageCmd`, `sessionsCmd`, `listCmd`, `showCmd`, `contextCmd`, `repoCmd`, `initCmd`, `backfillCmd`
- Remove helper functions: `openStore`, `newLLMClient`, `processSessions`, `truncStr`, `shortTyp`, `isModelForProvider`, `quadrantLabel`
- Remove imports: `database/sql`, `scanner`, `opencode`, `store`, `analyzer`, `llm`, `clankctx`, `huh`
- Keep: `codeCmd`, `inboxCmd`, `configCmd` (simplified), `runInbox`, `runComposing`, `ensureDaemon`

### Phase 2: Clean Up Config

**Edit `internal/config/config.go`:**
- Remove `ScanConfig` struct
- Remove `Scan ScanConfig` field from `Config`
- Remove `defaultOpenCodeDB()` function
- Remove `Scan` from `DefaultConfig()`

**Edit `configCmd` in `clankcli.go`:**
- Remove the "OpenCode DB Path" form field
- Remove `scan.opencode_db` from the `set` subcommand's switch
- Remove `OpenCode DB` line from `show` subcommand

### Phase 3: Add `ExternalID` to `SessionInfo`

**Edit `internal/agent/agent.go`:**
```go
type SessionInfo struct {
    // ... existing fields ...
    ExternalID  string        `json:"external_id,omitempty"` // Backend's native session ID (e.g. OpenCode session ID)
}
```

**Edit `internal/daemon/daemon.go`:**
- In `createSession()`: after `backend.Start()` completes (in the background goroutine `runBackend`), set `ms.info.ExternalID = backend.SessionID()`. This keeps the fast ULID response while populating the external ID as soon as it's available.
- Actually, since `Start()` is called in `runBackend` and the session is already registered before that, we need to update the info after `Start()` returns. Add to `runBackend()`:

```go
// After Start() returns, capture the backend's session ID.
d.mu.Lock()
if ms2, ok := d.sessions[id]; ok {
    ms2.info.ExternalID = ms.backend.SessionID()
}
d.mu.Unlock()
```

- In `handleGetSessionMessages` and `handleSendMessage`: when we need to talk to OpenCode about a session, we use `ms.info.ExternalID` (which is the OpenCode session ID the backend knows about).

For historical sessions loaded from `Session.List()`, the `ExternalID` is set at load time to the OpenCode session ID.

### Phase 4: Add `ListProjects` and `ListSessions` to `OpenCodeServerManager`

**Edit `internal/agent/opencode.go`:**

Add to `OpenCodeServerManager`:

```go
// ListProjects starts an OpenCode server for the given project directory
// and queries it for all known projects.
func (m *OpenCodeServerManager) ListProjects(ctx context.Context, projectDir string) ([]ProjectInfo, error) {
    serverURL, err := m.GetOrStartServer(ctx, projectDir)
    if err != nil {
        return nil, err
    }
    client := opencode.NewClient(option.WithBaseURL(serverURL))
    projects, err := client.Project.List(ctx, opencode.ProjectListParams{})
    if err != nil {
        return nil, fmt.Errorf("list projects: %w", err)
    }
    var result []ProjectInfo
    for _, p := range *projects {
        result = append(result, ProjectInfo{ID: p.ID, Worktree: p.Worktree})
    }
    return result, nil
}

// ListSessions starts an OpenCode server for the given project directory
// and returns all sessions for that project.
func (m *OpenCodeServerManager) ListSessions(ctx context.Context, projectDir string) ([]SessionSnapshot, error) {
    serverURL, err := m.GetOrStartServer(ctx, projectDir)
    if err != nil {
        return nil, err
    }
    client := opencode.NewClient(option.WithBaseURL(serverURL))
    sessions, err := client.Session.List(ctx, opencode.SessionListParams{})
    if err != nil {
        return nil, fmt.Errorf("list sessions: %w", err)
    }
    var result []SessionSnapshot
    for _, s := range *sessions {
        if s.ParentID != "" {
            continue // Skip child/forked sessions
        }
        result = append(result, SessionSnapshot{
            ID:        s.ID,
            Title:     s.Title,
            Directory: s.Directory,
            CreatedAt: time.Unix(int64(s.Time.Created), 0),
            UpdatedAt: time.Unix(int64(s.Time.Updated), 0),
        })
    }
    return result, nil
}
```

**Edit `internal/agent/agent.go`:**

Add new types:

```go
type ProjectInfo struct {
    ID       string `json:"id"`
    Worktree string `json:"worktree"`
}

// SessionSnapshot is a lightweight session summary from the OpenCode API,
// used during discovery to populate the daemon's session list.
type SessionSnapshot struct {
    ID        string    `json:"id"`
    Title     string    `json:"title"`
    Directory string    `json:"directory"`
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}
```

### Phase 5: Add `POST /sessions/discover` Endpoint

**Edit `internal/daemon/daemon.go`:**

Add a new function field to `Daemon`:

```go
// SessionDiscoverer discovers historical sessions for a project directory.
// It lists all projects from the OpenCode server, then lists all sessions
// for each project. Set by the caller before Run().
SessionDiscoverer func(ctx context.Context, projectDir string) ([]agent.SessionInfo, error)
```

Wire it up in `daemoncli.go` (RunStart foreground path):

```go
d.SessionDiscoverer = factory.DiscoverSessions
```

Add `DiscoverSessions` to `DefaultBackendFactory`:

```go
func (f *DefaultBackendFactory) DiscoverSessions(ctx context.Context, seedDir string) ([]agent.SessionInfo, error) {
    // 1. List all projects via the OpenCode server for seedDir
    projects, err := f.serverMgr.ListProjects(ctx, seedDir)
    if err != nil {
        return nil, err
    }
    // 2. For each project, list all sessions
    var allSessions []agent.SessionInfo
    for _, proj := range projects {
        sessions, err := f.serverMgr.ListSessions(ctx, proj.Worktree)
        if err != nil {
            continue // log and skip
        }
        for _, s := range sessions {
            allSessions = append(allSessions, agent.SessionInfo{
                ID:          ulid.Make().String(),
                ExternalID:  s.ID,
                Backend:     agent.BackendOpenCode,
                Status:      agent.StatusIdle,
                ProjectDir:  s.Directory,
                ProjectName: filepath.Base(s.Directory),
                Title:       s.Title,
                CreatedAt:   s.CreatedAt,
                UpdatedAt:   s.UpdatedAt,
                // LastReadAt left zero → shows as "unread" initially? 
                // Actually we should mark historical sessions as read.
                LastReadAt:  s.UpdatedAt, // Mark as read — they're not new
            })
        }
    }
    return allSessions, nil
}
```

Register the new route:

```go
mux.HandleFunc("POST /sessions/discover", d.handleDiscoverSessions)
```

Implement the handler:

```go
func (d *Daemon) handleDiscoverSessions(w http.ResponseWriter, r *http.Request) {
    var body struct {
        ProjectDir string `json:"project_dir"`
    }
    if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
        writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
        return
    }
    if body.ProjectDir == "" {
        writeJSON(w, http.StatusBadRequest, map[string]string{"error": "project_dir is required"})
        return
    }
    if d.SessionDiscoverer == nil {
        writeJSON(w, http.StatusOK, []agent.SessionInfo{})
        return
    }

    sessions, err := d.SessionDiscoverer(r.Context(), body.ProjectDir)
    if err != nil {
        writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
        return
    }

    // Register discovered sessions, skipping any whose ExternalID already exists
    // (i.e., sessions the daemon is already managing from a previous create or discover).
    added := 0
    d.mu.Lock()
    for _, info := range sessions {
        // Check if we already have this OpenCode session (by ExternalID).
        duplicate := false
        for _, existing := range d.sessions {
            if existing.info.ExternalID == info.ExternalID {
                duplicate = true
                break
            }
        }
        if duplicate {
            continue
        }
        d.sessions[info.ID] = &managedSession{info: info, backend: nil}
        added++
    }
    d.mu.Unlock()

    writeJSON(w, http.StatusOK, map[string]interface{}{
        "discovered": added,
        "total":      len(sessions),
    })
}
```

**Note on deduplication**: When the daemon creates a new session via `POST /sessions`, the backend's OpenCode session ID won't be known until `Start()` completes. So there's a brief window where discover could register a session that was just created. The `ExternalID` check handles this — once `runBackend` sets `ExternalID`, future discovers will skip it. For the race window, we could also skip sessions whose `UpdatedAt` is within the last few seconds, but that's an edge case we can address later.

**Note on performance**: The `duplicate` check is O(n) per session. For hundreds of sessions this is fine. If it becomes a problem later, we can add a `externalIDIndex map[string]string` to the daemon.

**Edit `internal/daemon/client.go`:**

Add client method:

```go
func (c *Client) DiscoverSessions(ctx context.Context, projectDir string) error {
    body := struct {
        ProjectDir string `json:"project_dir"`
    }{ProjectDir: projectDir}
    return c.do(ctx, "POST", "/sessions/discover", body, nil)
}
```

### Phase 6: Lazy Backend Activation for Historical Sessions

**Edit `internal/agent/opencode.go`:**

Add session ID parameter to the constructor:

```go
func NewOpenCodeBackend(serverURL string, sessionID string) *OpenCodeBackend {
    ctx, cancel := context.WithCancel(context.Background())
    client := opencode.NewClient(option.WithBaseURL(strings.TrimRight(serverURL, "/")))
    return &OpenCodeBackend{
        status:    StatusStarting,
        serverURL: strings.TrimRight(serverURL, "/"),
        sessionID: sessionID,  // empty for new sessions, set for resume/historical
        events:    make(chan Event, 128),
        ctx:       ctx,
        cancel:    cancel,
        client:    client,
    }
}
```

Update the callsite in `DefaultBackendFactory.Create()` to pass `req.SessionID`.

**Edit `internal/daemon/daemon.go`:**

Add `activateBackend` method:

```go
// activateBackend creates and attaches a backend to a historical session
// (one loaded via discover that has backend == nil). The backend is ready
// for Messages() calls but does NOT start SSE streaming or send a prompt.
func (d *Daemon) activateBackend(id string, ms *managedSession) error {
    backend, err := d.BackendFactory(ms.info.Backend, agent.StartRequest{
        Backend:    ms.info.Backend,
        ProjectDir: ms.info.ProjectDir,
        SessionID:  ms.info.ExternalID,
    })
    if err != nil {
        return fmt.Errorf("create backend: %w", err)
    }
    d.mu.Lock()
    ms.backend = backend
    d.mu.Unlock()
    return nil
}
```

**Edit `handleGetSessionMessages`:**

```go
func (d *Daemon) handleGetSessionMessages(w http.ResponseWriter, r *http.Request) {
    id := r.PathValue("id")
    d.mu.RLock()
    ms, ok := d.sessions[id]
    d.mu.RUnlock()
    if !ok {
        writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
        return
    }
    if ms.backend == nil {
        // Historical session — activate a read-only backend to fetch messages.
        if err := d.activateBackend(id, ms); err != nil {
            writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
            return
        }
    }
    messages, err := ms.backend.Messages(r.Context())
    // ... rest unchanged ...
}
```

This works because `Messages()` on `OpenCodeBackend` only needs `b.sessionID` and `b.client`, both of which are set by `NewOpenCodeBackend(serverURL, sessionID)`. No need to call `Start()`.

**Edit `handleSendMessage`:**

For resuming a historical session with a follow-up message, the existing flow almost works. When `backend == nil`:
1. `activateBackend()` creates the backend with the session ID set
2. Call `backend.Start()` with the follow-up prompt — but `Start()` currently starts SSE + sends prompt. For a resume, we want both.

Actually, `Start()` already handles resume: if `req.SessionID != ""`, it skips `Session.New()` and just starts SSE + sends the prompt. Since `activateBackend` already set `sessionID` via the constructor, `Start()` will see it's set and skip creation. Then it starts SSE and sends the prompt.

But we need to wire the event relay. When `handleSendMessage` activates a historical session, we need to start `runBackend` to relay events. So:

```go
func (d *Daemon) handleSendMessage(w http.ResponseWriter, r *http.Request) {
    // ... parse body, find session ...
    if ms.backend == nil {
        // Historical session — activate and start the backend.
        if err := d.activateBackend(id, ms); err != nil {
            writeJSON(w, http.StatusInternalServerError, ...)
            return
        }
        // Start the event relay + prompt dispatch in background.
        req := agent.StartRequest{
            Backend:    ms.info.Backend,
            ProjectDir: ms.info.ProjectDir,
            SessionID:  ms.info.ExternalID,
            Prompt:     body.Text,
            Agent:      body.Agent,
        }
        d.wg.Add(1)
        go func() {
            defer d.wg.Done()
            d.runBackend(id, ms, req)
        }()
        writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
        return
    }
    // ... existing flow for already-active sessions ...
}
```

### Phase 7: TUI Calls Discover on Init

**Edit `internal/tui/inbox.go`:**

In `Init()`, fire a discover command alongside the data load:

```go
func (m *InboxModel) Init() tea.Cmd {
    cmds := []tea.Cmd{
        func() tea.Msg { return tea.RequestWindowSize },
        m.discoverCmd(),
        m.loadDataCmd(),
        m.autoRefreshCmd(),
    }
    // ...
}
```

```go
func (m *InboxModel) discoverCmd() tea.Cmd {
    return func() tea.Msg {
        cwd, err := os.Getwd()
        if err != nil {
            return nil // Non-fatal
        }
        ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
        defer cancel()
        _ = m.client.DiscoverSessions(ctx, cwd)
        // After discovery, trigger a refresh to show the new sessions.
        return inboxRefreshMsg{}
    }
}
```

Discovery runs concurrently. The initial `loadDataCmd` may return an empty list (or only daemon-created sessions). Once `discoverCmd` finishes, it sends `inboxRefreshMsg` which triggers a reload, and now the historical sessions appear.

### Phase 8: (removed)

### Phase 9: Tests

**Daemon tests (`internal/daemon/daemon_test.go`):**
1. `TestDiscoverSessionsAddsHistoricalSessions` — mock `SessionDiscoverer`, call `POST /sessions/discover`, verify sessions appear in `GET /sessions`
2. `TestDiscoverSessionsDeduplicates` — call discover twice, verify no duplicates
3. `TestDiscoverSessionsSkipsManagedSessions` — create a session first, then discover; verify the created session isn't duplicated
4. `TestHistoricalSessionMessagesActivatesBackend` — load a historical session (backend=nil), call `GET /sessions/{id}/messages`, verify backend was created and messages returned
5. `TestHistoricalSessionResumeActivatesBackend` — load a historical session, call `POST /sessions/{id}/message`, verify backend starts with the correct external session ID and prompt

**Agent tests (`internal/agent/opencode_test.go`):**
6. `TestNewOpenCodeBackendWithSessionID` — verify that creating a backend with a session ID sets it correctly and `Messages()` works without calling `Start()`

**Update `mockBackend`** in daemon_test.go — add `ExternalID` support to match new `SessionID()` behavior.

### Phase 10: `go mod tidy` + Verify Build

- Run `go mod tidy` to remove unused dependencies (`langchaingo`, `modernc.org/sqlite`, Google Cloud deps, `huh`)
- Run `go build ./...` to verify everything compiles
- Run `go test ./...` to verify all tests pass

---

### File Change Summary

| File | Action |
|------|--------|
| `internal/scanner/scanner.go` | **DELETE** |
| `internal/scanner/opencode/opencode.go` | **DELETE** |
| `internal/store/store.go` | **DELETE** |
| `internal/analyzer/analyzer.go` | **DELETE** |
| `internal/llm/client.go` | **DELETE** |
| `internal/context/context.go` | **DELETE** |
| `plugin/index.ts` | **DELETE** |
| `plugin/package.json` | **DELETE** |
| `internal/tui/sessions.go` | **DELETE** |
| `internal/tui/app.go` | **DELETE** |
| `internal/tui/list.go` | **DELETE** |
| `internal/tui/detail.go` | **DELETE** |
| `internal/tui/triage.go` | **DELETE** |
| `internal/cli/clankcli/clankcli.go` | **EDIT** — remove 9 commands + helpers, simplify config, add daemon subcommand |
| `internal/config/config.go` | **EDIT** — remove `ScanConfig` |
| `internal/agent/agent.go` | **EDIT** — add `ExternalID`, `ProjectInfo`, `SessionSnapshot` |
| `internal/agent/opencode.go` | **EDIT** — add `ListProjects`, `ListSessions`, update `NewOpenCodeBackend` signature |
| `internal/daemon/daemon.go` | **EDIT** — add `SessionDiscoverer`, `handleDiscoverSessions`, `activateBackend`, update `handleGetSessionMessages` and `handleSendMessage` |
| `internal/daemon/client.go` | **EDIT** — add `DiscoverSessions` method |
| `internal/cli/daemoncli/daemoncli.go` | **EDIT** — wire `SessionDiscoverer` in `RunStart` |
| `internal/tui/inbox.go` | **EDIT** — add `discoverCmd` to `Init` |
| `internal/daemon/daemon_test.go` | **EDIT** — add 5 new tests, update `mockBackend` |
| `internal/agent/opencode_test.go` | **EDIT** — add test for constructor with session ID |
| `go.mod` / `go.sum` | **AUTO** — `go mod tidy` |

### Opportunities

- **ExternalID index**: The dedup check in `handleDiscoverSessions` is O(n*m). If session counts grow large, add a `map[string]string` index mapping external ID to daemon ID. Low priority for now.
- **Incremental discovery**: Currently `discover` reloads all sessions every time. Could cache which projects have been discovered and only refresh on explicit request. Fine for v1.
- **Discover on every server start**: Could hook into `GetOrStartServer` to auto-discover when a new project's server spins up, rather than requiring the TUI to call `POST /sessions/discover`. Nice-to-have for later.
