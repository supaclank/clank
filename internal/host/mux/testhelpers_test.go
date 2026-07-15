package hostmux_test

import (
	"context"

	"github.com/acksell/clank/internal/agent"
)

// noopBackendManager satisfies agent.BackendManager for tests that need
// a host.Service but never exercise a real backend.
type noopBackendManager struct{}

func (m *noopBackendManager) Init(_ context.Context, _ func() ([]string, error)) error { return nil }
func (m *noopBackendManager) CreateBackend(_ context.Context, _ agent.BackendInvocation) (agent.SessionBackend, error) {
	return &noopBackend{}, nil
}
func (m *noopBackendManager) Shutdown() {}

type noopBackend struct{}

func (b *noopBackend) Open(_ context.Context) error                                 { return nil }
func (b *noopBackend) OpenAndSend(_ context.Context, _ agent.SendMessageOpts) error { return nil }
func (b *noopBackend) Send(_ context.Context, _ agent.SendMessageOpts) error        { return nil }
func (b *noopBackend) Abort(_ context.Context) error                                { return nil }
func (b *noopBackend) Stop() error                                                  { return nil }
func (b *noopBackend) Events() <-chan agent.Event {
	ch := make(chan agent.Event)
	close(ch)
	return ch
}
func (b *noopBackend) Status() agent.SessionStatus                             { return agent.StatusIdle }
func (b *noopBackend) SessionID() string                                       { return "stub" }
func (b *noopBackend) Messages(_ context.Context) ([]agent.MessageData, error) { return nil, nil }
func (b *noopBackend) Revert(_ context.Context, _ string) error                { return nil }
func (b *noopBackend) Fork(_ context.Context, _ string) (agent.ForkResult, error) {
	return agent.ForkResult{}, nil
}
func (b *noopBackend) PendingPermissions(context.Context) ([]agent.PermissionData, error) {
	return nil, nil
}
func (b *noopBackend) RespondPermission(_ context.Context, _ string, _ bool, _ string) error {
	return nil
}

func (b *noopBackend) RespondQuestion(_ context.Context, _ string, _ []agent.QuestionAnswer, _ bool) error {
	return nil
}
