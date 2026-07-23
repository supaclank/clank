// Package acptest provides an in-process scripted ACP agent for tests: a
// real agent speaking real JSON-RPC over real pipes through the same SDK
// clank uses — the protocol-level analog of hosttest.StubBackend. Only
// exec.Cmd is substituted (io.Pipe pairs instead of a child process).
package acptest

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"

	acpx "github.com/acksell/clank/internal/agent/acp"
	sdk "github.com/coder/acp-go-sdk"
)

// ScriptedAgent implements the SDK's Agent (+ AgentLoader) interfaces via
// optional function fields. Nil fields get safe defaults: Initialize
// advertises a minimal capability set; everything else errors.
type ScriptedAgent struct {
	InitializeFn   func(ctx context.Context, p sdk.InitializeRequest) (sdk.InitializeResponse, error)
	AuthenticateFn func(ctx context.Context, p sdk.AuthenticateRequest) (sdk.AuthenticateResponse, error)
	LogoutFn       func(ctx context.Context, p sdk.LogoutRequest) (sdk.LogoutResponse, error)
	CancelFn       func(ctx context.Context, p sdk.CancelNotification) error
	CloseSessionFn func(ctx context.Context, p sdk.CloseSessionRequest) (sdk.CloseSessionResponse, error)
	ListSessionsFn func(ctx context.Context, p sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error)
	NewSessionFn   func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error)
	PromptFn       func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error)
	ResumeFn       func(ctx context.Context, p sdk.ResumeSessionRequest) (sdk.ResumeSessionResponse, error)
	SetModeFn      func(ctx context.Context, p sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error)
	SetConfigFn    func(ctx context.Context, p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error)
	LoadSessionFn  func(ctx context.Context, p sdk.LoadSessionRequest) (sdk.LoadSessionResponse, error)

	conn atomic.Pointer[sdk.AgentSideConnection]
}

// Conn returns the agent-side connection once the fixture is spawned —
// scripts use it to push session/update notifications or issue
// session/request_permission calls toward the client under test.
func (a *ScriptedAgent) Conn() *sdk.AgentSideConnection { return a.conn.Load() }

func (a *ScriptedAgent) setConn(c *sdk.AgentSideConnection) { a.conn.Store(c) }

// DefaultInitialize is the capability set advertised when InitializeFn is
// nil: protocol v1, loadSession plus list/resume/close session caps.
func DefaultInitialize() sdk.InitializeResponse {
	return sdk.InitializeResponse{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		AgentInfo:       &sdk.Implementation{Name: "acptest", Version: "0"},
		AgentCapabilities: sdk.AgentCapabilities{
			LoadSession: true,
			SessionCapabilities: sdk.SessionCapabilities{
				List:   &sdk.SessionListCapabilities{},
				Resume: &sdk.SessionResumeCapabilities{},
				Close:  &sdk.SessionCloseCapabilities{},
			},
		},
	}
}

func (a *ScriptedAgent) Initialize(ctx context.Context, p sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	if a.InitializeFn != nil {
		return a.InitializeFn(ctx, p)
	}
	return DefaultInitialize(), nil
}

func (a *ScriptedAgent) Authenticate(ctx context.Context, p sdk.AuthenticateRequest) (sdk.AuthenticateResponse, error) {
	if a.AuthenticateFn != nil {
		return a.AuthenticateFn(ctx, p)
	}
	return sdk.AuthenticateResponse{}, fmt.Errorf("acptest: Authenticate not scripted")
}

func (a *ScriptedAgent) Logout(ctx context.Context, p sdk.LogoutRequest) (sdk.LogoutResponse, error) {
	if a.LogoutFn != nil {
		return a.LogoutFn(ctx, p)
	}
	return sdk.LogoutResponse{}, fmt.Errorf("acptest: Logout not scripted")
}

func (a *ScriptedAgent) Cancel(ctx context.Context, p sdk.CancelNotification) error {
	if a.CancelFn != nil {
		return a.CancelFn(ctx, p)
	}
	return nil
}

func (a *ScriptedAgent) CloseSession(ctx context.Context, p sdk.CloseSessionRequest) (sdk.CloseSessionResponse, error) {
	if a.CloseSessionFn != nil {
		return a.CloseSessionFn(ctx, p)
	}
	return sdk.CloseSessionResponse{}, nil
}

func (a *ScriptedAgent) ListSessions(ctx context.Context, p sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error) {
	if a.ListSessionsFn != nil {
		return a.ListSessionsFn(ctx, p)
	}
	return sdk.ListSessionsResponse{}, fmt.Errorf("acptest: ListSessions not scripted")
}

func (a *ScriptedAgent) NewSession(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	if a.NewSessionFn != nil {
		return a.NewSessionFn(ctx, p)
	}
	return sdk.NewSessionResponse{SessionId: "acptest-session"}, nil
}

func (a *ScriptedAgent) Prompt(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
	if a.PromptFn != nil {
		return a.PromptFn(ctx, p)
	}
	return sdk.PromptResponse{}, fmt.Errorf("acptest: Prompt not scripted")
}

func (a *ScriptedAgent) ResumeSession(ctx context.Context, p sdk.ResumeSessionRequest) (sdk.ResumeSessionResponse, error) {
	if a.ResumeFn != nil {
		return a.ResumeFn(ctx, p)
	}
	return sdk.ResumeSessionResponse{}, fmt.Errorf("acptest: ResumeSession not scripted")
}

func (a *ScriptedAgent) SetSessionMode(ctx context.Context, p sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
	if a.SetModeFn != nil {
		return a.SetModeFn(ctx, p)
	}
	return sdk.SetSessionModeResponse{}, nil
}

func (a *ScriptedAgent) SetSessionConfigOption(ctx context.Context, p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
	if a.SetConfigFn != nil {
		return a.SetConfigFn(ctx, p)
	}
	return sdk.SetSessionConfigOptionResponse{}, nil
}

func (a *ScriptedAgent) LoadSession(ctx context.Context, p sdk.LoadSessionRequest) (sdk.LoadSessionResponse, error) {
	if a.LoadSessionFn != nil {
		return a.LoadSessionFn(ctx, p)
	}
	return sdk.LoadSessionResponse{}, fmt.Errorf("acptest: LoadSession not scripted")
}

// Proc wires factory's agent to a fresh AdapterConn over two io.Pipe
// pairs and returns it as an AdapterProc — the SpawnFunc payload. Closing
// either side (Stop) tears down both connections.
func Proc(ctx context.Context, profile acpx.AdapterProfile, agent *ScriptedAgent, logf func(string, ...any)) (*acpx.AdapterProc, error) {
	agentIn, clientOut := io.Pipe() // client → agent
	clientIn, agentOut := io.Pipe() // agent → client

	agentConn := sdk.NewAgentSideConnection(agent, agentOut, agentIn)
	agent.setConn(agentConn)

	conn, err := acpx.NewAdapterConn(ctx, profile, clientOut, clientIn, logf)
	if err != nil {
		_ = clientOut.Close()
		_ = agentOut.Close()
		return nil, err
	}
	stop := func() {
		_ = clientOut.Close()
		_ = agentOut.Close()
	}
	return &acpx.AdapterProc{Conn: conn, Stop: stop}, nil
}

// SpawnFunc adapts a per-scope agent factory into the supervisor's spawn
// seam. The factory runs once per (re)spawn, so tests can count spawns or
// vary behavior across restarts.
func SpawnFunc(factory func(scopeDir string) *ScriptedAgent, profile acpx.AdapterProfile, logf func(string, ...any)) acpx.SpawnFunc {
	return func(ctx context.Context, scopeDir string) (*acpx.AdapterProc, error) {
		return Proc(ctx, profile, factory(scopeDir), logf)
	}
}
