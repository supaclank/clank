// Package hosttest provides shared test doubles for wiring an in-process
// host.Service in end-to-end tests: a stub backend manager standing in for
// the real opencode/claude process boundary, and a throwaway git repo
// factory. Everything else in such tests (host service, mux, store,
// daemonclient) is real.
package hosttest

import (
	"context"
	"sync"

	"github.com/acksell/clank/internal/agent"
)

// StubBackendManager spawns a StubBackend on every CreateBackend. Last()
// returns the most recently created backend so tests can inspect what it
// received (e.g. did handleCreateSession actually dispatch the initial
// prompt via OpenAndSend?).
type StubBackendManager struct {
	mu   sync.Mutex
	last *StubBackend
}

func (m *StubBackendManager) Init(_ context.Context, _ func() ([]string, error)) error { return nil }

func (m *StubBackendManager) CreateBackend(_ context.Context, _ agent.BackendInvocation) (agent.SessionBackend, error) {
	b := &StubBackend{
		events: make(chan agent.Event, 16),
		done:   make(chan struct{}),
	}
	m.mu.Lock()
	m.last = b
	m.mu.Unlock()
	return b, nil
}

func (m *StubBackendManager) Shutdown() {}

// Last returns the backend created by the most recent CreateBackend call,
// or nil if none has been created yet.
func (m *StubBackendManager) Last() *StubBackend {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

// StubBackend is a programmable agent.SessionBackend that records what the
// host dispatched to it, so wire-level tests can assert the translation
// without mocking yet another struct.
type StubBackend struct {
	events chan agent.Event
	// done closes when Stop runs. PushEvent guards on it so a test
	// goroutine racing service-Cleanup never panics on send-to-
	// closed-channel and the race detector stays quiet.
	done chan struct{}
	// pendingPushes counts in-flight PushEvent calls so Stop can wait
	// for them to finish before closing the events channel — without
	// it, a PushEvent that has passed its done-check but hasn't
	// reached the send still races with close(events).
	pendingPushes sync.WaitGroup
	stopOnce      sync.Once

	mu          sync.Mutex
	openCalled  bool
	sendOpts    agent.SendMessageOpts
	openAndSend bool

	// Tests override the runtime fields a real backend usually
	// updates from inside Open/Start. Both fields are protected by
	// b.mu and read by Status()/SessionID() on the host's relay
	// goroutine, so concurrent test mutation is safe.
	statusOverride agent.SessionStatus
	statusSet      bool
	idOverride     string
	idSet          bool

	aborted          bool
	stopped          bool
	revertID         string
	forkID           string
	permissionID     string
	permissionAllow  bool
	permissionCalled bool

	questionRequestID string
	questionAnswers   []agent.QuestionAnswer
	questionReject    bool
	questionCalled    bool
}

// PushEvent injects an event into the backend's events channel as if
// the agent emitted it. Drops the event if Stop has been called so
// test goroutines that race the service's Cleanup don't panic on
// send-to-closed-channel (and don't trip the race detector).
func (b *StubBackend) PushEvent(evt agent.Event) {
	// stopped and pendingPushes.Add must be guarded by the same lock
	// Stop uses around close(done)+Wait, otherwise Add can race with
	// (or follow) a Wait that already returned on a zero counter.
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return
	}
	b.pendingPushes.Add(1)
	b.mu.Unlock()

	defer b.pendingPushes.Done()
	select {
	case b.events <- evt:
	case <-b.done:
	}
}

// SetExternalID overrides what SessionID() returns going forward.
// Tests use this to mimic opencode's late-binding behavior — the real
// session ID isn't known until Open completes.
func (b *StubBackend) SetExternalID(id string) {
	b.mu.Lock()
	b.idOverride = id
	b.idSet = true
	b.mu.Unlock()
}

// SetStatus overrides what Status() returns going forward.
func (b *StubBackend) SetStatus(s agent.SessionStatus) {
	b.mu.Lock()
	b.statusOverride = s
	b.statusSet = true
	b.mu.Unlock()
}

// LastSendOpts returns the options from the most recent Send or
// OpenAndSend dispatch.
func (b *StubBackend) LastSendOpts() agent.SendMessageOpts {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sendOpts
}

// OpenAndSendCalled reports whether the host dispatched the initial
// prompt via OpenAndSend.
func (b *StubBackend) OpenAndSendCalled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.openAndSend
}

// Aborted reports whether Abort was dispatched.
func (b *StubBackend) Aborted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.aborted
}

// RevertedMessageID returns the message id from the most recent Revert.
func (b *StubBackend) RevertedMessageID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.revertID
}

// ForkedMessageID returns the message id from the most recent Fork.
func (b *StubBackend) ForkedMessageID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.forkID
}

// PermissionReply returns whether RespondPermission was called and with
// which permission id and verdict.
func (b *StubBackend) PermissionReply() (called bool, permissionID string, allow bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.permissionCalled, b.permissionID, b.permissionAllow
}

// QuestionReply returns whether RespondQuestion was called and with which
// request id, answers, and reject flag.
func (b *StubBackend) QuestionReply() (called bool, requestID string, answers []agent.QuestionAnswer, reject bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.questionCalled, b.questionRequestID, b.questionAnswers, b.questionReject
}

func (b *StubBackend) Open(context.Context) error {
	b.mu.Lock()
	b.openCalled = true
	b.mu.Unlock()
	return nil
}

func (b *StubBackend) OpenAndSend(_ context.Context, opts agent.SendMessageOpts) error {
	b.mu.Lock()
	b.openCalled = true
	b.openAndSend = true
	b.sendOpts = opts
	b.mu.Unlock()
	return nil
}

func (b *StubBackend) Send(_ context.Context, opts agent.SendMessageOpts) error {
	b.mu.Lock()
	b.sendOpts = opts
	b.mu.Unlock()
	return nil
}

func (b *StubBackend) Abort(context.Context) error {
	b.mu.Lock()
	b.aborted = true
	b.mu.Unlock()
	return nil
}

func (b *StubBackend) Stop() error {
	b.stopOnce.Do(func() {
		// stopped and close(done) happen under mu so PushEvent can
		// never call pendingPushes.Add after this Wait has started.
		b.mu.Lock()
		b.stopped = true
		close(b.done)
		b.mu.Unlock()

		b.pendingPushes.Wait()
		close(b.events)
	})
	return nil
}

func (b *StubBackend) Status() agent.SessionStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.statusSet {
		return b.statusOverride
	}
	return agent.StatusIdle
}

func (b *StubBackend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.idSet {
		return b.idOverride
	}
	return "stub-ext-id"
}

func (*StubBackend) Messages(context.Context) ([]agent.MessageData, error) { return nil, nil }

func (b *StubBackend) Fork(_ context.Context, msgID string) (agent.ForkResult, error) {
	b.mu.Lock()
	b.forkID = msgID
	b.mu.Unlock()
	return agent.ForkResult{ID: "ext-forked-" + msgID}, nil
}

func (b *StubBackend) RespondPermission(_ context.Context, permissionID string, allow bool, _ string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.permissionID = permissionID
	b.permissionAllow = allow
	b.permissionCalled = true
	return nil
}

func (b *StubBackend) Events() <-chan agent.Event { return b.events }
