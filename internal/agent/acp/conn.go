package acp

import (
	"context"
	"fmt"
	"io"
	"sync"

	sdk "github.com/coder/acp-go-sdk"
)

// SessionHandler receives the server→client traffic for one ACP session.
// The acp.Backend implements it (slice 3); tests implement it directly.
type SessionHandler interface {
	HandleSessionUpdate(ctx context.Context, n sdk.SessionNotification)
	HandleRequestPermission(ctx context.Context, req sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error)
}

// AdapterConn wraps one adapter process's stdio JSON-RPC connection:
// initialize runs once at construction, the response (capabilities) is
// cached, and session/update + session/request_permission traffic is
// routed to the SessionHandler registered for the session id.
type AdapterConn struct {
	profile AdapterProfile
	conn    *sdk.ClientSideConnection
	init    sdk.InitializeResponse
	logf    func(format string, args ...any)

	mu       sync.RWMutex
	sessions map[sdk.SessionId]SessionHandler

	closeOnce sync.Once
	closed    chan struct{}
}

// NewAdapterConn binds the child's stdin/stdout pipes, runs initialize,
// and starts routing. It does not own the process — the supervisor does.
// Exported so acptest can build in-process procs over pipe pairs.
func NewAdapterConn(ctx context.Context, profile AdapterProfile, stdin io.Writer, stdout io.Reader, logf func(string, ...any)) (*AdapterConn, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	c := &AdapterConn{
		profile:  profile,
		logf:     logf,
		sessions: make(map[sdk.SessionId]SessionHandler),
		closed:   make(chan struct{}),
	}
	c.conn = sdk.NewClientSideConnection(clientAdapter{c}, stdin, stdout)
	go func() {
		<-c.conn.Done()
		c.markClosed()
	}()

	initCtx, cancel := context.WithTimeout(ctx, spawnTimeout)
	defer cancel()
	init, err := c.conn.Initialize(initCtx, sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		ClientInfo:      &sdk.Implementation{Name: "clank", Version: "dev"},
	})
	if err != nil {
		c.markClosed()
		return nil, fmt.Errorf("acp %s: initialize: %w", profile.ID, err)
	}
	if init.ProtocolVersion != sdk.ProtocolVersionNumber {
		c.markClosed()
		return nil, fmt.Errorf("acp %s: protocol version mismatch: agent=%d client=%d", profile.ID, init.ProtocolVersion, sdk.ProtocolVersionNumber)
	}
	c.init = init
	return c, nil
}

// Init returns the cached initialize response (agent capabilities).
func (c *AdapterConn) Init() sdk.InitializeResponse { return c.init }

// Conn exposes the underlying connection for outbound RPCs
// (session/new, session/prompt, …).
func (c *AdapterConn) Conn() *sdk.ClientSideConnection { return c.conn }

// Closed is closed when the peer disconnects or the process dies.
func (c *AdapterConn) Closed() <-chan struct{} { return c.closed }

func (c *AdapterConn) markClosed() { c.closeOnce.Do(func() { close(c.closed) }) }

// Register routes a session's server→client traffic to h.
func (c *AdapterConn) Register(id sdk.SessionId, h SessionHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions[id] = h
}

// Deregister stops routing for a session id.
func (c *AdapterConn) Deregister(id sdk.SessionId) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, id)
}

func (c *AdapterConn) handlerFor(id sdk.SessionId) (SessionHandler, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h, ok := c.sessions[id]
	return h, ok
}

// clientAdapter implements the SDK's Client interface on behalf of the
// conn. fs/terminal methods reject: clank never advertises those
// capabilities, so a compliant agent won't call them.
type clientAdapter struct{ c *AdapterConn }

func (a clientAdapter) SessionUpdate(ctx context.Context, n sdk.SessionNotification) error {
	if h, ok := a.c.handlerFor(n.SessionId); ok {
		h.HandleSessionUpdate(ctx, n)
		return nil
	}
	a.c.logf("acp %s: dropping update for unrouted session %s", a.c.profile.ID, n.SessionId)
	return nil
}

func (a clientAdapter) RequestPermission(ctx context.Context, req sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	if h, ok := a.c.handlerFor(req.SessionId); ok {
		return h.HandleRequestPermission(ctx, req)
	}
	// Cancelling (instead of erroring) keeps the agent unwedged when a
	// prompt races session teardown.
	a.c.logf("acp %s: cancelling permission request for unrouted session %s", a.c.profile.ID, req.SessionId)
	return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeCancelled()}, nil
}

func (a clientAdapter) ReadTextFile(ctx context.Context, _ sdk.ReadTextFileRequest) (sdk.ReadTextFileResponse, error) {
	return sdk.ReadTextFileResponse{}, fmt.Errorf("fs is not supported by this client")
}

func (a clientAdapter) WriteTextFile(ctx context.Context, _ sdk.WriteTextFileRequest) (sdk.WriteTextFileResponse, error) {
	return sdk.WriteTextFileResponse{}, fmt.Errorf("fs is not supported by this client")
}

func (a clientAdapter) CreateTerminal(ctx context.Context, _ sdk.CreateTerminalRequest) (sdk.CreateTerminalResponse, error) {
	return sdk.CreateTerminalResponse{}, fmt.Errorf("terminal is not supported by this client")
}

func (a clientAdapter) KillTerminal(ctx context.Context, _ sdk.KillTerminalRequest) (sdk.KillTerminalResponse, error) {
	return sdk.KillTerminalResponse{}, fmt.Errorf("terminal is not supported by this client")
}

func (a clientAdapter) TerminalOutput(ctx context.Context, _ sdk.TerminalOutputRequest) (sdk.TerminalOutputResponse, error) {
	return sdk.TerminalOutputResponse{}, fmt.Errorf("terminal is not supported by this client")
}

func (a clientAdapter) ReleaseTerminal(ctx context.Context, _ sdk.ReleaseTerminalRequest) (sdk.ReleaseTerminalResponse, error) {
	return sdk.ReleaseTerminalResponse{}, fmt.Errorf("terminal is not supported by this client")
}

func (a clientAdapter) WaitForTerminalExit(ctx context.Context, _ sdk.WaitForTerminalExitRequest) (sdk.WaitForTerminalExitResponse, error) {
	return sdk.WaitForTerminalExitResponse{}, fmt.Errorf("terminal is not supported by this client")
}
