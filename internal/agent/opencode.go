package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	opencode "github.com/sst/opencode-sdk-go"
	"github.com/sst/opencode-sdk-go/option"
)

// OpenCodeBackend manages a single OpenCode session via the OpenCode Go SDK.
//
// Architecture:
//   - The daemon manages one OpenCode server per project directory
//     (via OpenCodeServerManager). The backend receives the server URL.
//   - Each backend instance corresponds to one session on that server.
//   - Events are streamed via SSE from GET /event on the server (using SDK's ListStreaming).
type OpenCodeBackend struct {
	mu           sync.Mutex
	status       SessionStatus
	sessionID    string // OpenCode's session ID (assigned by server)
	serverURL    string // e.g. "http://127.0.0.1:4123"
	events       chan Event
	eventOnce    sync.Once // protects close(events)
	eventsClosed bool      // true after events channel is closed
	watchOnce    sync.Once // ensures streamEvents is started at most once
	ctx          context.Context
	cancel       context.CancelFunc
	client       *opencode.Client

	// messageRoles tracks message ID -> role so we can skip user part updates.
	messageRoles sync.Map // map[string]opencode.MessageRole
}

// NewOpenCodeBackend creates a new OpenCode backend that communicates with
// an already-running OpenCode server at the given URL. If sessionID is
// non-empty, the backend is pre-associated with that session (used for
// historical/resume sessions loaded from discover).
func NewOpenCodeBackend(serverURL string, sessionID string) *OpenCodeBackend {
	ctx, cancel := context.WithCancel(context.Background())
	client := opencode.NewClient(option.WithBaseURL(strings.TrimRight(serverURL, "/")))
	return &OpenCodeBackend{
		status:    StatusIdle,
		serverURL: strings.TrimRight(serverURL, "/"),
		sessionID: sessionID,
		events:    make(chan Event, 128),
		ctx:       ctx,
		cancel:    cancel,
		client:    client,
	}
}

func (b *OpenCodeBackend) Start(ctx context.Context, req StartRequest) error {
	if req.SessionID != "" {
		// Resume existing session.
		b.mu.Lock()
		b.sessionID = req.SessionID
		b.mu.Unlock()
	}

	if b.SessionID() == "" {
		// Create new session via SDK.
		session, err := b.client.Session.New(ctx, opencode.SessionNewParams{})
		if err != nil {
			return fmt.Errorf("create session: %w", err)
		}
		b.mu.Lock()
		b.sessionID = session.ID
		b.mu.Unlock()
	}

	// Start SSE event listener in background (idempotent — skips if already watching).
	b.startWatching()

	// Mark as busy BEFORE sending the prompt, so the TUI shows the correct
	// status while the agent is working.
	b.setStatus(StatusBusy)

	// Send the initial prompt via SDK. This blocks until the full response
	// is received, but events stream in via SSE in the background.
	params := opencode.SessionPromptParams{
		Parts: opencode.F([]opencode.SessionPromptParamsPartUnion{
			opencode.TextPartInputParam{
				Text: opencode.F(req.Prompt),
				Type: opencode.F(opencode.TextPartInputTypeText),
			},
		}),
	}
	if req.Agent != "" {
		params.Agent = opencode.F(req.Agent)
	}
	_, err := b.client.Session.Prompt(ctx, b.sessionID, params)
	if err != nil {
		b.setStatus(StatusError)
		return fmt.Errorf("send prompt: %w", err)
	}

	// Don't set status here — the SSE stream will deliver session.idle
	// which triggers the idle transition.
	return nil
}

// Watch starts listening for SSE events on this session without sending a
// prompt. Use this to observe a discovered/historical session that may be
// active. The Events channel will produce events after Watch returns.
func (b *OpenCodeBackend) Watch(ctx context.Context) error {
	if b.SessionID() == "" {
		return fmt.Errorf("cannot watch: session ID not set")
	}
	b.startWatching()
	return nil
}

// startWatching launches the SSE event listener goroutine if not already running.
func (b *OpenCodeBackend) startWatching() {
	b.watchOnce.Do(func() {
		go b.streamEvents()
	})
}

func (b *OpenCodeBackend) SendMessage(ctx context.Context, opts SendMessageOpts) error {
	if b.sessionID == "" {
		return fmt.Errorf("session not started")
	}

	// Mark as busy BEFORE sending, so the TUI shows the correct status.
	b.setStatus(StatusBusy)

	params := opencode.SessionPromptParams{
		Parts: opencode.F([]opencode.SessionPromptParamsPartUnion{
			opencode.TextPartInputParam{
				Text: opencode.F(opts.Text),
				Type: opencode.F(opencode.TextPartInputTypeText),
			},
		}),
	}
	if opts.Agent != "" {
		params.Agent = opencode.F(opts.Agent)
	}
	_, err := b.client.Session.Prompt(ctx, b.sessionID, params)
	if err != nil {
		b.setStatus(StatusError)
		return fmt.Errorf("send message: %w", err)
	}
	// Don't set status here — SSE session.idle will trigger the transition.
	return nil
}

func (b *OpenCodeBackend) Abort(ctx context.Context) error {
	if b.sessionID == "" {
		return fmt.Errorf("session not started")
	}
	_, err := b.client.Session.Abort(ctx, b.sessionID, opencode.SessionAbortParams{})
	if err != nil {
		return fmt.Errorf("abort: %w", err)
	}
	return nil
}

func (b *OpenCodeBackend) Stop() error {
	b.cancel()
	b.closeEvents()
	return nil
}

func (b *OpenCodeBackend) Events() <-chan Event {
	return b.events
}

func (b *OpenCodeBackend) Status() SessionStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

func (b *OpenCodeBackend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

func (b *OpenCodeBackend) RespondPermission(ctx context.Context, permissionID string, allow bool) error {
	if b.sessionID == "" {
		return fmt.Errorf("session not started")
	}
	response := opencode.SessionPermissionRespondParamsResponseOnce
	if !allow {
		response = opencode.SessionPermissionRespondParamsResponseReject
	}
	_, err := b.client.Session.Permissions.Respond(ctx, b.sessionID, permissionID, opencode.SessionPermissionRespondParams{
		Response: opencode.F(response),
	})
	if err != nil {
		return fmt.Errorf("respond permission: %w", err)
	}
	return nil
}

func (b *OpenCodeBackend) setStatus(s SessionStatus) {
	b.mu.Lock()
	old := b.status
	b.status = s
	b.mu.Unlock()

	if old != s {
		b.emit(Event{
			Type:      EventStatusChange,
			Timestamp: time.Now(),
			Data: StatusChangeData{
				OldStatus: old,
				NewStatus: s,
			},
		})
	}
}

func (b *OpenCodeBackend) closeEvents() {
	b.eventOnce.Do(func() {
		b.mu.Lock()
		b.eventsClosed = true
		b.mu.Unlock()
		close(b.events)
	})
}

func (b *OpenCodeBackend) emit(evt Event) {
	// Recover from send-on-closed-channel if the SSE stream goroutine
	// closed the events channel between our check and the send.
	defer func() { recover() }()

	b.mu.Lock()
	closed := b.eventsClosed
	b.mu.Unlock()
	if closed {
		return
	}
	select {
	case b.events <- evt:
	default:
		// Drop if full.
	}
}

// streamEvents connects to the OpenCode SSE endpoint via the SDK and translates events.
func (b *OpenCodeBackend) streamEvents() {
	defer b.closeEvents()

	stream := b.client.Event.ListStreaming(b.ctx, opencode.EventListParams{})
	defer stream.Close()

	for stream.Next() {
		event := stream.Current()
		b.handleSDKEvent(event)
	}

	// If stream ended due to error (and not intentional cancellation), emit error.
	if err := stream.Err(); err != nil {
		// Don't emit error if we're shutting down.
		select {
		case <-b.ctx.Done():
			return
		default:
			b.emitError("event stream: " + err.Error())
		}
	}
}

// handleSDKEvent translates an OpenCode SDK event into our unified Event type.
func (b *OpenCodeBackend) handleSDKEvent(event opencode.EventListResponse) {
	switch e := event.AsUnion().(type) {
	case opencode.EventListResponseEventSessionIdle:
		if e.Properties.SessionID != b.sessionID {
			return
		}
		b.setStatus(StatusIdle)

	case opencode.EventListResponseEventSessionError:
		if e.Properties.SessionID != "" && e.Properties.SessionID != b.sessionID {
			return
		}
		b.setStatus(StatusError)
		b.emitError(string(e.Properties.Error.Name))

	case opencode.EventListResponseEventMessageUpdated:
		msg := e.Properties.Info
		if msg.SessionID != b.sessionID {
			return
		}
		// Track message ID -> role so we can filter user parts.
		b.messageRoles.Store(msg.ID, msg.Role)

		// Skip user messages — the TUI already shows the user's prompt
		// from the initial request, and follow-ups from the input box.
		if msg.Role == opencode.MessageRoleUser {
			return
		}

		b.emit(Event{
			Type:      EventMessage,
			Timestamp: time.Now(),
			Data: MessageData{
				Role: string(msg.Role),
			},
		})

	case opencode.EventListResponseEventMessagePartUpdated:
		part := e.Properties.Part
		delta := e.Properties.Delta

		// Filter to our session.
		if part.SessionID != b.sessionID {
			return
		}

		// Skip parts belonging to user messages.
		if role, ok := b.messageRoles.Load(part.MessageID); ok {
			if role == opencode.MessageRoleUser {
				return
			}
		}

		// If there's a delta, emit it as a text part update (streaming text).
		if delta != "" {
			b.emit(Event{
				Type:      EventPartUpdate,
				Timestamp: time.Now(),
				Data: PartUpdateData{
					Part: Part{
						ID:   part.ID,
						Type: PartText,
						Text: delta,
					},
				},
			})
			return
		}

		// Handle full part update based on part type.
		b.handlePartUpdate(part)

	case opencode.EventListResponseEventPermissionUpdated:
		perm := e.Properties
		if perm.SessionID != b.sessionID {
			return
		}
		b.emit(Event{
			Type:      EventPermission,
			Timestamp: time.Now(),
			Data: PermissionData{
				RequestID:   perm.ID,
				Tool:        perm.Type,
				Description: perm.Title,
			},
		})

	case opencode.EventListResponseEventSessionUpdated:
		sess := e.Properties.Info
		if sess.ID != b.sessionID {
			return
		}
		// Emit a title change event when the session title is updated
		// (e.g. by OpenCode's hidden "title" agent after the first response).
		if sess.Title != "" {
			b.emit(Event{
				Type:      EventTitleChange,
				Timestamp: time.Now(),
				Data: TitleChangeData{
					Title: sess.Title,
				},
			})
		}
	}
}

// handlePartUpdate processes a full Part update from the SDK.
func (b *OpenCodeBackend) handlePartUpdate(p opencode.Part) {
	switch concrete := p.AsUnion().(type) {
	case opencode.TextPart:
		b.emit(Event{
			Type:      EventPartUpdate,
			Timestamp: time.Now(),
			Data: PartUpdateData{
				Part: Part{
					ID:   concrete.ID,
					Type: PartText,
					Text: concrete.Text,
				},
			},
		})

	case opencode.ToolPart:
		var partStatus PartStatus
		switch concrete.State.Status {
		case opencode.ToolPartStateStatusPending:
			partStatus = PartPending
		case opencode.ToolPartStateStatusRunning:
			partStatus = PartRunning
		case opencode.ToolPartStateStatusCompleted:
			partStatus = PartCompleted
		case opencode.ToolPartStateStatusError:
			partStatus = PartFailed
		}
		b.emit(Event{
			Type:      EventPartUpdate,
			Timestamp: time.Now(),
			Data: PartUpdateData{
				Part: Part{
					ID:     concrete.ID,
					Type:   PartToolCall,
					Tool:   concrete.Tool,
					Status: partStatus,
				},
			},
		})

	case opencode.ReasoningPart:
		b.emit(Event{
			Type:      EventPartUpdate,
			Timestamp: time.Now(),
			Data: PartUpdateData{
				Part: Part{
					ID:   concrete.ID,
					Type: PartThinking,
					Text: concrete.Text,
				},
			},
		})

	default:
		// Skip types we don't care about (StepStartPart, StepFinishPart, etc.).
	}
}

// Messages returns the full message history for this session by calling
// the OpenCode SDK's Session.Messages API and translating to our types.
func (b *OpenCodeBackend) Messages(ctx context.Context) ([]MessageData, error) {
	if b.sessionID == "" {
		return nil, fmt.Errorf("session not started")
	}

	resp, err := b.client.Session.Messages(ctx, b.sessionID, opencode.SessionMessagesParams{})
	if err != nil {
		return nil, fmt.Errorf("fetch messages: %w", err)
	}
	if resp == nil {
		return nil, nil
	}

	var messages []MessageData
	for _, msg := range *resp {
		md := MessageData{
			Role: string(msg.Info.Role),
		}

		for _, p := range msg.Parts {
			converted := b.convertSDKPart(p)
			if converted != nil {
				md.Parts = append(md.Parts, *converted)
			}
		}

		messages = append(messages, md)
	}
	return messages, nil
}

// convertSDKPart translates an OpenCode SDK Part into our Part type.
// Returns nil for part types we don't display (StepStart, StepFinish, etc.).
func (b *OpenCodeBackend) convertSDKPart(p opencode.Part) *Part {
	switch concrete := p.AsUnion().(type) {
	case opencode.TextPart:
		return &Part{
			ID:   concrete.ID,
			Type: PartText,
			Text: concrete.Text,
		}
	case opencode.ToolPart:
		var partStatus PartStatus
		switch concrete.State.Status {
		case opencode.ToolPartStateStatusPending:
			partStatus = PartPending
		case opencode.ToolPartStateStatusRunning:
			partStatus = PartRunning
		case opencode.ToolPartStateStatusCompleted:
			partStatus = PartCompleted
		case opencode.ToolPartStateStatusError:
			partStatus = PartFailed
		}
		return &Part{
			ID:     concrete.ID,
			Type:   PartToolCall,
			Tool:   concrete.Tool,
			Status: partStatus,
		}
	case opencode.ReasoningPart:
		return &Part{
			ID:   concrete.ID,
			Type: PartThinking,
			Text: concrete.Text,
		}
	default:
		return nil
	}
}

func (b *OpenCodeBackend) emitError(msg string) {
	b.emit(Event{
		Type:      EventError,
		Timestamp: time.Now(),
		Data:      ErrorData{Message: msg},
	})
}

// --- OpenCode Server Manager ---

// OpenCodeServer tracks a running `opencode serve` process for a project.
type OpenCodeServer struct {
	URL        string
	ProjectDir string
	Cmd        *exec.Cmd
	StartedAt  time.Time
}

// OpenCodeServerManager manages one OpenCode server per project directory.
// The daemon uses this to reuse servers across sessions for the same project.
type OpenCodeServerManager struct {
	mu      sync.Mutex
	servers map[string]*OpenCodeServer // keyed by project dir
	agents  map[string][]AgentInfo     // cached agents per server URL
}

// NewOpenCodeServerManager creates a new server manager.
func NewOpenCodeServerManager() *OpenCodeServerManager {
	return &OpenCodeServerManager{
		servers: make(map[string]*OpenCodeServer),
		agents:  make(map[string][]AgentInfo),
	}
}

// GetOrStartServer returns a running server URL for the given project dir.
// If no server is running, it starts one.
func (m *OpenCodeServerManager) GetOrStartServer(ctx context.Context, projectDir string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if we already have a server for this project.
	if srv, ok := m.servers[projectDir]; ok {
		// Verify it's still alive with a health check.
		if m.healthCheck(srv.URL) {
			return srv.URL, nil
		}
		// Dead server — clean up and start a new one.
		srv.Cmd.Process.Kill()
		delete(m.servers, projectDir)
	}

	// Start a new server.
	srv, err := m.startServer(ctx, projectDir)
	if err != nil {
		return "", err
	}
	m.servers[projectDir] = srv
	return srv.URL, nil
}

// StopAll stops all managed servers.
func (m *OpenCodeServerManager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for dir, srv := range m.servers {
		if srv.Cmd.Process != nil {
			srv.Cmd.Process.Signal(os.Interrupt)
			// Give it a moment to clean up.
			done := make(chan error, 1)
			go func() { done <- srv.Cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				srv.Cmd.Process.Kill()
			}
		}
		delete(m.servers, dir)
	}
}

// startServer spawns `opencode serve` and waits for it to print the listening URL.
func (m *OpenCodeServerManager) startServer(ctx context.Context, projectDir string) (*OpenCodeServer, error) {
	cmd := exec.CommandContext(ctx, "opencode", "serve", "--port=0")
	cmd.Dir = projectDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr // Let stderr pass through for debugging.

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start opencode serve: %w", err)
	}

	// Parse stdout for the listening URL.
	// OpenCode prints: "opencode server listening on http://127.0.0.1:<port>"
	urlCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if idx := strings.Index(line, "http://"); idx >= 0 {
				// Extract URL from the line.
				url := line[idx:]
				// Trim any trailing whitespace or control chars.
				url = strings.TrimSpace(url)
				urlCh <- url
				return
			}
		}
		close(urlCh)
	}()

	select {
	case url, ok := <-urlCh:
		if !ok || url == "" {
			cmd.Process.Kill()
			return nil, fmt.Errorf("opencode serve exited without printing URL")
		}
		return &OpenCodeServer{
			URL:        url,
			ProjectDir: projectDir,
			Cmd:        cmd,
			StartedAt:  time.Now(),
		}, nil
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		return nil, fmt.Errorf("opencode serve did not start within 15s")
	case <-ctx.Done():
		cmd.Process.Kill()
		return nil, ctx.Err()
	}
}

// healthCheck pings the server's health endpoint.
func (m *OpenCodeServerManager) healthCheck(url string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url + "/global/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ListAgents returns the primary, non-hidden agents for the given project
// directory. Results are cached per server (agents don't change during a
// server's lifetime). If the server isn't running yet, it will be started.
func (m *OpenCodeServerManager) ListAgents(ctx context.Context, projectDir string) ([]AgentInfo, error) {
	serverURL, err := m.GetOrStartServer(ctx, projectDir)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if cached, ok := m.agents[serverURL]; ok {
		m.mu.Unlock()
		return cached, nil
	}
	m.mu.Unlock()

	agents, err := fetchAgents(ctx, serverURL)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.agents[serverURL] = agents
	m.mu.Unlock()

	return agents, nil
}

// fetchAgents calls GET /agent on the OpenCode server and returns primary,
// non-hidden agents. We make a direct HTTP call instead of using the SDK
// because the SDK's Agent struct doesn't include the "hidden" field.
func fetchAgents(ctx context.Context, serverURL string) ([]AgentInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", serverURL+"/agent", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch agents: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch agents: status %d", resp.StatusCode)
	}

	var raw []AgentInfo
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode agents: %w", err)
	}

	// Filter to primary, non-hidden agents only.
	var result []AgentInfo
	for _, a := range raw {
		if a.Mode == "primary" && !a.Hidden {
			result = append(result, a)
		}
	}
	return result, nil
}

// ListProjects queries the OpenCode server for all known projects.
// The server for seedDir must already be running or will be started.
func (m *OpenCodeServerManager) ListProjects(ctx context.Context, seedDir string) ([]ProjectInfo, error) {
	serverURL, err := m.GetOrStartServer(ctx, seedDir)
	if err != nil {
		return nil, err
	}
	client := opencode.NewClient(option.WithBaseURL(strings.TrimRight(serverURL, "/")))
	projects, err := client.Project.List(ctx, opencode.ProjectListParams{})
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	if projects == nil {
		return nil, nil
	}
	var result []ProjectInfo
	for _, p := range *projects {
		result = append(result, ProjectInfo{ID: p.ID, Worktree: p.Worktree})
	}
	return result, nil
}

// ListSessions queries the OpenCode server for all sessions belonging to
// the given project directory. Child/forked sessions (non-empty ParentID)
// are filtered out.
func (m *OpenCodeServerManager) ListSessions(ctx context.Context, projectDir string) ([]SessionSnapshot, error) {
	serverURL, err := m.GetOrStartServer(ctx, projectDir)
	if err != nil {
		return nil, err
	}
	client := opencode.NewClient(option.WithBaseURL(strings.TrimRight(serverURL, "/")))
	sessions, err := client.Session.List(ctx, opencode.SessionListParams{})
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	if sessions == nil {
		return nil, nil
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
			CreatedAt: time.UnixMilli(int64(s.Time.Created)),
			UpdatedAt: time.UnixMilli(int64(s.Time.Updated)),
		})
	}
	return result, nil
}
