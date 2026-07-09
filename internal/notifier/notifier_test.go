package notifier

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/acksell/clank/internal/agent"
)

func TestClassify(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		evt       agent.Event
		wantOK    bool
		wantKind  Kind
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
			wantKind:  KindIdle,
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
			wantKind: KindIdle,
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
			wantKind:  KindPermission,
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
			wantKind:  KindPermission,
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
			wantKind:  KindError,
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
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n, ok := classify(tc.evt, SessionContext{})
			if ok != tc.wantOK {
				t.Fatalf("classify returned ok=%v, want %v (n=%+v)", ok, tc.wantOK, n)
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

// TestClassify_SessionContextEnrichment documents why notifications
// carry session metadata: a phone showing three "Agent finished" banners
// is useless — the session name and the reply preview are what let the
// user decide whether to pick up the phone.
func TestClassify_SessionContextEnrichment(t *testing.T) {
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
		sctx          SessionContext
		wantTitle     string
		wantBody      string
		wantDataTitle string
	}{
		{
			name:          "idle_with_title_and_preview",
			evt:           idleEvt,
			sctx:          SessionContext{Title: "Fix login retry", LastAssistantText: "Done. The retry loop now backs off."},
			wantTitle:     "Fix login retry",
			wantBody:      "Done. The retry loop now backs off.",
			wantDataTitle: "Fix login retry",
		},
		{
			name:      "idle_with_title_only_keeps_finished_signal",
			evt:       idleEvt,
			sctx:      SessionContext{Title: "Fix login retry"},
			wantTitle: "Fix login retry",
			wantBody:  "Finished — tap to see the result.",
		},
		{
			name:      "idle_with_preview_only",
			evt:       idleEvt,
			sctx:      SessionContext{LastAssistantText: "All tests pass."},
			wantTitle: "Agent finished",
			wantBody:  "All tests pass.",
		},
		{
			name:      "idle_preview_collapses_newlines",
			evt:       idleEvt,
			sctx:      SessionContext{LastAssistantText: "Done.\n\n- fixed retry\n- added tests"},
			wantTitle: "Agent finished",
			wantBody:  "Done. - fixed retry - added tests",
		},
		{
			name:          "permission_with_title_moves_kind_into_body",
			evt:           permEvt,
			sctx:          SessionContext{Title: "Fix login retry"},
			wantTitle:     "Fix login retry",
			wantBody:      "Permission requested: bash — run `ls -la`",
			wantDataTitle: "Fix login retry",
		},
		{
			name:          "error_with_title_moves_kind_into_body",
			evt:           errEvt,
			sctx:          SessionContext{Title: "Fix login retry"},
			wantTitle:     "Fix login retry",
			wantBody:      "Agent error — model unavailable",
			wantDataTitle: "Fix login retry",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n, ok := classify(tc.evt, tc.sctx)
			if !ok {
				t.Fatal("classify rejected a notification-worthy event")
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

// TestClassify_LongSessionTitleIsBounded documents why: push providers
// reject oversized payloads, and nothing upstream bounds an AI-generated
// or user-set session title before it reaches here.
func TestClassify_LongSessionTitleIsBounded(t *testing.T) {
	t.Parallel()
	longTitle := strings.Repeat("x", 200)
	evt := agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "s1",
		Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusIdle},
	}
	n, ok := classify(evt, SessionContext{Title: longTitle})
	if !ok {
		t.Fatal("classify rejected a notification-worthy event")
	}
	if r := []rune(n.Title); len(r) != maxTitleLen {
		t.Errorf("Title len = %d runes, want %d", len(r), maxTitleLen)
	}
	if r := []rune(n.Data["session_title"].(string)); len(r) != maxTitleLen {
		t.Errorf("Data[session_title] len = %d runes, want %d", len(r), maxTitleLen)
	}
}

func TestClassify_ZeroTimestampDefaultsToNow(t *testing.T) {
	t.Parallel()
	evt := agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "s1",
		Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusIdle},
	}
	before := time.Now()
	n, ok := classify(evt, SessionContext{})
	after := time.Now()
	if !ok {
		t.Fatal("expected classify to accept the event")
	}
	if n.OccurredAt.Before(before) || n.OccurredAt.After(after) {
		t.Errorf("OccurredAt = %v, want in [%v, %v]", n.OccurredAt, before, after)
	}
}

// recordingProvider is a real Provider used as a test fixture — it
// captures every Send + Close call. Not a mock; the Loop talks to it
// over the real interface.
type recordingProvider struct {
	mu       sync.Mutex
	sent     []Notification
	closes   int
	sendErr  error
	closeErr error
}

func (p *recordingProvider) Send(_ context.Context, n Notification) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, n)
	return p.sendErr
}

func (p *recordingProvider) Close(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closes++
	return p.closeErr
}

func (p *recordingProvider) sentCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

func (p *recordingProvider) closeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closes
}

func TestLoop_DeliversNotificationWorthyEvents(t *testing.T) {
	t.Parallel()
	rec := &recordingProvider{}
	loop := New(Config{Provider: rec})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = loop.Stop(stopCtx)
	})

	loop.OnEvent(agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "s1",
		Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusIdle},
	})
	loop.OnEvent(agent.Event{Type: agent.EventMessage, SessionID: "s1", Data: agent.MessageData{}}) // dropped
	loop.OnEvent(agent.Event{
		Type:      agent.EventPermission,
		SessionID: "s1",
		Data:      agent.PermissionData{RequestID: "r1", Tool: "bash"},
	})

	if got := waitForSent(rec, 2, 500*time.Millisecond); got != 2 {
		t.Fatalf("got %d notifications, want 2", got)
	}
}

// TestLoop_SessionContextEnrichesDeliveredNotification pins the wiring:
// an installed SessionContextFunc is consulted for notification-worthy
// events only (not for every chatty part/message event — the lookup
// hits the session store and transcript), and its result shows up in
// what the Provider receives.
func TestLoop_SessionContextEnrichesDeliveredNotification(t *testing.T) {
	t.Parallel()
	rec := &recordingProvider{}
	loop := New(Config{Provider: rec})

	var mu sync.Mutex
	lookups := 0
	loop.SetSessionContext(func(_ context.Context, sessionID string) SessionContext {
		mu.Lock()
		lookups++
		mu.Unlock()
		if sessionID != "s1" {
			t.Errorf("lookup for session %q, want s1", sessionID)
		}
		return SessionContext{Title: "Fix login retry", LastAssistantText: "Done, tests pass."}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = loop.Stop(stopCtx)
	})

	loop.OnEvent(agent.Event{Type: agent.EventPartUpdate, SessionID: "s1", Data: agent.PartUpdateData{}}) // dropped, no lookup
	loop.OnEvent(agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "s1",
		Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusIdle},
	})

	if got := waitForSent(rec, 1, 500*time.Millisecond); got != 1 {
		t.Fatalf("got %d notifications, want 1", got)
	}
	rec.mu.Lock()
	n := rec.sent[0]
	rec.mu.Unlock()
	if n.Title != "Fix login retry" {
		t.Errorf("Title = %q, want session title", n.Title)
	}
	if n.Body != "Done, tests pass." {
		t.Errorf("Body = %q, want last-reply preview", n.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if lookups != 1 {
		t.Errorf("lookup count = %d, want 1 (dropped events must not trigger lookups)", lookups)
	}
}

// blockingProvider pauses Send until release is called, then records
// the notification. Used to deterministically queue events in the
// Loop's input channel before signalling Stop, which is what
// exercises the drain path.
type blockingProvider struct {
	released chan struct{}
	mu       sync.Mutex
	sent     []Notification
	closes   int
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{released: make(chan struct{})}
}

func (p *blockingProvider) release() { close(p.released) }

func (p *blockingProvider) Send(ctx context.Context, n Notification) error {
	select {
	case <-p.released:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	p.sent = append(p.sent, n)
	p.mu.Unlock()
	return nil
}

func (p *blockingProvider) Close(_ context.Context) error {
	p.mu.Lock()
	p.closes++
	p.mu.Unlock()
	return nil
}

func (p *blockingProvider) sentCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

// TestLoop_StopDrainsQueuedEvents pins the regression: before the
// drain fix, Run returned immediately on l.stop and dropped any
// events still sitting in l.events. With the drain in place the
// shutdown path delivers every notification-worthy event that was
// already classified-and-enqueued.
func TestLoop_StopDrainsQueuedEvents(t *testing.T) {
	t.Parallel()
	rec := newBlockingProvider()
	loop := New(Config{Provider: rec, Buffer: 4})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	// Three notification-worthy events buffered while the provider
	// is still blocked. None are delivered yet.
	for i := 0; i < 3; i++ {
		loop.OnEvent(agent.Event{
			Type:      agent.EventStatusChange,
			SessionID: "s1",
			Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusIdle},
		})
	}
	// Give the worker time to consume the first event into handle()
	// — that one's blocked on Send. The remaining 2 sit in l.events.
	time.Sleep(20 * time.Millisecond)

	// Release the provider AFTER Stop has been signalled so the
	// blocked Send completes, then the drain runs through the
	// remaining queued events. Stop blocks until done is closed.
	done := make(chan error, 1)
	go func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		done <- loop.Stop(stopCtx)
	}()
	time.Sleep(20 * time.Millisecond)
	rec.release()

	if err := <-done; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := rec.sentCount(); got != 3 {
		t.Errorf("Provider.Send call count = %d, want 3 (drain should deliver every queued event)", got)
	}
}

func TestLoop_StopCallsProviderClose(t *testing.T) {
	t.Parallel()
	rec := &recordingProvider{}
	loop := New(Config{Provider: rec})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := loop.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if rec.closeCount() != 1 {
		t.Errorf("Close call count = %d, want 1", rec.closeCount())
	}
	// Idempotent: a second Stop must not double-call Close.
	if err := loop.Stop(stopCtx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if rec.closeCount() != 1 {
		t.Errorf("after second Stop, Close call count = %d, want 1 (idempotent)", rec.closeCount())
	}
}

func TestLoop_NewPanicsWithoutProvider(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when Provider is nil")
		}
	}()
	_ = New(Config{})
}

func waitForSent(p *recordingProvider, want int, timeout time.Duration) int {
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
