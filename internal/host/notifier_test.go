package host

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host/store"
	"github.com/acksell/clank/internal/notifier"
)

// recordingProvider is a real notifier.Provider used as a test fixture
// — captures every Send and Close call. Lives in the host package so
// tests can drive the Service's private subscriberRegistry directly,
// mirroring the existing recordingListener pattern in keepalive_test.go.
type recordingNotifierProvider struct {
	mu     sync.Mutex
	sent   []notifier.Notification
	closes int
}

func (p *recordingNotifierProvider) Send(_ context.Context, n notifier.Notification) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, n)
	return nil
}

func (p *recordingNotifierProvider) Close(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closes++
	return nil
}

func (p *recordingNotifierProvider) sentCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

func (p *recordingNotifierProvider) sentSnapshot() []notifier.Notification {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]notifier.Notification, len(p.sent))
	copy(out, p.sent)
	return out
}

func (p *recordingNotifierProvider) closeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closes
}

// newTestServiceWithNotifier constructs a Service wired with a
// recording Provider. The Service has no backends — the test publishes
// events directly via subscribers.Broadcast (the production fan-out
// point) so it can exercise the notifier wiring without spinning a
// real agent.
func newTestServiceWithNotifier(t *testing.T) (*Service, *recordingNotifierProvider) {
	t.Helper()
	rec := &recordingNotifierProvider{}
	loop := notifier.New(notifier.Config{Provider: rec})
	svc := New(Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{},
		NotifierLoop:    loop,
	})
	return svc, rec
}

func TestNotifier_BusyToIdleEventProducesOneNotification(t *testing.T) {
	t.Parallel()
	svc, rec := newTestServiceWithNotifier(t)
	if err := svc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(svc.Shutdown)

	svc.subscribers.Broadcast(agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "s1",
		Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusIdle},
	})

	if got := waitForNotifierSent(rec, 1, 500*time.Millisecond); got != 1 {
		t.Fatalf("got %d notifications, want 1", got)
	}
	got := rec.sentSnapshot()[0]
	if got.Kind != notifier.KindIdle {
		t.Errorf("Kind = %q, want %q", got.Kind, notifier.KindIdle)
	}
	if got.SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1", got.SessionID)
	}
}

func TestNotifier_DropsChattyEvents(t *testing.T) {
	t.Parallel()
	svc, rec := newTestServiceWithNotifier(t)
	if err := svc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(svc.Shutdown)

	// Mix of dropped + one notification-worthy event. Use the
	// notification-worthy one as a sync point: when it arrives, the
	// loop has processed the queue in order, so any earlier dropped
	// events are guaranteed to have been examined and discarded.
	svc.subscribers.Broadcast(agent.Event{Type: agent.EventMessage, SessionID: "s1", Data: agent.MessageData{}})
	svc.subscribers.Broadcast(agent.Event{Type: agent.EventPartUpdate, SessionID: "s1", Data: agent.PartUpdateData{}})
	svc.subscribers.Broadcast(agent.Event{Type: agent.EventTitleChange, SessionID: "s1", Data: agent.TitleChangeData{Title: "hi"}})
	svc.subscribers.Broadcast(agent.Event{
		Type:      agent.EventPermission,
		SessionID: "s1",
		Data:      agent.PermissionData{RequestID: "r1", Tool: "bash"},
	})

	if got := waitForNotifierSent(rec, 1, 500*time.Millisecond); got != 1 {
		t.Fatalf("got %d notifications, want 1 (permission only)", got)
	}
	if got := rec.sentSnapshot()[0].Kind; got != notifier.KindPermission {
		t.Errorf("Kind = %q, want %q", got, notifier.KindPermission)
	}
}

// transcriptBackend is a fixture SessionBackend whose Messages returns
// a canned transcript. Only the notifier's read path is exercised.
type transcriptBackend struct {
	noopBackendForNotifier
	msgs []agent.MessageData
}

func (b *transcriptBackend) Messages(_ context.Context) ([]agent.MessageData, error) {
	return b.msgs, nil
}

// noopBackendForNotifier satisfies the parts of agent.SessionBackend
// the notifier tests never touch.
type noopBackendForNotifier struct{}

func (noopBackendForNotifier) Open(_ context.Context) error                          { return nil }
func (noopBackendForNotifier) Send(_ context.Context, _ agent.SendMessageOpts) error { return nil }
func (noopBackendForNotifier) OpenAndSend(_ context.Context, _ agent.SendMessageOpts) error {
	return nil
}
func (noopBackendForNotifier) Abort(_ context.Context) error { return nil }
func (noopBackendForNotifier) Stop() error                   { return nil }
func (noopBackendForNotifier) Events() <-chan agent.Event {
	ch := make(chan agent.Event)
	close(ch)
	return ch
}
func (noopBackendForNotifier) Status() agent.SessionStatus { return agent.StatusIdle }
func (noopBackendForNotifier) SessionID() string           { return "" }
func (noopBackendForNotifier) Messages(_ context.Context) ([]agent.MessageData, error) {
	return nil, nil
}
func (noopBackendForNotifier) Revert(_ context.Context, _ string) error { return nil }
func (noopBackendForNotifier) Fork(_ context.Context, _ string) (agent.ForkResult, error) {
	return agent.ForkResult{}, nil
}
func (noopBackendForNotifier) RespondPermission(_ context.Context, _ string, _ bool, _ string) error {
	return nil
}

// TestNotifier_IdleNotificationCarriesTitleAndLastReply pins the
// end-to-end enrichment: a session with a persisted title and a live
// backend transcript produces an idle push titled with the session name
// and bodied with the agent's final reply — not the generic
// "Agent finished / Tap to see the result." copy.
func TestNotifier_IdleNotificationCarriesTitleAndLastReply(t *testing.T) {
	t.Parallel()
	st, err := store.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	rec := &recordingNotifierProvider{}
	loop := notifier.New(notifier.Config{Provider: rec})
	svc := New(Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{},
		SessionsStore:   st,
		NotifierLoop:    loop,
	})
	if err := svc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(svc.Shutdown)

	if err := st.UpsertSession(context.Background(), agent.SessionInfo{
		ID:      "s1",
		Backend: agent.BackendClaudeCode,
		Status:  agent.StatusBusy,
		Title:   "Fix login retry",
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	svc.mu.Lock()
	svc.sessions["s1"] = &transcriptBackend{msgs: []agent.MessageData{
		{Role: "user", Content: "please fix the retry loop"},
		{Role: "assistant", Parts: []agent.Part{
			{Type: agent.PartText, Text: "Fixed the retry loop and added a regression test."},
			{Type: agent.PartToolCall, Tool: "bash"},
		}},
	}}
	svc.mu.Unlock()

	svc.subscribers.Broadcast(agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "s1",
		Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusIdle},
	})

	if got := waitForNotifierSent(rec, 1, 500*time.Millisecond); got != 1 {
		t.Fatalf("got %d notifications, want 1", got)
	}
	n := rec.sentSnapshot()[0]
	if n.Title != "Fix login retry" {
		t.Errorf("Title = %q, want session title", n.Title)
	}
	if n.Body != "Fixed the retry loop and added a regression test." {
		t.Errorf("Body = %q, want last assistant reply", n.Body)
	}
	if got := n.Data["session_title"]; got != "Fix login retry" {
		t.Errorf("Data[session_title] = %v, want session title", got)
	}
}

// TestLastAssistantText documents the transcript-walk rules: the newest
// assistant message with text wins, tool-only tail messages are skipped,
// and Content is used when a backend doesn't populate parts.
func TestLastAssistantText(t *testing.T) {
	t.Parallel()
	msgs := []agent.MessageData{
		{Role: "assistant", Content: "older reply"},
		{Role: "assistant", Parts: []agent.Part{
			{Type: agent.PartThinking, Text: "hmm"},
			{Type: agent.PartText, Text: "final answer"},
		}},
		{Role: "assistant", Parts: []agent.Part{{Type: agent.PartToolCall, Tool: "bash"}}},
		{Role: "user", Content: "thanks"},
	}
	if got := lastAssistantText(msgs); got != "final answer" {
		t.Errorf("lastAssistantText = %q, want %q", got, "final answer")
	}
	if got := lastAssistantText([]agent.MessageData{{Role: "assistant", Content: "from content"}}); got != "from content" {
		t.Errorf("content fallback = %q, want %q", got, "from content")
	}
	if got := lastAssistantText(nil); got != "" {
		t.Errorf("empty transcript = %q, want empty", got)
	}
}

func TestNotifier_ShutdownCallsProviderClose(t *testing.T) {
	t.Parallel()
	svc, rec := newTestServiceWithNotifier(t)
	if err := svc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}

	svc.Shutdown()
	if rec.closeCount() != 1 {
		t.Errorf("Provider.Close call count = %d, want 1", rec.closeCount())
	}
}

func TestNotifier_NilLoopSkipsWiring(t *testing.T) {
	t.Parallel()
	svc := New(Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{},
		// NotifierLoop: nil
	})
	if svc.notifierLoop != nil {
		t.Error("notifierLoop should be nil when no Loop configured")
	}
	if err := svc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(svc.Shutdown)

	if got := len(svc.subscribers.subs); got != 0 {
		t.Errorf("subscriber slots = %d, want 0 (no notifier subscriber)", got)
	}
}

// TestSessionNotificationContext_SuppressesExpiredContextLogs pins that
// a shutdown/timeout-cancelled lookup context degrades silently — only
// a genuine lookup failure (TestSessionNotificationContext_LogsGenuineFailure)
// should reach the log.
func TestSessionNotificationContext_SuppressesExpiredContextLogs(t *testing.T) {
	t.Parallel()
	st, err := store.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	var buf bytes.Buffer
	svc := New(Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{},
		SessionsStore:   st,
		Log:             log.New(&buf, "", 0),
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.sessionNotificationContext(ctx, "s1")

	if buf.Len() != 0 {
		t.Errorf("expected no log output for a canceled lookup context, got %q", buf.String())
	}
}

// TestSessionNotificationContext_LogsGenuineFailure is the control for
// the above: a lookup failure that isn't context expiry must still log.
func TestSessionNotificationContext_LogsGenuineFailure(t *testing.T) {
	t.Parallel()
	st, err := store.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.Close() // forces GetSession to fail with a non-context error

	var buf bytes.Buffer
	svc := New(Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{},
		SessionsStore:   st,
		Log:             log.New(&buf, "", 0),
	})

	svc.sessionNotificationContext(context.Background(), "s1")

	if buf.Len() == 0 {
		t.Error("expected a genuine lookup failure to be logged")
	}
}

func waitForNotifierSent(p *recordingNotifierProvider, want int, timeout time.Duration) int {
	deadline := time.After(timeout)
	for {
		if c := p.sentCount(); c >= want {
			return c
		}
		select {
		case <-deadline:
			return p.sentCount()
		case <-time.After(5 * time.Millisecond):
		}
	}
}
