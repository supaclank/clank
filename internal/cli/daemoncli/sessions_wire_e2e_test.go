package daemoncli

// Session-lifecycle wire coverage. Ported from internal/hub/sessions_test.go
// (deleted in PR 3 phase 3c). Each test drives one or two RPCs and
// asserts the host applied them through to the in-process stub
// backend. These pin the small things that are easy to break and
// harder to notice in manual testing — wrong HTTP method, dropped
// body field, response-shape mismatch.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host/hosttest"
)

func TestWire_SendMessage(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, b := td.CreateOpenCodeSession(t, "first")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := td.Client.Session(info.ID).Send(ctx, agent.SendMessageOpts{Text: "follow-up"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := b.LastSendOpts().Text
	if got != "follow-up" {
		t.Errorf("backend.Send received text %q, want %q", got, "follow-up")
	}
}

func TestWire_SendMessageToNonexistentSessionIs404(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := td.Client.Session("does-not-exist").Send(ctx, agent.SendMessageOpts{Text: "x"})
	if err == nil {
		t.Fatal("expected error sending to nonexistent session, got nil")
	}
	assertNotFound(t, err)
}

func TestWire_AbortSession(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, b := td.CreateOpenCodeSession(t, "long task")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := td.Client.Session(info.ID).Abort(ctx); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if !b.Aborted() {
		t.Error("backend.Abort was not called")
	}
}
func TestWire_ForkSession(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, b := td.CreateOpenCodeSession(t, "parent task")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got, err := td.Client.Session(info.ID).Fork(ctx, "msg-7")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if got == nil {
		t.Fatal("Fork returned nil SessionInfo")
	}
	gotID := b.ForkedMessageID()
	if gotID != "msg-7" {
		t.Errorf("backend.Fork message_id = %q, want %q", gotID, "msg-7")
	}
	if got.ID == info.ID {
		t.Errorf("Fork should return a fresh session ID, got the source's: %q", got.ID)
	}
	if strings.HasPrefix(got.ID, "ext-forked-") {
		t.Errorf("Fork returned the backend's external id as SessionInfo.ID: %q (the bug)", got.ID)
	}
	if got.ExternalID != "ext-forked-msg-7" {
		t.Errorf("Fork SessionInfo.ExternalID = %q, want %q", got.ExternalID, "ext-forked-msg-7")
	}
	roundTrip, err := td.Client.Session(got.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession on forked id: %v", err)
	}
	if roundTrip.ID != got.ID {
		t.Errorf("round-trip id mismatch: got %q want %q", roundTrip.ID, got.ID)
	}
}

func TestWire_DeleteSession(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, _ := td.CreateOpenCodeSession(t, "tmp")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := td.Client.Session(info.ID).Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// GET should now fail with not-found specifically — accepting any
	// error would let a 5xx regression sneak through.
	_, err := td.Client.Session(info.ID).Get(ctx)
	if err == nil {
		t.Fatal("expected error fetching deleted session, got nil")
	}
	assertNotFound(t, err)
}

// assertNotFound fails the test if err's message doesn't look like a
// 404 / not-found response. The wire format puts the HTTP status into
// the message, so a substring match is enough — a 502/timeout/etc
// would not contain either token.
func assertNotFound(t *testing.T, err error) {
	t.Helper()
	low := strings.ToLower(err.Error())
	if !strings.Contains(low, "404") && !strings.Contains(low, "not found") {
		t.Fatalf("expected 404/not-found error, got: %v", err)
	}
}

func TestWire_MarkSessionRead(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, _ := td.CreateOpenCodeSession(t, "tmp")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := td.Client.Session(info.ID).MarkRead(ctx); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	got, err := td.Client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("Get after MarkRead: %v", err)
	}
	if got.LastReadAt.IsZero() {
		t.Error("LastReadAt is zero after MarkRead")
	}
}

func TestWire_ToggleFollowUp(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, _ := td.CreateOpenCodeSession(t, "tmp")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	on, err := td.Client.Session(info.ID).ToggleFollowUp(ctx)
	if err != nil {
		t.Fatalf("ToggleFollowUp on: %v", err)
	}
	if !on {
		t.Errorf("toggle 1: got %v, want true", on)
	}
	off, err := td.Client.Session(info.ID).ToggleFollowUp(ctx)
	if err != nil {
		t.Fatalf("ToggleFollowUp off: %v", err)
	}
	if off {
		t.Errorf("toggle 2: got %v, want false", off)
	}
}

func TestWire_SetVisibility(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, _ := td.CreateOpenCodeSession(t, "tmp")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := td.Client.Session(info.ID).SetVisibility(ctx, agent.VisibilityDone); err != nil {
		t.Fatalf("SetVisibility: %v", err)
	}
	got, err := td.Client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Visibility != agent.VisibilityDone {
		t.Errorf("Visibility = %q, want %q", got.Visibility, agent.VisibilityDone)
	}
}

func TestWire_SetDraft(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, _ := td.CreateOpenCodeSession(t, "tmp")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := td.Client.Session(info.ID).SetDraft(ctx, "what i was about to say"); err != nil {
		t.Fatalf("SetDraft: %v", err)
	}
	got, err := td.Client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Draft != "what i was about to say" {
		t.Errorf("Draft = %q, want %q", got.Draft, "what i was about to say")
	}
}

func TestWire_ListSessions_ReflectsCreate(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, _ := td.CreateOpenCodeSession(t, "first")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	all, err := td.Client.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found bool
	for _, s := range all {
		if s.ID == info.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("created session %s missing from List", info.ID)
	}
}

// TestWire_CreateRequiresPrompt pins the validation guard. A wire
// regression dropping the prompt would let an empty session through;
// the host's StartRequest.Validate catches it before the backend even
// spawns.
func TestWire_CreateRequiresPrompt(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	repo := hosttest.InitGitRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := td.Client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: repo, WorktreeID: "git@example.com:x/y.git"},
		Config:  workstationConfig(agent.BackendOpenCode),
		// Prompt intentionally empty.
	})
	if err == nil {
		t.Fatal("expected validation error for empty Prompt, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "prompt") {
		t.Errorf("error should mention prompt; got %v", err)
	}
}

// TestWire_TitleEventUpdatesPersistedSession verifies the host's
// applyEventToMetadata path: when a TitleChange flows through the
// relay, the persisted SessionInfo's Title is updated.
func TestWire_TitleEventUpdatesPersistedSession(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, b := td.CreateOpenCodeSession(t, "tmp")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go b.PushEvent(agent.Event{
		Type:      agent.EventTitleChange,
		Timestamp: time.Now(),
		Data:      agent.TitleChangeData{Title: "Refactor auth flow"},
	})

	// The relay applies the event metadata asynchronously; poll Get
	// until the title shows up or the deadline fires.
	deadline := time.Now().Add(2 * time.Second)
	var got *agent.SessionInfo
	for time.Now().Before(deadline) {
		s, err := td.Client.Session(info.ID).Get(ctx)
		if err == nil && s.Title == "Refactor auth flow" {
			got = s
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("Title never propagated to persisted SessionInfo")
	}
}

// TestWire_GetMessages_Empty pins that the GET /messages route is
// registered and returns successfully on a session with no history —
// it does not assert on the nil-vs-empty-slice shape because the
// daemonclient unmarshals JSON and either form is fine for the TUI's
// range loop. Kept as a smoke test for the route wiring.
func TestWire_GetMessages_Empty(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, _ := td.CreateOpenCodeSession(t, "tmp")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := td.Client.Session(info.ID).Messages(ctx); err != nil {
		// Some backends may return an error here on empty; either is
		// acceptable as long as the wire didn't 404 the route.
		if strings.Contains(err.Error(), "404") {
			t.Fatalf("messages endpoint not registered? got: %v", err)
		}
	}
}

// TestWire_PendingPermission_StubReturnsEmpty pins the post-PR-3
// behavior: the host doesn't snapshot pending permissions yet, but
// the TUI's recovery path calls /pending-permission and we must
// return an empty list (not 404). When a real queue lands in a
// future PR, this test should be expanded to assert on its contents.
func TestWire_PendingPermission_StubReturnsEmpty(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, _ := td.CreateOpenCodeSession(t, "tmp")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	perms, err := td.Client.Session(info.ID).PendingPermissions(ctx)
	if err != nil {
		t.Fatalf("PendingPermissions: %v", err)
	}
	if len(perms) != 0 {
		t.Errorf("stub should return empty; got %d entries", len(perms))
	}
}

// TestWire_PermissionReply pins that the wire actually invokes
// RespondPermission on the backend (POST routes through a path with
// two path-vars — easy to break with a mux refactor).
func TestWire_PermissionReply(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, b := td.CreateOpenCodeSession(t, "tmp")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := td.Client.Session(info.ID).ReplyPermission(ctx, "perm-1", true, ""); err != nil {
		t.Fatalf("ReplyPermission: %v", err)
	}
	called, gotID, gotAllow := b.PermissionReply()
	if !called {
		t.Fatal("backend.RespondPermission was not invoked")
	}
	if gotID != "perm-1" {
		t.Errorf("permission id = %q, want %q", gotID, "perm-1")
	}
	if !gotAllow {
		t.Errorf("permission allow = %v, want true", gotAllow)
	}
}

// TestWire_PermissionReply_RejectsEmptyID pins client-side guard.
func TestWire_PermissionReply_RejectsEmptyID(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, _ := td.CreateOpenCodeSession(t, "tmp")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := td.Client.Session(info.ID).ReplyPermission(ctx, "", true, "")
	if err == nil {
		t.Error("expected error for empty permission id")
	}
}

// TestWire_BackendStatusEventBroadcastsMetaChange pins the row-level
// counterpart of applyEventToMetadata's persist path: when a status
// event lands, subscribers should receive BOTH the original
// EventStatusChange (used by session view for streaming finalization)
// and a paired EventMetaChange carrying the full post-mutation row —
// including a freshly bumped UpdatedAt that the TUI sidebar uses to
// hoist the session to the top.
//
// Regression: before this contract was unified, the sidebar only saw
// the StatusChange and patched the Status field in place, leaving its
// cached UpdatedAt stale. The session row would not reorder until the
// next full List() refetch (which only happened on the user's next
// keystroke into the session).
func TestWire_BackendStatusEventBroadcastsMetaChange(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, b := td.CreateOpenCodeSession(t, "tmp")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Subscribe AFTER CreateSession so we don't have to wade through
	// the create's own MetaChange/StatusChange burst.
	events, err := td.Client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	preBump, err := td.Client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("Get pre-bump: %v", err)
	}
	// time.Now() resolution on macOS is sub-microsecond but the daemon
	// reads its own clock to stamp UpdatedAt; sleep one tick so the
	// post-bump timestamp can be strictly newer.
	time.Sleep(2 * time.Millisecond)

	// Push: Starting → Busy (the canonical "user sent a message"
	// transition). applyEventToMetadata only writes on a status delta,
	// so we must use a value that differs from the session's current
	// Status (Starting after create).
	go b.PushEvent(agent.Event{
		Type:      agent.EventStatusChange,
		Timestamp: time.Now(),
		Data:      agent.StatusChangeData{OldStatus: agent.StatusStarting, NewStatus: agent.StatusBusy},
	})

	gotStatus, drained := receiveEventsByType(t, events, agent.EventStatusChange, 2*time.Second)
	if gotStatus == nil {
		t.Fatalf("no EventStatusChange received; drained: %d events", len(drained))
	}
	gotMeta, drained := receiveEventsByType(t, events, agent.EventMetaChange, 2*time.Second)
	if gotMeta == nil {
		t.Fatalf("no EventMetaChange received after status flip; this is the bug. Drained: %d events", len(drained))
	}

	data, ok := gotMeta.Data.(agent.MetaChangeData)
	if !ok {
		t.Fatalf("EventMetaChange.Data type = %T, want MetaChangeData", gotMeta.Data)
	}
	if data.Session.ID != info.ID {
		t.Errorf("MetaChange Session.ID = %q, want %q", data.Session.ID, info.ID)
	}
	if data.Session.Status != agent.StatusBusy {
		t.Errorf("MetaChange Session.Status = %v, want Busy (the post-mutation value)", data.Session.Status)
	}
	if !data.Session.UpdatedAt.After(preBump.UpdatedAt) {
		t.Errorf("MetaChange Session.UpdatedAt = %v, want strictly after pre-bump %v (sidebar needs this for hoist)",
			data.Session.UpdatedAt, preBump.UpdatedAt)
	}
}

// TestWire_BackendTitleEventBroadcastsMetaChange is the title-flavored
// counterpart of TestWire_BackendStatusEventBroadcastsMetaChange.
// Title changes are rarer than status flips but follow the same
// contract: persist the row, then broadcast EventMetaChange.
func TestWire_BackendTitleEventBroadcastsMetaChange(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, b := td.CreateOpenCodeSession(t, "tmp")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := td.Client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	preBump, err := td.Client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("Get pre-bump: %v", err)
	}
	time.Sleep(2 * time.Millisecond)

	go b.PushEvent(agent.Event{
		Type:      agent.EventTitleChange,
		Timestamp: time.Now(),
		Data:      agent.TitleChangeData{Title: "Refactor auth flow"},
	})

	gotTitle, drained := receiveEventsByType(t, events, agent.EventTitleChange, 2*time.Second)
	if gotTitle == nil {
		t.Fatalf("no EventTitleChange received; drained: %d events", len(drained))
	}
	gotMeta, drained := receiveEventsByType(t, events, agent.EventMetaChange, 2*time.Second)
	if gotMeta == nil {
		t.Fatalf("no EventMetaChange received after title change. Drained: %d events", len(drained))
	}

	data, ok := gotMeta.Data.(agent.MetaChangeData)
	if !ok {
		t.Fatalf("EventMetaChange.Data type = %T, want MetaChangeData", gotMeta.Data)
	}
	if data.Session.Title != "Refactor auth flow" {
		t.Errorf("MetaChange Session.Title = %q, want %q", data.Session.Title, "Refactor auth flow")
	}
	if !data.Session.UpdatedAt.After(preBump.UpdatedAt) {
		t.Errorf("MetaChange Session.UpdatedAt = %v, want strictly after pre-bump %v",
			data.Session.UpdatedAt, preBump.UpdatedAt)
	}
}

// TestWire_BackendStartingEventDoesNotBumpOrBroadcast pins that
// StatusStarting is treated as transient: applyEventToMetadata neither
// persists it as the row's Status, bumps UpdatedAt, nor broadcasts
// EventMetaChange. The raw EventStatusChange still flows through to
// SSE subscribers (session view needs it for its "Connecting..." UI).
//
// Regression: without this, every backend lazy-resume (e.g. on
// session-view open after a daemon restart) would broadcast an
// EventMetaChange that hoists + spinners the session in the sidebar
// even though the user did nothing. The 1-2s SDK connection window
// would briefly show a "busy" spinner that wasn't tied to any agent
// work.
func TestWire_BackendStartingEventDoesNotBumpOrBroadcast(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, b := td.CreateOpenCodeSession(t, "tmp")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := td.Client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	preBump, err := td.Client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("Get pre-event: %v", err)
	}

	// Push a Starting event — the kind that fires when ensureBackend
	// lazy-creates a wrapper and Open() runs.
	go b.PushEvent(agent.Event{
		Type:      agent.EventStatusChange,
		Timestamp: time.Now(),
		Data:      agent.StatusChangeData{OldStatus: agent.StatusIdle, NewStatus: agent.StatusStarting},
	})

	// The StatusChange event itself MUST still arrive on the wire —
	// session view consumers depend on it.
	gotStatus, drained := receiveEventsByType(t, events, agent.EventStatusChange, 2*time.Second)
	if gotStatus == nil {
		t.Fatalf("EventStatusChange(Starting) didn't propagate; drained=%d", len(drained))
	}

	// But NO EventMetaChange should follow. Wait long enough for one to
	// have arrived if it were going to (the host's upsert + broadcast
	// path completes in well under 500ms in tests).
	gotMeta, drainedMeta := receiveEventsByType(t, events, agent.EventMetaChange, 500*time.Millisecond)
	if gotMeta != nil {
		t.Errorf("Starting status spuriously broadcast EventMetaChange (sidebar would hoist + spin); drained=%d", len(drainedMeta))
	}

	// Persisted row should be unchanged — same Status, same UpdatedAt.
	postEvent, err := td.Client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("Get post-event: %v", err)
	}
	if postEvent.Status != preBump.Status {
		t.Errorf("Status changed by Starting event (pre=%v post=%v); want unchanged",
			preBump.Status, postEvent.Status)
	}
	if !postEvent.UpdatedAt.Equal(preBump.UpdatedAt) {
		t.Errorf("UpdatedAt bumped by Starting event (pre=%v post=%v); want unchanged",
			preBump.UpdatedAt, postEvent.UpdatedAt)
	}
}

// TestWire_StatusEventNormalizesPersistedStatus verifies that an
// idle status event from the backend (the canonical "task complete"
// signal) is reflected in the persisted SessionInfo. Without this,
// the inbox would show a session as busy forever after a task done.
func TestWire_StatusEventNormalizesPersistedStatus(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, b := td.CreateOpenCodeSession(t, "tmp")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Push: starting → busy → idle. We assert on the final state.
	go func() {
		b.PushEvent(agent.Event{
			Type:      agent.EventStatusChange,
			Timestamp: time.Now(),
			Data:      agent.StatusChangeData{OldStatus: agent.StatusStarting, NewStatus: agent.StatusBusy},
		})
		b.PushEvent(agent.Event{
			Type:      agent.EventStatusChange,
			Timestamp: time.Now(),
			Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusIdle},
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	var last *agent.SessionInfo
	for time.Now().Before(deadline) {
		s, err := td.Client.Session(info.ID).Get(ctx)
		if err == nil && s.Status == agent.StatusIdle {
			last = s
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if last == nil {
		t.Errorf("status never converged to idle")
	}
}
