package hub

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/git"
	"github.com/acksell/clank/internal/host"
	"github.com/oklog/ulid/v2"
)

// HUB
// discoverSessions is the non-HTTP entrypoint for session discovery.
// Wire shape and semantics are owned by Service; mux only marshals.
func (s *Service) discoverSessions(ctx context.Context, projectDir string) (DiscoverResult, error) {
	// Fan out to every backend the host knows about and accumulate
	// snapshots. Discovery is best-effort per backend — failures are
	// logged but do not abort the whole call.
	var snapshots []agent.SessionSnapshot
	// Discovery currently targets only the local host. When multi-host
	// discovery lands this needs to fan out across s.snapshotHosts().
	h, err := s.hostFor("local")
	if err != nil {
		return DiscoverResult{}, err
	}
	backends, err := h.Backends(ctx)
	if err != nil {
		return DiscoverResult{}, err
	}
	for _, bi := range backends {
		found, err := h.Backend(bi.Name).Discover(ctx, projectDir)
		if err != nil {
			s.log.Printf("discover sessions (%s): %v", bi.Name, err)
			continue
		}
		snapshots = append(snapshots, found...)
	}
	if snapshots == nil {
		return DiscoverResult{}, nil
	}

	// Build a worktree path → branch name map so we can attribute discovered
	// sessions to the correct worktree. This lets the TUI filter sessions by
	// worktree directory (ProjectDir).
	wtPathToBranch := make(map[string]string)
	worktrees, err := git.ListWorktrees(projectDir)
	if err == nil {
		for _, wt := range worktrees {
			if !wt.Bare && wt.Branch != "" {
				wtPathToBranch[wt.Path] = wt.Branch
			}
		}
	}

	// Register discovered sessions, skipping any whose ExternalID already exists
	// (i.e., sessions the daemon is already managing from a previous create or discover).
	// Also check backend.SessionID() to catch sessions whose Start() is still
	// in progress (ExternalID not yet written back to info).
	//
	// For duplicates, refresh backend-owned fields (title, timestamps) from the
	// snapshot while preserving user-owned fields (visibility, follow_up, draft,
	// last_read_at) from the in-memory/DB copy.
	added := 0
	matchedExt := 0
	matchedSID := 0
	healed := 0
	s.mu.Lock()
	for _, snap := range snapshots {
		// Fast-fail rather than silently mis-tagging the session. A snapshot
		// without a Backend is a backend-manager bug; persisting it would
		// break activateBackend on restart (wrong manager dispatch).
		if snap.Backend == "" {
			s.log.Printf("discover: WARN snapshot extID=%s has empty Backend; skipping", snap.ID)
			continue
		}
		var existingMS *managedSession
		var matchKind string
		for _, existing := range s.sessions {
			if existing.info.ExternalID == snap.ID {
				existingMS = existing
				matchKind = "extID"
				break
			}
			if existing.backend != nil && existing.backend.SessionID() == snap.ID {
				existingMS = existing
				matchKind = "backendSID"
				break
			}
		}
		if existingMS != nil {
			if matchKind == "extID" {
				matchedExt++
			} else {
				matchedSID++
			}
			s.log.Printf("discover: snap extID=%s matched existing hub_id=%s via %s", snap.ID, existingMS.info.ID, matchKind)
			// Heal a mis-tagged backend (regression: prior versions of
			// discoverSessions hardcoded BackendOpenCode for every snapshot
			// regardless of source, leaving Claude sessions persisted as
			// opencode in sqlite. On reopen the host would dispatch to the
			// wrong backend manager and hang). Trust the snapshot — it came
			// straight from the backend that owns the session.
			if existingMS.info.Backend != snap.Backend {
				s.log.Printf("discover: HEAL hub_id=%s backend %s → %s (extID=%s)", existingMS.info.ID, existingMS.info.Backend, snap.Backend, snap.ID)
				existingMS.info.Backend = snap.Backend
				healed++
			}
			existingMS.info.Title = snap.Title
			existingMS.info.CreatedAt = snap.CreatedAt
			existingMS.info.UpdatedAt = snap.UpdatedAt
			existingMS.info.RevertMessageID = snap.RevertMessageID
			if existingMS.info.GitRef.WorktreeBranch == "" {
				if branch, ok := wtPathToBranch[snap.Directory]; ok {
					existingMS.info.GitRef.WorktreeBranch = branch
				}
			}
			if existingMS.backend == nil && (existingMS.info.Status == agent.StatusBusy || existingMS.info.Status == agent.StatusStarting || existingMS.info.Status == agent.StatusDead) {
				existingMS.info.Status = agent.StatusIdle
			}
			s.persistSession(existingMS)
			continue
		}

		var wtBranch string
		if branch, ok := wtPathToBranch[snap.Directory]; ok {
			wtBranch = branch
		}

		id := ulid.Make().String()
		// Derive a GitRef so lazy backend activation (activateBackend)
		// can reach the host plane. Snap directory is the host's local
		// path; remote URL is best-effort for cross-host identity.
		remoteURL, _ := git.RemoteURL(snap.Directory, "origin")
		gitRef := agent.GitRef{
			LocalPath:      snap.Directory,
			RemoteURL:      remoteURL,
			WorktreeBranch: wtBranch,
		}
		info := agent.SessionInfo{
			ID:              id,
			ExternalID:      snap.ID,
			Backend:         snap.Backend,
			Status:          agent.StatusIdle,
			GitRef:          gitRef,
			Title:           snap.Title,
			RevertMessageID: snap.RevertMessageID,
			CreatedAt:       snap.CreatedAt,
			UpdatedAt:       snap.UpdatedAt,
			LastReadAt:      snap.UpdatedAt, // mark as read — not new activity
		}
		s.sessions[id] = &managedSession{info: info, backend: nil}
		s.persistSession(s.sessions[id])
		s.log.Printf("discover: snap extID=%s NEW → hub_id=%s backend=%s dir=%s", snap.ID, id, snap.Backend, snap.Directory)
		added++
	}
	s.mu.Unlock()

	s.log.Printf("discover: %d snapshots, %d matched (extID=%d, backendSID=%d), %d added, %d healed", len(snapshots), matchedExt+matchedSID, matchedExt, matchedSID, added, healed)

	return DiscoverResult{Discovered: added, Total: len(snapshots)}, nil
}

// HOST
// activateBackend creates and attaches a backend to a historical session
// (one loaded via discover that has backend == nil). The backend is started
// via Watch() to enable SSE streaming without sending a prompt. An event
// relay goroutine is started so that events from the backend flow through
// the daemon's broadcast system.
func (s *Service) activateBackend(id string, ms *managedSession) error {
	h, err := s.hostFor(ms.info.Hostname)
	if err != nil {
		return err
	}
	backend, _, err := h.Sessions().Create(s.ctx, id, agent.StartRequest{
		Backend:   ms.info.Backend,
		Hostname:  ms.info.Hostname,
		GitRef:    ms.info.GitRef,
		SessionID: ms.info.ExternalID,
	})
	if err != nil {
		return fmt.Errorf("activate backend: %w", err)
	}

	// Open the backend without dispatching a prompt so SSE events
	// (for OpenCode) start flowing for this re-attached session.
	if err := backend.Open(s.ctx); err != nil {
		return fmt.Errorf("open backend: %w", err)
	}

	s.mu.Lock()
	ms.backend = backend
	ms.watchOnly = true
	s.mu.Unlock()

	// Start event relay goroutine so backend events flow through broadcast.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for evt := range backend.Events() {
			evt.SessionID = id
			s.broadcast(evt)

			if evt.Type == agent.EventStatusChange {
				if data, ok := evt.Data.(agent.StatusChangeData); ok {
					s.updateSessionStatus(id, data.NewStatus)
				}
			}
			if evt.Type == agent.EventTitleChange {
				if data, ok := evt.Data.(agent.TitleChangeData); ok {
					s.updateSessionTitle(id, data.Title)
				}
			}
			if evt.Type == agent.EventRevertChange {
				if data, ok := evt.Data.(agent.RevertChangeData); ok {
					s.updateSessionRevert(id, data.MessageID)
				}
			}
			if evt.Type == agent.EventPermission {
				if data, ok := evt.Data.(agent.PermissionData); ok {
					s.mu.Lock()
					ms.pendingPerms = append(ms.pendingPerms, data)
					s.mu.Unlock()
				}
			}
		}
	}()

	return nil
}

// HUB
// snapshotSessions returns a point-in-time copy of all session infos
// with live status from backends. ServerURL is per-session, populated
// at create time by the host (POST /sessions response) and stored on
// ms.info; not refreshed after daemon restart until a backend is
// reactivated via activateBackend.
func (s *Service) snapshotSessions() []agent.SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sessions := make([]agent.SessionInfo, 0, len(s.sessions))
	for _, ms := range s.sessions {
		info := ms.info
		if ms.backend != nil {
			info.Status = ms.backend.Status()
		}
		sessions = append(sessions, info)
	}
	return sessions
}

// HUB
// searchSessions returns sessions matching the given search parameters.
//
// Query supports pipe-separated OR groups: "auth bug|dark mode" matches
// sessions containing ("auth" AND "bug") OR ("dark" AND "mode"). All
// matching is case-insensitive substring matching against the concatenation
// of title, prompt, draft, and project_name.
//
// Since/Until filter on UpdatedAt. Results are sorted by updated_at descending.
func (s *Service) searchSessions(p agent.SearchParams) []agent.SessionInfo {
	// Parse OR groups from the query. Each group is a slice of AND terms.
	var orGroups [][]string
	if p.Query != "" {
		for _, group := range strings.Split(p.Query, "|") {
			terms := strings.Fields(strings.ToLower(strings.TrimSpace(group)))
			if len(terms) > 0 {
				orGroups = append(orGroups, terms)
			}
		}
	}

	hasQuery := len(orGroups) > 0
	hasSince := !p.Since.IsZero()
	hasUntil := !p.Until.IsZero()

	all := s.snapshotSessions()
	results := make([]agent.SessionInfo, 0)
	for _, si := range all {
		// Visibility filter.
		switch p.Visibility {
		case agent.VisibilityAll:
			// No filter — include everything.
		case agent.VisibilityDone, agent.VisibilityArchived:
			if si.Visibility != p.Visibility {
				continue
			}
		default:
			// Default ("") — active sessions only.
			if si.Hidden() {
				continue
			}
		}

		// Time filter.
		if hasSince && si.UpdatedAt.Before(p.Since) {
			continue
		}
		if hasUntil && !si.UpdatedAt.Before(p.Until) {
			continue
		}

		// Text filter: match if ANY OR group matches (all terms in the group present).
		if hasQuery {
			hay := strings.ToLower(si.Title + " " + si.Prompt + " " + si.Draft + " " + agent.RepoDisplayName(si.GitRef))
			matched := false
			for _, terms := range orGroups {
				allMatch := true
				for _, term := range terms {
					if !strings.Contains(hay, term) {
						allMatch = false
						break
					}
				}
				if allMatch {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		results = append(results, si)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].UpdatedAt.After(results[j].UpdatedAt)
	})

	return results
}

// HUB
// parseTimeParam parses a time parameter that is either an RFC 3339 timestamp
// or a relative duration suffix (e.g. "7d", "24h") interpreted as "ago from now".
// Supported units: h (hours), d (days).
func parseTimeParam(s string) (time.Time, error) {
	return agent.ParseTimeParam(s)
}

// HUB
// createSession creates a new managed session and starts the backend.
// Identity is path-free per §7: callers send (Hostname, GitRef,
// WorktreeBranch); the Host resolves a workDir on the way down and
// returns a per-session serverURL (empty for backends without an HTTP
// server, e.g. Claude Code).
func (s *Service) createSession(req agent.StartRequest) (*agent.SessionInfo, error) {
	// LaunchHost (if set) provisions a fresh sandbox host before
	// dispatching the session there. The launcher chooses the
	// hostname; we register the resulting client and overwrite
	// req.Hostname so the rest of the flow runs unchanged.
	//
	// When the request omits LaunchHost, fall back to the hub's
	// service-level default (set from preferences on cloud hubs).
	// This is what makes TUI-created sessions auto-spawn a Daytona
	// sandbox: the TUI doesn't know about launchers, but the cloud
	// hub does. Read defensively under launchersMu and copy the
	// pointed-to value so a concurrent SetDefaultLaunchHost can't
	// tear the spec we just captured.
	if req.LaunchHost == nil {
		s.launchersMu.RLock()
		if s.defaultLaunchHostSpec != nil {
			cp := *s.defaultLaunchHostSpec
			req.LaunchHost = &cp
		}
		s.launchersMu.RUnlock()
	}
	if req.LaunchHost != nil {
		launcher, err := s.hostLauncher(req.LaunchHost.Provider)
		if err != nil {
			return nil, err
		}
		name, client, err := launcher.Launch(s.ctx, *req.LaunchHost)
		if err != nil {
			return nil, fmt.Errorf("launch host: %w", err)
		}
		if _, regErr := s.RegisterHost(name, client); regErr != nil {
			_ = client.Close()
			return nil, fmt.Errorf("register launched host: %w", regErr)
		}
		req.Hostname = string(name)
	}

	if req.Hostname == "" {
		req.Hostname = string(host.HostLocal)
	}

	// Hub assigns the session ID up front, then asks the Host to create
	// and register a backend under it. The Host resolves
	// (GitRef, WorktreeBranch) → workDir internally and returns the
	// resolved server URL (OpenCode only) for per-session shell-out.
	id := ulid.Make().String()
	h, err := s.hostFor(req.Hostname)
	if err != nil {
		return nil, err
	}
	backend, serverURL, err := h.Sessions().Create(s.ctx, id, req)
	if err != nil {
		return nil, fmt.Errorf("create session backend: %w", err)
	}

	now := time.Now()

	sessInfo := agent.SessionInfo{
		ID:        id,
		Backend:   req.Backend,
		Status:    agent.StatusStarting,
		Hostname:  req.Hostname,
		GitRef:    req.GitRef,
		ServerURL: serverURL,
		Prompt:    req.Prompt,
		TicketID:  req.TicketID,
		Agent:     req.Agent,
		CreatedAt: now,
		UpdatedAt: now,
	}

	ms := &managedSession{
		info:    sessInfo,
		backend: backend,
	}

	s.mu.Lock()
	s.sessions[id] = ms
	s.persistSession(ms)
	s.mu.Unlock()

	// Broadcast session creation.
	s.broadcast(agent.Event{
		Type:      agent.EventSessionCreate,
		SessionID: id,
		Timestamp: now,
		Data:      sessInfo,
	})

	// Start the backend in a goroutine.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runBackend(id, ms, req)
	}()

	return &sessInfo, nil
}

// HOST
// runBackend starts the backend and relays its events.
func (s *Service) runBackend(id string, ms *managedSession, req agent.StartRequest) {
	// Start relaying events BEFORE calling Start(), because Start() blocks
	// for the entire LLM response (Prompt() is synchronous). Events emitted
	// by the backend's SSE goroutine during Start() must be relayed in real time.
	events := ms.backend.Events()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for evt := range events {
			evt.SessionID = id
			s.broadcast(evt)

			// Persist ExternalID the moment the backend learns it
			if evt.ExternalID != "" {
				s.mu.Lock()
				if ms2, ok := s.sessions[id]; ok && ms2.info.ExternalID != evt.ExternalID {
					ms2.info.ExternalID = evt.ExternalID
					s.persistSession(ms2)
				}
				s.mu.Unlock()
			}

			if evt.Type == agent.EventStatusChange {
				if data, ok := evt.Data.(agent.StatusChangeData); ok {
					s.updateSessionStatus(id, data.NewStatus)
				}
			}
			if evt.Type == agent.EventTitleChange {
				if data, ok := evt.Data.(agent.TitleChangeData); ok {
					s.updateSessionTitle(id, data.Title)
				}
			}
			if evt.Type == agent.EventRevertChange {
				if data, ok := evt.Data.(agent.RevertChangeData); ok {
					s.updateSessionRevert(id, data.MessageID)
				}
			}
			if evt.Type == agent.EventPermission {
				if data, ok := evt.Data.(agent.PermissionData); ok {
					s.mu.Lock()
					ms.pendingPerms = append(ms.pendingPerms, data)
					s.mu.Unlock()
				}
			}
		}
	}()
	defer func() { <-done }() // wait for relay goroutine to finish

	sendOpts := agent.SendMessageOpts{
		Text:  req.Prompt,
		Agent: req.Agent,
		Model: req.Model,
	}
	if err := ms.backend.OpenAndSend(s.ctx, sendOpts); err != nil {
		s.log.Printf("session %s: backend start error: %v", id, err)
		s.updateSessionStatus(id, agent.StatusError)
		s.broadcast(agent.Event{
			Type:      agent.EventError,
			SessionID: id,
			Timestamp: time.Now(),
			Data:      agent.ErrorData{Message: err.Error()},
		})
		return
	}

	// Backend event channel closed — mark as dead if still busy.
	s.mu.RLock()
	ms2, ok := s.sessions[id]
	s.mu.RUnlock()
	if ok && ms2.backend != nil {
		status := ms2.backend.Status()
		if status == agent.StatusBusy || status == agent.StatusStarting {
			s.updateSessionStatus(id, agent.StatusDead)
		}
	}
}

// HUB
// updateSessionStatus updates the cached status and UpdatedAt.
func (s *Service) updateSessionStatus(id string, status agent.SessionStatus) {
	s.mu.Lock()
	if ms, ok := s.sessions[id]; ok {
		ms.info.Status = status
		ms.info.UpdatedAt = time.Now()
		s.persistSession(ms)
	}
	s.mu.Unlock()
}

// HUB
// updateSessionTitle updates the cached title and UpdatedAt.
func (s *Service) updateSessionTitle(id string, title string) {
	s.mu.Lock()
	if ms, ok := s.sessions[id]; ok {
		ms.info.Title = title
		ms.info.UpdatedAt = time.Now()
		s.persistSession(ms)
	}
	s.mu.Unlock()
}

// HUB
// updateSessionRevert updates the cached revert message ID.
func (s *Service) updateSessionRevert(id string, messageID string) {
	s.mu.Lock()
	if ms, ok := s.sessions[id]; ok {
		ms.info.RevertMessageID = messageID
		s.persistSession(ms)
	}
	s.mu.Unlock()
}
