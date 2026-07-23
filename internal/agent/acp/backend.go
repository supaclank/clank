package acp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/acksell/clank/internal/agent"
	sdk "github.com/coder/acp-go-sdk"
)

// eventBufferSize matches the host relay's appetite; emits block briefly
// when full rather than dropping (the relay is the sole, fast drain).
const eventBufferSize = 128

// drain windows for the late-update class: after a prompt resolves we
// wait for the stream to go quiet before committing the turn, so
// trailing tool_call_updates (claude-agent-acp #864) land inside it.
const (
	drainQuiet = 300 * time.Millisecond
	drainCap   = 2 * time.Second
)

// ConnResolver returns a live conn for the backend's scope — the
// supervisor's GetConn wrapped by the manager (analog of ServerResolver).
type ConnResolver func(ctx context.Context) (*AdapterConn, error)

// Backend adapts one ACP session to agent.SessionBackend. One value per
// clank session; the adapter process behind it is shared and supervised.
type Backend struct {
	profile     AdapterProfile
	resolver    ConnResolver
	workDir     string
	guidance    string
	initialMode agent.ClaudePermissionMode
	logf        func(format string, args ...any)

	// openMu serializes Open/OpenAndSend (idempotency contract).
	openMu sync.Mutex

	mu              sync.Mutex
	opened          bool
	status          agent.SessionStatus
	sessionID       string
	conn            *AdapterConn
	red             *reducer
	events          chan agent.Event
	eventsClosed    bool
	queue           []queuedPrompt
	runnerOn        bool
	aborting        bool
	stopping        bool
	pendingPerms    map[string]chan permDecision
	permSeq         int
	userSeq         int
	currentMode     string
	availableModes  []agent.SessionMode
	currentModel    string
	availableModels []agent.ModelInfo

	// onCatalog publishes the agent-advertised model list to the manager
	// so /models can answer without a live session of its own.
	onCatalog func(workDir string, models []agent.ModelInfo)
	// onModes publishes the agent-advertised mode list to the manager so
	// the compose view can offer modes before a session exists.
	onModes func(workDir string, modes []agent.SessionMode)

	lastUpdate atomicTime

	bgCtx    context.Context
	bgCancel context.CancelFunc
	stopOnce sync.Once
}

type queuedPrompt struct{ blocks []sdk.ContentBlock }

type permDecision struct{ allow bool }

// SetCatalogSink registers the manager callback that receives this
// session's agent-advertised model list.
func (b *Backend) SetCatalogSink(fn func(workDir string, models []agent.ModelInfo)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onCatalog = fn
}

// SetModeSink registers the manager callback that receives this
// session's agent-advertised mode list.
func (b *Backend) SetModeSink(fn func(workDir string, modes []agent.SessionMode)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onModes = fn
}

// NewBackend builds a SessionBackend for one clank session.
// resumeExternalID != "" resumes an existing ACP session via
// session/load; guidance is injected only on fresh sessions.
func NewBackend(profile AdapterProfile, workDir, resumeExternalID, guidance string, initialMode agent.ClaudePermissionMode, resolver ConnResolver, logf func(string, ...any)) *Backend {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &Backend{
		profile:      profile,
		resolver:     resolver,
		workDir:      workDir,
		guidance:     guidance,
		initialMode:  initialMode,
		logf:         logf,
		status:       agent.StatusStarting,
		sessionID:    resumeExternalID,
		red:          newReducer(logf),
		events:       make(chan agent.Event, eventBufferSize),
		pendingPerms: make(map[string]chan permDecision),
		bgCtx:        ctx,
		bgCancel:     cancel,
	}
	return b
}

var _ agent.SessionBackend = (*Backend)(nil)
var _ SessionHandler = (*Backend)(nil)

// Events returns the backend's event stream (hub relay is the sole drain).
func (b *Backend) Events() <-chan agent.Event { return b.events }

// Status returns the current session status snapshot.
func (b *Backend) Status() agent.SessionStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

// SessionID returns the backend-native (ACP) session id, "" until known.
func (b *Backend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

// Messages snapshots the in-memory transcript (committed + in-flight).
func (b *Backend) Messages(ctx context.Context) ([]agent.MessageData, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.opened {
		return nil, fmt.Errorf("acp %s: backend not open", b.profile.ID)
	}
	return b.red.snapshot(), nil
}

// emitLocked stamps and delivers one event. Callers hold b.mu — the
// buffer plus the always-draining relay keep the critical section short,
// and serializing sends under mu makes close-vs-send races impossible.
func (b *Backend) emitLocked(e agent.Event) {
	if b.eventsClosed {
		return
	}
	e.ExternalID = b.sessionID
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	b.events <- e
}

func (b *Backend) setStatusLocked(s agent.SessionStatus) {
	if b.status == s {
		return
	}
	old := b.status
	b.status = s
	b.emitLocked(agent.Event{
		Type: agent.EventStatusChange,
		Data: agent.StatusChangeData{OldStatus: old, NewStatus: s},
	})
}

// Stop tears the backend down: best-effort session/close (frees the
// claude adapter's per-session CLI child), deregister, final dead status.
// The adapter process itself stays up — the supervisor owns it.
func (b *Backend) Stop() error {
	b.stopOnce.Do(func() {
		b.mu.Lock()
		b.stopping = true
		conn := b.conn
		sid := b.sessionID
		b.failPendingPermsLocked()
		b.queue = nil
		b.mu.Unlock()

		if conn != nil && sid != "" && conn.Init().AgentCapabilities.SessionCapabilities.Close != nil {
			ctx, cancel := context.WithTimeout(context.Background(), stopGrace)
			_, err := conn.Conn().CloseSession(ctx, sdk.CloseSessionRequest{SessionId: sdk.SessionId(sid)})
			cancel()
			if err != nil {
				b.logf("acp %s: session/close %s: %v", b.profile.ID, sid, err)
			}
		}
		if conn != nil && sid != "" {
			conn.Deregister(sdk.SessionId(sid))
		}
		b.bgCancel()

		b.mu.Lock()
		b.setStatusLocked(agent.StatusDead)
		b.eventsClosed = true
		close(b.events)
		b.mu.Unlock()
	})
	return nil
}

// watchConn marks the session dead when the adapter transport dies —
// the host's ensureBackend then drops and rebuilds it (the same recovery
// contract the bespoke backends honor).
func (b *Backend) watchConn(conn *AdapterConn) {
	select {
	case <-conn.Closed():
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.stopping || b.conn != conn {
			return
		}
		b.logf("acp %s: adapter connection lost; marking session %s dead", b.profile.ID, b.sessionID)
		b.failPendingPermsLocked()
		b.queue = nil
		b.setStatusLocked(agent.StatusDead)
	case <-b.bgCtx.Done():
	}
}

// failPendingPermsLocked resolves every parked permission request as
// cancelled (abort/stop/disconnect sweep).
func (b *Backend) failPendingPermsLocked() {
	for id, ch := range b.pendingPerms {
		close(ch)
		delete(b.pendingPerms, id)
	}
}
