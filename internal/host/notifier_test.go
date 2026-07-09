package host

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host/store"
	"github.com/acksell/clank/internal/notifier"
)

func TestClassifyEvent(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		evt       agent.Event
		wantOK    bool
		wantKind  notifier.Kind
		wantTitle string
		wantData  map[string]any
	}{
		{
			name: "busy_to_idle_produces_kind_idle",
			evt: agent.Event{
				Type:      agent.EventStatusChange,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusIdle},
			},
			wantOK:    true,
			wantKind:  notifier.KindIdle,
			wantTitle: "Agent finished",
		},
		{
			name: "starting_to_idle_produces_kind_idle",
			evt: agent.Event{
				Type:      agent.EventStatusChange,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.StatusChangeData{OldStatus: agent.StatusStarting, NewStatus: agent.StatusIdle},
			},
			wantOK:   true,
			wantKind: notifier.KindIdle,
		},
		{
			name: "idle_to_idle_dropped",
			evt: agent.Event{
				Type:      agent.EventStatusChange,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.StatusChangeData{OldStatus: agent.StatusIdle, NewStatus: agent.StatusIdle},
			},
			wantOK: false,
		},
		{
			name: "idle_to_busy_dropped",
			evt: agent.Event{
				Type:      agent.EventStatusChange,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.StatusChangeData{OldStatus: agent.StatusIdle, NewStatus: agent.StatusBusy},
			},
			wantOK: false,
		},
		{
			name: "error_to_idle_dropped",
			evt: agent.Event{
				Type:      agent.EventStatusChange,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.StatusChangeData{OldStatus: agent.StatusError, NewStatus: agent.StatusIdle},
			},
			wantOK: false,
		},
		{
			name: "busy_to_error_dropped",
			evt: agent.Event{
				Type:      agent.EventStatusChange,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusError},
			},
			wantOK: false,
		},
		{
			name: "permission_request",
			evt: agent.Event{
				Type:      agent.EventPermission,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.PermissionData{RequestID: "req-1", Tool: "bash", Description: "run `ls -la`"},
			},
			wantOK:    true,
			wantKind:  notifier.KindPermission,
			wantTitle: "Permission requested: bash",
			wantData:  map[string]any{"request_id": "req-1", "tool": "bash"},
		},
		{
			name: "permission_without_tool_name",
			evt: agent.Event{
				Type:      agent.EventPermission,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.PermissionData{RequestID: "req-2"},
			},
			wantOK:    true,
			wantKind:  notifier.KindPermission,
			wantTitle: "Permission requested",
		},
		{
			name: "error_event",
			evt: agent.Event{
				Type:      agent.EventError,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.ErrorData{Message: "model unavailable"},
			},
			wantOK:    true,
			wantKind:  notifier.KindError,
			wantTitle: "Agent error",
		},
		{
			name: "message_dropped",
			evt:  agent.Event{Type: agent.EventMessage, SessionID: "s1", Data: agent.MessageData{Role: "user"}},
		},
		{
			name: "part_dropped",
			evt:  agent.Event{Type: agent.EventPartUpdate, SessionID: "s1", Data: agent.PartUpdateData{}},
		},
		{
			name: "title_dropped",
			evt:  agent.Event{Type: agent.EventTitleChange, SessionID: "s1", Data: agent.TitleChangeData{Title: "hello"}},
		},
		{
			name: "reconnecting_dropped",
			evt:  agent.Event{Type: agent.EventReconnecting, SessionID: "s1", Data: agent.ReconnectingData{Attempt: 1}},
		},
		{
			name: "voice_status_dropped",
			evt:  agent.Event{Type: agent.EventVoiceStatus, SessionID: "s1", Data: agent.VoiceStatusData{Status: agent.VoiceStatusIdle}},
		},
		{
			name: "session_create_dropped",
			evt:  agent.Event{Type: agent.EventSessionCreate, SessionID: "s1"},
		},
		{
			name: "status_change_with_wrong_data_dropped",
			evt:  agent.Event{Type: agent.EventStatusChange, SessionID: "s1", Data: "garbage"},
		},
		{
			name: "permission_with_wrong_data_dropped",
			evt:  agent.Event{Type: agent.EventPermission, SessionID: "s1", Data: "garbage"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n, ok := classifyEvent(tc.evt, pushContext{})
			if ok != tc.wantOK {
				t.Fatalf("classifyEvent returned ok=%v, want %v (n=%+v)", ok, tc.wantOK, n)
			}
			if !ok {
				return
			}
			if n.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", n.Kind, tc.wantKind)
			}
			if n.SessionID != tc.evt.SessionID {
				t.Errorf("SessionID = %q, want %q", n.SessionID, tc.evt.SessionID)
			}
			if tc.wantTitle != "" && n.Title != tc.wantTitle {
				t.Errorf("Title = %q, want %q", n.Title, tc.wantTitle)
			}
			if n.Body == "" {
				t.Error("Body must not be empty")
			}
			for k, want := range tc.wantData {
				got, ok := n.Data[k]
				if !ok {
					t.Errorf("Data[%q] missing", k)
					continue
				}
				if got != want {
					t.Errorf("Data[%q] = %v, want %v", k, got, want)
				}
			}
			if n.OccurredAt.IsZero() {
				t.Error("OccurredAt must be set")
			}
		})
	}
}

// TestClassifyEvent_PushContextEnrichment documents why notifications
// carry session metadata: a phone showing three "Agent finished" banners
// is useless — the session name and the reply preview are what let the
// user decide whether to pick up the phone.
func TestClassifyEvent_PushContextEnrichment(t *testing.T) {
	t.Parallel()

	idleEvt := agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "s1",
		Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusIdle},
	}
	permEvt := agent.Event{
		Type:      agent.EventPermission,
		SessionID: "s1",
		Data:      agent.PermissionData{RequestID: "r1", Tool: "bash", Description: "run `ls -la`"},
	}
	errEvt := agent.Event{
		Type:      agent.EventError,
		SessionID: "s1",
		Data:      agent.ErrorData{Message: "model unavailable"},
	}

	cases := []struct {
		name          string
		evt           agent.Event
		pctx          pushContext
		wantTitle     string
		wantBody      string
		wantDataTitle string
	}{
		{
			name:          "idle_with_title_and_preview",
			evt:           idleEvt,
			pctx:          pushContext{Title: "Fix login retry", LastAssistantText: "Done. The retry loop now backs off."},
			wantTitle:     "Fix login retry",
			wantBody:      "Done. The retry loop now backs off.",
			wantDataTitle: "Fix login retry",
		},
		{
			name:      "idle_with_title_only_keeps_finished_signal",
			evt:       idleEvt,
			pctx:      pushContext{Title: "Fix login retry"},
			wantTitle: "Fix login retry",
			wantBody:  "Finished — tap to see the result.",
		},
		{
			name:      "idle_with_preview_only",
			evt:       idleEvt,
			pctx:      pushContext{LastAssistantText: "All tests pass."},
			wantTitle: "Agent finished",
			wantBody:  "All tests pass.",
		},
		{
			name:      "idle_preview_collapses_newlines",
			evt:       idleEvt,
			pctx:      pushContext{LastAssistantText: "Done.\n\n- fixed retry\n- added tests"},
			wantTitle: "Agent finished",
			wantBody:  "Done. - fixed retry - added tests",
		},
		{
			name:          "permission_with_title_moves_kind_into_body",
			evt:           permEvt,
			pctx:          pushContext{Title: "Fix login retry"},
			wantTitle:     "Fix login retry",
			wantBody:      "Permission requested: bash — run `ls -la`",
			wantDataTitle: "Fix login retry",
		},
		{
			name:          "error_with_title_moves_kind_into_body",
			evt:           errEvt,
			pctx:          pushContext{Title: "Fix login retry"},
			wantTitle:     "Fix login retry",
			wantBody:      "Agent error — model unavailable",
			wantDataTitle: "Fix login retry",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n, ok := classifyEvent(tc.evt, tc.pctx)
			if !ok {
				t.Fatal("classifyEvent rejected a notification-worthy event")
			}
			if n.Title != tc.wantTitle {
				t.Errorf("Title = %q, want %q", n.Title, tc.wantTitle)
			}
			if n.Body != tc.wantBody {
				t.Errorf("Body = %q, want %q", n.Body, tc.wantBody)
			}
			if tc.wantDataTitle != "" {
				if got := n.Data["session_title"]; got != tc.wantDataTitle {
					t.Errorf("Data[session_title] = %v, want %q", got, tc.wantDataTitle)
				}
			}
		})
	}
}

func TestClassifyEvent_ZeroTimestampDefaultsToNow(t *testing.T) {
	t.Parallel()
	evt := agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "s1",
		Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusIdle},
	}
	before := time.Now()
	n, ok := classifyEvent(evt, pushContext{})
	after := time.Now()
	if !ok {
		t.Fatal("expected classifyEvent to accept the event")
	}
	if n.OccurredAt.Before(before) || n.OccurredAt.After(after) {
		t.Errorf("OccurredAt = %v, want in [%v, %v]", n.OccurredAt, before, after)
	}
}

func TestPreviewText_TruncatesLongReplies(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("na ", 200)
	got := previewText(long)
	if r := []rune(got); len(r) != maxBodyPreviewLen {
		t.Errorf("len = %d runes, want %d", len(r), maxBodyPreviewLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated preview must end with ellipsis, got %q", got[len(got)-8:])
	}
	if short := previewText("short"); short != "short" {
		t.Errorf("short input must pass through, got %q", short)
	}
}

// TestPreviewText_ScanBoundHandlesReplyLargerThanScanWindow pins that
// scanning only a bounded prefix (previewScanBytes) instead of the full
// reply still produces a correctly truncated, valid-UTF8 preview — even
// when the reply is orders of magnitude larger than that prefix, and
// even when the scan window cuts through a multi-byte rune.
func TestPreviewText_ScanBoundHandlesReplyLargerThanScanWindow(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("word ", 100_000) // far beyond previewScanBytes
	got := previewText(huge)
	if r := []rune(got); len(r) != maxBodyPreviewLen {
		t.Errorf("len = %d runes, want %d", len(r), maxBodyPreviewLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated preview must end with ellipsis, got %q", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("preview must be valid UTF-8, got %q", got)
	}

	// Multi-byte runes ("café ", 5 bytes) placed right across the
	// previewScanBytes boundary must not corrupt the output.
	multiByte := strings.Repeat("café ", (previewScanBytes/5)+50)
	got = previewText(multiByte)
	if !utf8.ValidString(got) {
		t.Errorf("preview must be valid UTF-8 across a multi-byte scan boundary, got %q", got)
	}
}

func TestTruncateRunes(t *testing.T) {
	t.Parallel()
	if got := truncateRunes("short", 80); got != "short" {
		t.Errorf("under-limit input must pass through unchanged, got %q", got)
	}
	long := strings.Repeat("x", 100)
	got := truncateRunes(long, maxTitleLen)
	if r := []rune(got); len(r) != maxTitleLen {
		t.Errorf("len = %d runes, want %d", len(r), maxTitleLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated title must end with ellipsis, got %q", got)
	}
}

// TestClassifyEvent_LongSessionTitleIsBounded documents why: push
// providers reject oversized payloads, and nothing upstream bounds an
// AI-generated or user-set session title before it reaches here.
func TestClassifyEvent_LongSessionTitleIsBounded(t *testing.T) {
	t.Parallel()
	longTitle := strings.Repeat("x", 200)
	evt := agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "s1",
		Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusIdle},
	}
	n, ok := classifyEvent(evt, pushContext{Title: longTitle})
	if !ok {
		t.Fatal("classifyEvent rejected a notification-worthy event")
	}
	if r := []rune(n.Title); len(r) != maxTitleLen {
		t.Errorf("Title len = %d runes, want %d", len(r), maxTitleLen)
	}
	if r := []rune(n.Data["session_title"].(string)); len(r) != maxTitleLen {
		t.Errorf("Data[session_title] len = %d runes, want %d", len(r), maxTitleLen)
	}
}

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

// TestPushContextFor_SuppressesExpiredContextLogs pins that
// a shutdown/timeout-cancelled lookup context degrades silently — only
// a genuine lookup failure (TestPushContextFor_LogsGenuineFailure)
// should reach the log.
func TestPushContextFor_SuppressesExpiredContextLogs(t *testing.T) {
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
	svc.pushContextFor(ctx, "s1")

	if buf.Len() != 0 {
		t.Errorf("expected no log output for a canceled lookup context, got %q", buf.String())
	}
}

// TestPushContextFor_LogsGenuineFailure is the control for
// the above: a lookup failure that isn't context expiry must still log.
func TestPushContextFor_LogsGenuineFailure(t *testing.T) {
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

	svc.pushContextFor(context.Background(), "s1")

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
