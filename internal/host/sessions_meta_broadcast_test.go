package host_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/host"
	"github.com/supaclank/clank/internal/host/store"
)

// TestSessionsMetaBroadcasts_MarkRead verifies MarkSessionRead emits an
// EventMetaChange so SSE-connected sidebars can update without polling.
// Regression: before this change, mark-read mutations stayed invisible
// to other clients until a periodic List() refresh.
func TestSessionsMetaBroadcasts_MarkRead(t *testing.T) {
	t.Parallel()
	svc, st := newServiceWithStore(t)

	const id = "sess-mark-read"
	must(t, svc.UpsertSessionMetadata(context.Background(), agent.SessionInfo{
		ID:        id,
		Backend:   agent.BackendOpenCode,
		Status:    agent.StatusIdle,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}))

	subID, ch := svc.Subscribe()
	defer svc.Unsubscribe(subID)

	if err := svc.MarkSessionRead(context.Background(), id); err != nil {
		t.Fatalf("MarkSessionRead: %v", err)
	}

	evt := receiveEvent(t, ch, agent.EventMetaChange)
	if evt.SessionID != id {
		t.Fatalf("SessionID = %q, want %q", evt.SessionID, id)
	}
	data, ok := evt.Data.(agent.MetaChangeData)
	if !ok {
		t.Fatalf("Data type = %T, want agent.MetaChangeData", evt.Data)
	}
	if data.Session.LastReadAt.IsZero() {
		t.Errorf("LastReadAt is zero; want set by MarkSessionRead")
	}
	if data.Session.ID != id {
		t.Errorf("Session.ID = %q, want %q", data.Session.ID, id)
	}
	_ = st
}

// TestSessionsMetaBroadcasts_DeleteSession verifies session deletion
// emits an EventSessionDelete so sidebars can remove the row without
// a refetch round-trip.
func TestSessionsMetaBroadcasts_DeleteSession(t *testing.T) {
	t.Parallel()
	svc, _ := newServiceWithStore(t)

	const id = "sess-delete"
	must(t, svc.UpsertSessionMetadata(context.Background(), agent.SessionInfo{
		ID:        id,
		Backend:   agent.BackendOpenCode,
		Status:    agent.StatusIdle,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}))

	subID, ch := svc.Subscribe()
	defer svc.Unsubscribe(subID)

	if err := svc.DeleteSessionMetadata(context.Background(), id); err != nil {
		t.Fatalf("DeleteSessionMetadata: %v", err)
	}

	evt := receiveEvent(t, ch, agent.EventSessionDelete)
	if evt.SessionID != id {
		t.Fatalf("SessionID = %q, want %q", evt.SessionID, id)
	}
}

// TestSessionsMetaBroadcasts_ToggleFollowUp verifies follow-up toggles
// emit EventMetaChange carrying the post-mutation row.
func TestSessionsMetaBroadcasts_ToggleFollowUp(t *testing.T) {
	t.Parallel()
	svc, _ := newServiceWithStore(t)

	const id = "sess-follow"
	must(t, svc.UpsertSessionMetadata(context.Background(), agent.SessionInfo{
		ID:        id,
		Backend:   agent.BackendOpenCode,
		Status:    agent.StatusIdle,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}))

	subID, ch := svc.Subscribe()
	defer svc.Unsubscribe(subID)

	if _, err := svc.ToggleSessionFollowUp(context.Background(), id); err != nil {
		t.Fatalf("ToggleSessionFollowUp: %v", err)
	}

	evt := receiveEvent(t, ch, agent.EventMetaChange)
	data := evt.Data.(agent.MetaChangeData)
	if !data.Session.FollowUp {
		t.Errorf("Session.FollowUp = false, want true")
	}
}

func newServiceWithStore(t *testing.T) (*host.Service, *store.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "host.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{},
		SessionsStore:   st,
	})
	t.Cleanup(svc.Shutdown)
	return svc, st
}

func receiveEvent(t *testing.T, ch <-chan agent.Event, want agent.EventType) agent.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				t.Fatalf("event channel closed before %s", want)
			}
			if evt.Type == want {
				return evt
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", want)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
