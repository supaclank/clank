package hub

// api.go is the public domain surface that internal/hub/mux consumes.
// Every method here is the *non-HTTP* version of what used to be a
// `handleX` on Service. Mux owns request decoding, response encoding,
// and status codes; this file owns the actual logic.
//
// Step 2 of the hub-host refactor (see hub_host_refactor_code_review.md
// §7.8) extracted handlers into internal/hub/mux/. Adding the surface
// here, rather than inlining it across the topical files, keeps step 2
// reviewable as an extraction-and-rename instead of a rewrite.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	"github.com/oklog/ulid/v2"
)

// ErrSessionNotFound is returned when a session ID is not in the registry.
var ErrSessionNotFound = errors.New("session not found")

// ErrNoActiveBackend is returned when an op requires a running backend
// but the session is in historical (backend == nil) state.
var ErrNoActiveBackend = errors.New("session has no active backend")

// ErrInvalidVisibility is returned when SetSessionVisibility receives a
// value outside the allowed enum.
var ErrInvalidVisibility = errors.New("invalid visibility")

// ErrInvalidRequest is returned for input-validation failures (mapped to 400).
var ErrInvalidRequest = errors.New("invalid request")

// ErrPartialActivation is returned by ForkSession when the new session
// row was persisted and broadcast successfully but the backend failed
// to activate. Callers receive a populated *SessionInfo alongside this
// error and should treat the operation as a partial success: the user
// can navigate to the session, but it has no live agent yet. Use
// errors.Is to detect; the wrapped chain carries the underlying
// activation failure for diagnostics.
var ErrPartialActivation = errors.New("session forked but backend activation failed")

// --- Sessions ---

// Sessions returns a point-in-time snapshot of all session metadata
// with backend-derived status overlaid where available.
func (s *Service) Sessions() []agent.SessionInfo { return s.snapshotSessions() }

// SearchSessions filters Sessions() by SearchParams. See the doc on
// searchSessions for query semantics (pipe = OR groups, space = AND
// terms, case-insensitive substring on title+prompt+draft+project_name).
func (s *Service) SearchSessions(p agent.SearchParams) []agent.SessionInfo {
	return s.searchSessions(p)
}

// CreateSession creates a managed session and starts the backend in
// the background. Returns the freshly-created SessionInfo with
// Status=Starting; status transitions arrive as events.
func (s *Service) CreateSession(ctx context.Context, req agent.StartRequest) (*agent.SessionInfo, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return s.createSession(req)
}

// GetSession returns a copy of the session metadata, with live backend
// status overlaid and OpenCode ServerURL populated when applicable.
func (s *Service) GetSession(id string) (*agent.SessionInfo, error) {
	s.mu.RLock()
	ms, ok := s.sessions[id]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrSessionNotFound
	}
	info := ms.info
	if ms.backend != nil {
		info.Status = ms.backend.Status()
	}
	return &info, nil
}

// SessionMessages returns the message history for a session. For
// historical (backend-less) sessions, lazily activates a read-only
// backend so the messages can be fetched.
func (s *Service) SessionMessages(ctx context.Context, id string) ([]agent.MessageData, error) {
	s.mu.RLock()
	ms, ok := s.sessions[id]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrSessionNotFound
	}
	if ms.backend == nil {
		if err := s.activateBackend(id, ms); err != nil {
			return nil, err
		}
	}
	msgs, err := ms.backend.Messages(ctx)
	if err != nil {
		return nil, err
	}
	if msgs == nil {
		msgs = []agent.MessageData{}
	}
	return msgs, nil
}

// SendMessageInput is the input for SendMessage. Carries no wire-only
// fields; mux owns the JSON shape.
type SendMessageInput struct {
	Text  string
	Agent string
	Model *agent.ModelOverride
}

// SendMessage sends a follow-up message to a session. For historical
// sessions, creates a fresh backend and dispatches the prompt. For
// active backends, dispatches asynchronously (returns once accepted;
// errors arrive via the event stream).
func (s *Service) SendMessage(ctx context.Context, id string, in SendMessageInput) error {
	if in.Text == "" {
		return fmt.Errorf("%w: text is required", ErrInvalidRequest)
	}
	s.mu.RLock()
	ms, ok := s.sessions[id]
	s.mu.RUnlock()
	if !ok {
		return ErrSessionNotFound
	}

	// Update the session's current agent if specified, clear any draft,
	// and reset visibility if the session was hidden (user re-engaging
	// means it's no longer done/archived).
	s.mu.Lock()
	if in.Agent != "" {
		ms.info.Agent = in.Agent
	}
	ms.info.Draft = ""
	if ms.info.Visibility == agent.VisibilityDone || ms.info.Visibility == agent.VisibilityArchived {
		ms.info.Visibility = agent.VisibilityVisible
	}
	s.persistSession(ms)
	s.mu.Unlock()

	if ms.backend == nil {
		req := agent.StartRequest{
			Backend:   ms.info.Backend,
			Hostname:  ms.info.Hostname,
			GitRef:    ms.info.GitRef,
			SessionID: ms.info.ExternalID,
			Prompt:    in.Text,
			Agent:     in.Agent,
			Model:     in.Model,
		}
		h, err := s.hostFor(req.Hostname)
		if err != nil {
			return err
		}
		backend, serverURL, err := h.Sessions().Create(s.ctx, id, req)
		if err != nil {
			return err
		}
		s.mu.Lock()
		ms.backend = backend
		ms.watchOnly = false
		// Refresh ServerURL on reactivation: the backend may have been
		// restarted on a new port since the previous session bound it.
		ms.info.ServerURL = serverURL
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.runBackend(id, ms, req)
		}()
		return nil
	}

	if ms.watchOnly {
		s.mu.Lock()
		ms.watchOnly = false
		s.mu.Unlock()
	}

	opts := agent.SendMessageOpts{
		Text:  in.Text,
		Agent: in.Agent,
		Model: in.Model,
	}

	// Dispatch asynchronously — backend.SendMessage blocks for the LLM
	// response. Errors arrive via the event stream as EventError.
	go func() {
		if err := ms.backend.Send(s.ctx, opts); err != nil {
			s.log.Printf("session %s: send message error: %v", id, err)
			s.broadcast(agent.Event{
				Type:      agent.EventError,
				SessionID: id,
				Timestamp: time.Now(),
				Data:      agent.ErrorData{Message: err.Error()},
			})
		}
	}()
	return nil
}

// AbortSession aborts an in-flight backend operation.
func (s *Service) AbortSession(ctx context.Context, id string) error {
	s.mu.RLock()
	ms, ok := s.sessions[id]
	s.mu.RUnlock()
	if !ok {
		return ErrSessionNotFound
	}
	if ms.backend == nil {
		return ErrNoActiveBackend
	}
	return ms.backend.Abort(ctx)
}

// RevertSession reverts the session to before the given message ID.
func (s *Service) RevertSession(ctx context.Context, id, messageID string) error {
	if messageID == "" {
		return fmt.Errorf("%w: message_id is required", ErrInvalidRequest)
	}
	s.mu.RLock()
	ms, ok := s.sessions[id]
	s.mu.RUnlock()
	if !ok {
		return ErrSessionNotFound
	}
	if ms.backend == nil {
		return ErrNoActiveBackend
	}
	if err := ms.backend.Revert(ctx, messageID); err != nil {
		return err
	}
	s.mu.Lock()
	ms.info.RevertMessageID = messageID
	s.persistSession(ms)
	s.mu.Unlock()
	return nil
}

// ForkSession forks a session at the given message ID (empty = fork
// the entire session). The forked session is registered and its
// backend activated so it can stream events and accept prompts.
func (s *Service) ForkSession(ctx context.Context, id, messageID string) (*agent.SessionInfo, error) {
	s.mu.RLock()
	ms, ok := s.sessions[id]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrSessionNotFound
	}
	if ms.backend == nil {
		return nil, ErrNoActiveBackend
	}

	forkResult, err := ms.backend.Fork(ctx, messageID)
	if err != nil {
		return nil, err
	}

	newID := ulid.Make().String()
	now := time.Now()
	newInfo := agent.SessionInfo{
		ID:         newID,
		ExternalID: forkResult.ID,
		Backend:    ms.info.Backend,
		Status:     agent.StatusIdle,
		Hostname:   ms.info.Hostname,
		GitRef:     ms.info.GitRef,
		ServerURL:  ms.info.ServerURL,
		Title:      forkResult.Title,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	newMS := &managedSession{info: newInfo}

	s.mu.Lock()
	s.sessions[newID] = newMS
	s.persistSession(newMS)
	s.mu.Unlock()

	if err := s.activateBackend(newID, newMS); err != nil {
		s.log.Printf("fork: failed to activate backend for %s: %v", newID, err)
		// Session is persisted and broadcast so the user can still
		// navigate to it, but callers must learn that activation
		// failed — silently returning success here causes the UI to
		// show a session that will never produce events. Wrap with
		// ErrPartialActivation so handlers can errors.Is-detect this
		// condition while still surfacing the underlying cause.
		s.broadcast(agent.Event{
			Type:      agent.EventSessionCreate,
			SessionID: newID,
			Timestamp: now,
			Data:      newInfo,
		})
		return &newInfo, fmt.Errorf("%w: %s: %w", ErrPartialActivation, newID, err)
	}

	s.broadcast(agent.Event{
		Type:      agent.EventSessionCreate,
		SessionID: newID,
		Timestamp: now,
		Data:      newInfo,
	})

	return &newInfo, nil
}

// MarkSessionRead updates LastReadAt to now.
func (s *Service) MarkSessionRead(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, ok := s.sessions[id]
	if !ok {
		return ErrSessionNotFound
	}
	ms.info.LastReadAt = time.Now()
	s.persistSession(ms)
	return nil
}

// ToggleSessionFollowUp flips the FollowUp flag and returns the new value.
func (s *Service) ToggleSessionFollowUp(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, ok := s.sessions[id]
	if !ok {
		return false, ErrSessionNotFound
	}
	ms.info.FollowUp = !ms.info.FollowUp
	s.persistSession(ms)
	return ms.info.FollowUp, nil
}

// SetSessionVisibility updates the session's visibility. Returns
// ErrInvalidVisibility for unknown values.
func (s *Service) SetSessionVisibility(id string, vis agent.SessionVisibility) error {
	switch vis {
	case agent.VisibilityVisible, agent.VisibilityDone, agent.VisibilityArchived:
	default:
		return fmt.Errorf("%w: %q", ErrInvalidVisibility, vis)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, ok := s.sessions[id]
	if !ok {
		return ErrSessionNotFound
	}
	ms.info.Visibility = vis
	s.persistSession(ms)
	return nil
}

// SetSessionDraft writes a draft message to a session.
func (s *Service) SetSessionDraft(id, draft string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, ok := s.sessions[id]
	if !ok {
		return ErrSessionNotFound
	}
	ms.info.Draft = draft
	s.persistSession(ms)
	return nil
}

// DeleteSession removes a session from the registry, the persistent
// store, and the host's backend registry. Broadcasts EventSessionDelete.
func (s *Service) DeleteSession(ctx context.Context, id string) error {
	s.mu.Lock()
	ms, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return ErrSessionNotFound
	}
	delete(s.sessions, id)
	s.deletePersistedSession(id)
	s.mu.Unlock()

	if ms.backend != nil {
		h, err := s.hostFor(ms.info.Hostname)
		if err != nil {
			s.log.Printf("error stopping session %s: %v", id, err)
		} else if err := h.Session(id).Stop(s.ctx); err != nil {
			s.log.Printf("error stopping session %s on host: %v", id, err)
		}
	}

	s.broadcast(agent.Event{
		Type:      agent.EventSessionDelete,
		SessionID: id,
		Timestamp: time.Now(),
	})
	return nil
}

// DiscoverResult is the return shape of DiscoverSessions.
type DiscoverResult struct {
	Discovered int
	Total      int
}

// DiscoverSessions walks every backend the host knows about, asks each
// for sessions in projectDir, and registers the new ones. Existing
// sessions get their backend-owned fields refreshed.
//
// Wire shape and semantics are unchanged from the legacy
// handleDiscoverSessions; this is just the non-HTTP entrypoint. The
// path-as-identity smell here will be removed in step 6/8.
func (s *Service) DiscoverSessions(ctx context.Context, projectDir string) (DiscoverResult, error) {
	if projectDir == "" {
		return DiscoverResult{}, fmt.Errorf("%w: project_dir is required", ErrInvalidRequest)
	}
	return s.discoverSessions(ctx, projectDir)
}

// --- Permissions ---

// RespondPermission resolves a pending permission prompt with allow/deny.
func (s *Service) RespondPermission(ctx context.Context, sessionID, permID string, allow bool) error {
	s.mu.RLock()
	ms, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if !ok {
		return ErrSessionNotFound
	}
	if ms.backend == nil {
		return ErrNoActiveBackend
	}
	if err := ms.backend.RespondPermission(ctx, permID, allow); err != nil {
		return err
	}
	s.mu.Lock()
	if allow {
		filtered := ms.pendingPerms[:0]
		for _, p := range ms.pendingPerms {
			if p.RequestID != permID {
				filtered = append(filtered, p)
			}
		}
		ms.pendingPerms = filtered
	} else {
		// OpenCode cancels the remaining batch on rejection; mirror that.
		ms.pendingPerms = nil
	}
	s.mu.Unlock()
	return nil
}

// PendingPermissions returns a copy of the pending permission queue.
func (s *Service) PendingPermissions(sessionID string) ([]agent.PermissionData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ms, ok := s.sessions[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}
	out := make([]agent.PermissionData, len(ms.pendingPerms))
	copy(out, ms.pendingPerms)
	return out, nil
}

// --- Agents / Models ---

// ListAgents returns the primary-agent list for (backend, hostname, gitRef).
// Serves from cache when available and refreshes in the background;
// falls back to a synchronous host call on cache miss.
func (s *Service) ListAgents(ctx context.Context, bt agent.BackendType, hostname host.Hostname, ref agent.GitRef) ([]agent.AgentInfo, error) {
	if s.Store != nil {
		cached, err := s.Store.LoadPrimaryAgents(bt, string(hostname), ref)
		if err != nil {
			s.log.Printf("warning: load cached primary agents: %v", err)
		}
		if cached != nil {
			s.refreshPrimaryAgentsInBackground(bt, hostname, ref)
			return cached, nil
		}
	}
	hc, ok := s.Host(hostname)
	if !ok {
		return nil, ErrHostNotRegistered(hostname)
	}
	agents, err := hc.Backend(bt).Agents(ctx, ref)
	if err != nil {
		return nil, err
	}
	if agents == nil {
		agents = []agent.AgentInfo{}
	}
	s.persistPrimaryAgents(bt, hostname, ref, agents)
	return agents, nil
}

// ListModels returns available models for (backend, hostname, gitRef).
func (s *Service) ListModels(ctx context.Context, bt agent.BackendType, hostname host.Hostname, ref agent.GitRef) ([]agent.ModelInfo, error) {
	hc, ok := s.Host(hostname)
	if !ok {
		return nil, ErrHostNotRegistered(hostname)
	}
	models, err := hc.Backend(bt).Models(ctx, ref)
	if err != nil {
		return nil, err
	}
	if models == nil {
		models = []agent.ModelInfo{}
	}
	return models, nil
}

// --- Worktrees (host pass-throughs) ---

// HostExists returns whether a host with the given ID is registered.
func (s *Service) HostExists(id host.Hostname) bool {
	_, ok := s.Host(id)
	return ok
}

// ListBranchesOnHost lists branches/worktrees for a repo on the named
// host. The repo is identified by its GitRef (Local or Remote);
// WorktreeBranch on ref is ignored — branches enumerate the whole repo.
func (s *Service) ListBranchesOnHost(ctx context.Context, hostname host.Hostname, ref agent.GitRef) ([]host.BranchInfo, error) {
	hc, ok := s.Host(hostname)
	if !ok {
		return nil, ErrHostNotRegistered(hostname)
	}
	return hc.ListBranches(ctx, ref)
}

// ResolveWorktreeOnHost creates (or resolves) a worktree for branch.
func (s *Service) ResolveWorktreeOnHost(ctx context.Context, hostname host.Hostname, ref agent.GitRef, branch string) (host.WorktreeInfo, error) {
	hc, ok := s.Host(hostname)
	if !ok {
		return host.WorktreeInfo{}, ErrHostNotRegistered(hostname)
	}
	return hc.ResolveWorktree(ctx, ref, branch)
}

// RemoveWorktreeOnHost removes a worktree.
func (s *Service) RemoveWorktreeOnHost(ctx context.Context, hostname host.Hostname, ref agent.GitRef, branch string, force bool) error {
	hc, ok := s.Host(hostname)
	if !ok {
		return ErrHostNotRegistered(hostname)
	}
	return hc.RemoveWorktree(ctx, ref, branch, force)
}

// MergeBranchOnHost merges branch into the repo's default branch and
// marks attached hub-side sessions as done.
func (s *Service) MergeBranchOnHost(ctx context.Context, hostname host.Hostname, ref agent.GitRef, branch, commitMessage string) (host.MergeResult, error) {
	hc, ok := s.Host(hostname)
	if !ok {
		return host.MergeResult{}, ErrHostNotRegistered(hostname)
	}
	res, err := hc.MergeBranch(ctx, ref, branch, commitMessage)
	if err != nil {
		return host.MergeResult{}, err
	}
	s.markBranchSessionsDone(ref, branch)
	return res, nil
}

// --- Auth (host pass-throughs) ---

// ListAuthProvidersOnHost returns the auth providers known to the
// named host plus their current connection state.
func (s *Service) ListAuthProvidersOnHost(ctx context.Context, hostname host.Hostname) ([]agent.ProviderAuthInfo, error) {
	hc, ok := s.Host(hostname)
	if !ok {
		return nil, ErrHostNotRegistered(hostname)
	}
	return hc.ListAuthProviders(ctx)
}

// StartAuthDeviceFlowOnHost kicks off device-flow auth for providerID
// on the named host, returning the user-facing fields the TUI shows.
func (s *Service) StartAuthDeviceFlowOnHost(ctx context.Context, hostname host.Hostname, providerID string) (agent.DeviceFlowStart, error) {
	hc, ok := s.Host(hostname)
	if !ok {
		return agent.DeviceFlowStart{}, ErrHostNotRegistered(hostname)
	}
	return hc.StartDeviceFlow(ctx, providerID)
}

// SubmitAuthAPIKeyOnHost stores an API key (and any provider-specific
// metadata) for providerID on the named host and returns a flow_id
// the client polls until the post-write OpenCode restart finishes.
func (s *Service) SubmitAuthAPIKeyOnHost(ctx context.Context, hostname host.Hostname, providerID, key string, metadata map[string]string) (agent.DeviceFlowStart, error) {
	hc, ok := s.Host(hostname)
	if !ok {
		return agent.DeviceFlowStart{}, ErrHostNotRegistered(hostname)
	}
	return hc.SubmitAPIKey(ctx, providerID, key, metadata)
}

// AuthFlowStatusOnHost returns the current state of an in-progress
// flow on the named host. Works for both device-flow and api-key
// flows (the flow_id is opaque). Pure read.
func (s *Service) AuthFlowStatusOnHost(ctx context.Context, hostname host.Hostname, providerID, flowID string) (agent.DeviceFlowStatus, error) {
	hc, ok := s.Host(hostname)
	if !ok {
		return agent.DeviceFlowStatus{}, ErrHostNotRegistered(hostname)
	}
	return hc.FlowStatus(ctx, providerID, flowID)
}

// CancelAuthFlowOnHost aborts an in-progress flow.
func (s *Service) CancelAuthFlowOnHost(ctx context.Context, hostname host.Hostname, providerID, flowID string) error {
	hc, ok := s.Host(hostname)
	if !ok {
		return ErrHostNotRegistered(hostname)
	}
	return hc.CancelFlow(ctx, providerID, flowID)
}

// DeleteAuthCredentialOnHost removes the stored credential and
// triggers an OpenCode restart on the named host.
func (s *Service) DeleteAuthCredentialOnHost(ctx context.Context, hostname host.Hostname, providerID string) error {
	hc, ok := s.Host(hostname)
	if !ok {
		return ErrHostNotRegistered(hostname)
	}
	return hc.DeleteAuthCredential(ctx, providerID)
}

// --- Hosts ---

// ErrHostNotRegisteredErr is the typed error for unknown hostname lookups.
type ErrHostNotRegisteredErr struct{ ID host.Hostname }

func (e ErrHostNotRegisteredErr) Error() string {
	return fmt.Sprintf("host not registered: %s", e.ID)
}

// ErrHostNotRegistered constructs ErrHostNotRegisteredErr.
func ErrHostNotRegistered(id host.Hostname) error { return ErrHostNotRegisteredErr{ID: id} }

// --- Events ---

// Subscribe registers a new event subscriber. Returns the subscriber ID,
// the receive channel, and an unsubscribe function. The channel is
// buffered; broadcast drops events for slow subscribers rather than
// blocking.
func (s *Service) Subscribe() (string, <-chan agent.Event, func()) {
	subID := ulid.Make().String()
	ch := make(chan agent.Event, 64)
	s.subMu.Lock()
	s.subscribers[subID] = ch
	s.subMu.Unlock()
	unsub := func() {
		s.subMu.Lock()
		if existing, ok := s.subscribers[subID]; ok && existing == ch {
			delete(s.subscribers, subID)
		}
		s.subMu.Unlock()
	}
	return subID, ch, unsub
}

// ShutdownContext returns the daemon's long-lived context, used by SSE
// handlers to detect process-wide shutdown in addition to the
// per-request context.
func (s *Service) ShutdownContext() context.Context { return s.ctx }

// StartTime returns the moment New() was called. Used by /ping and /status.
func (s *Service) StartTime() time.Time { return s.startTime }

// --- Voice ---

// VoiceStatus returns whether a voice session is active and its current
// state. The websocket handler itself stays on Service (see
// HandleVoiceAudio) because it owns Service-internal state and a long-
// lived websocket.
func (s *Service) VoiceStatus() (active bool, status agent.VoiceStatus) {
	s.mu.RLock()
	sess := s.voice
	s.mu.RUnlock()
	if sess == nil {
		return false, agent.VoiceStatusIdle
	}
	return true, sess.Status()
}
