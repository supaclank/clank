package tui

import (
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// TestInboxSSE_MetaChangeReplacesCachedRow verifies an EventMetaChange
// for a session in the cache replaces the row in place — so MarkRead
// over SSE makes the unread asterisk disappear immediately without a
// List() refetch. Regression: before push updates, the inbox waited
// up to 3 seconds for the next poll.
func TestInboxSSE_MetaChangeReplacesCachedRow(t *testing.T) {
	t.Parallel()
	m := NewInboxModel(nil)
	now := time.Now()
	m.cachedSessions = []agent.SessionInfo{
		{ID: "s1", Title: "t1", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: time.Time{}},
	}

	read := now.Add(5 * time.Second)
	updated := m.cachedSessions[0]
	updated.LastReadAt = read

	changed, cmd := m.applyInboxEvent(agent.Event{
		Type:      agent.EventMetaChange,
		SessionID: "s1",
		Data:      agent.MetaChangeData{Session: updated},
	})

	if !changed {
		t.Fatal("applyInboxEvent reported no change; want true")
	}
	if cmd != nil {
		t.Errorf("EventMetaChange should not trigger refetch cmd; got %T", cmd)
	}
	if got := m.cachedSessions[0].LastReadAt; !got.Equal(read) {
		t.Errorf("LastReadAt = %v, want %v", got, read)
	}
}

// TestInboxSSE_StatusChangeIgnoredBySidebar pins the unified contract:
// the sidebar no longer reacts to a lone EventStatusChange. The server
// emits a paired EventMetaChange with the full post-mutation row (incl.
// the bumped UpdatedAt and the new Status), which is what the sidebar
// listens to. Listening here too would patch one field but miss
// UpdatedAt and leave the sort stale — that was the bug.
//
// Regression: removing this no-op for StatusChange (e.g. "innocently
// add a Status patch for the spinner") would reintroduce the stale-
// sort bug.
func TestInboxSSE_StatusChangeIgnoredBySidebar(t *testing.T) {
	t.Parallel()
	m := NewInboxModel(nil)
	m.cachedSessions = []agent.SessionInfo{
		{ID: "s1", Status: agent.StatusIdle},
	}

	changed, cmd := m.applyInboxEvent(agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "s1",
		Data:      agent.StatusChangeData{OldStatus: agent.StatusIdle, NewStatus: agent.StatusBusy},
	})

	if changed {
		t.Error("EventStatusChange should not mark sidebar dirty; the paired EventMetaChange does")
	}
	if cmd != nil {
		t.Errorf("EventStatusChange should not trigger a cmd; got %T", cmd)
	}
	if got := m.cachedSessions[0].Status; got != agent.StatusIdle {
		t.Errorf("Status mutated by lone EventStatusChange (= %v); want unchanged Idle", got)
	}
}

// TestInboxSSE_TitleChangeIgnoredBySidebar is the title counterpart of
// TestInboxSSE_StatusChangeIgnoredBySidebar. Same reasoning: sidebar
// learns about titles via the paired EventMetaChange, not the
// transition-shaped EventTitleChange.
func TestInboxSSE_TitleChangeIgnoredBySidebar(t *testing.T) {
	t.Parallel()
	m := NewInboxModel(nil)
	m.cachedSessions = []agent.SessionInfo{
		{ID: "s1", Title: "old"},
	}

	changed, _ := m.applyInboxEvent(agent.Event{
		Type:      agent.EventTitleChange,
		SessionID: "s1",
		Data:      agent.TitleChangeData{Title: "new"},
	})

	if changed {
		t.Error("EventTitleChange should not mark sidebar dirty; the paired EventMetaChange does")
	}
	if got := m.cachedSessions[0].Title; got != "old" {
		t.Errorf("Title mutated by lone EventTitleChange (= %q); want unchanged %q", got, "old")
	}
}

// TestInboxSSE_MetaChangeHoistsSessionInSidebar pins the user-visible
// fix: an EventMetaChange with a bumped UpdatedAt causes the sidebar to
// re-sort so the touched session's worktree floats to the top.
//
// Regression: the original bug was the sidebar's cached row had a stale
// UpdatedAt (because the per-field EventStatusChange handler patched
// only Status), so SetSessions's worktree sort (sidebar_tree.go) kept
// the row in its old position. This test fails if either:
//  1. applyInboxEvent for EventMetaChange stops updating UpdatedAt, or
//  2. refreshSidebarFromCache stops calling sidebar.SetSessions, or
//  3. the worktree sort key drifts away from LatestUpdatedAt.
func TestInboxSSE_MetaChangeHoistsSessionInSidebar(t *testing.T) {
	t.Parallel()
	m := NewInboxModel(nil)
	// Fix the sidebar clock so age-based bucketing is deterministic.
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	m.sidebar.nowFn = fixedNow(now)

	// Two sessions on different worktrees. s1 is newer than s2 →
	// worktree-A (s1's) should sort above worktree-B (s2's).
	m.cachedSessions = []agent.SessionInfo{
		{ID: "s1", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/A"}},
		{ID: "s2", UpdatedAt: now.Add(-1 * time.Hour), GitRef: agent.GitRef{LocalPath: "/r/B"}},
	}
	m.refreshSidebarFromCache()

	firstWtBefore := firstWorktreeKey(m.sidebar.flat)
	if firstWtBefore != "wt:/r/A" {
		t.Fatalf("precondition: first worktree should be A (newer); got %q. flat=%v",
			firstWtBefore, keysOf(m.sidebar.flat))
	}

	// Now s2's session activity bumps it past s1 — the unified
	// EventMetaChange path the server emits after applyEventToMetadata
	// upserts a fresh UpdatedAt.
	updated := m.cachedSessions[1]
	updated.UpdatedAt = now.Add(1 * time.Minute)
	changed, _ := m.applyInboxEvent(agent.Event{
		Type:      agent.EventMetaChange,
		SessionID: "s2",
		Data:      agent.MetaChangeData{Session: updated},
	})
	if !changed {
		t.Fatal("EventMetaChange did not mark sidebar dirty")
	}
	m.refreshSidebarFromCache()

	firstWtAfter := firstWorktreeKey(m.sidebar.flat)
	if firstWtAfter != "wt:/r/B" {
		t.Errorf("worktree B did not hoist after MetaChange; first worktree = %q. flat=%v",
			firstWtAfter, keysOf(m.sidebar.flat))
	}
}

// firstWorktreeKey returns the Key() of the first worktree node in the
// sidebar's flat list (the row immediately under AllSessions). Skips
// non-worktree rows (AllSessions, footers) so the assertion is robust
// against unrelated rows reshuffling.
func firstWorktreeKey(flat []sidebarNode) string {
	for _, n := range flat {
		if n.Kind() == nodeWorktree {
			return n.Key()
		}
	}
	return ""
}

// TestInboxSSE_SessionCreateInsertsRowFromPayload verifies a create
// event inserts the row directly from the event payload instead of
// triggering a List() round-trip. Regression: an extra HTTP fetch on
// session create made the sidebar lag visibly behind the compose
// view's "Created" transition by hundreds of ms.
func TestInboxSSE_SessionCreateInsertsRowFromPayload(t *testing.T) {
	t.Parallel()
	m := NewInboxModel(nil)
	m.cachedSessions = nil

	info := agent.SessionInfo{ID: "s-new", Status: agent.StatusStarting, Title: "fresh"}
	changed, cmd := m.applyInboxEvent(agent.Event{
		Type:      agent.EventSessionCreate,
		SessionID: info.ID,
		Data:      agent.MetaChangeData{Session: info},
	})

	if !changed {
		t.Fatal("create event did not mark sidebar dirty")
	}
	if cmd != nil {
		t.Errorf("create with payload should not trigger refetch; got %T", cmd)
	}
	if len(m.cachedSessions) != 1 || m.cachedSessions[0].ID != "s-new" {
		t.Errorf("new session not inserted; len=%d", len(m.cachedSessions))
	}
}

// TestInboxSSE_SessionCreateDedupes verifies a create event for an
// already-known session updates in place instead of duplicating the
// row — guards against a race where loadDataCmd() lands between
// CreateSession returning and the SSE event arriving.
func TestInboxSSE_SessionCreateDedupes(t *testing.T) {
	t.Parallel()
	m := NewInboxModel(nil)
	m.cachedSessions = []agent.SessionInfo{{ID: "s1", Status: agent.StatusStarting}}

	m.applyInboxEvent(agent.Event{
		Type:      agent.EventSessionCreate,
		SessionID: "s1",
		Data:      agent.MetaChangeData{Session: agent.SessionInfo{ID: "s1", Status: agent.StatusIdle}},
	})

	if len(m.cachedSessions) != 1 {
		t.Errorf("duplicate row inserted; len=%d", len(m.cachedSessions))
	}
	if m.cachedSessions[0].Status != agent.StatusIdle {
		t.Errorf("Status not updated by dedupe; got %v", m.cachedSessions[0].Status)
	}
}

// TestInboxSSE_SessionDeleteRemovesRow verifies delete events drop the
// row inline so the sidebar updates instantly.
func TestInboxSSE_SessionDeleteRemovesRow(t *testing.T) {
	t.Parallel()
	m := NewInboxModel(nil)
	m.cachedSessions = []agent.SessionInfo{{ID: "s1"}, {ID: "s2"}}

	changed, _ := m.applyInboxEvent(agent.Event{Type: agent.EventSessionDelete, SessionID: "s1"})

	if !changed {
		t.Fatal("delete event did not mark sidebar dirty")
	}
	if len(m.cachedSessions) != 1 || m.cachedSessions[0].ID != "s2" {
		t.Errorf("row not removed; cachedSessions = %+v", m.cachedSessions)
	}
}

// TestInboxSSE_UnknownSessionMetaChangeNoOp verifies a meta-change for
// a session not in our cache is a no-op (no panic, no spurious row).
// This can legitimately happen when the daemon broadcasts updates for
// sessions filtered out of our view (e.g. wrong project filter).
func TestInboxSSE_UnknownSessionMetaChangeNoOp(t *testing.T) {
	t.Parallel()
	m := NewInboxModel(nil)
	m.cachedSessions = []agent.SessionInfo{{ID: "s1"}}

	changed, _ := m.applyInboxEvent(agent.Event{
		Type:      agent.EventMetaChange,
		SessionID: "s2",
		Data:      agent.MetaChangeData{Session: agent.SessionInfo{ID: "s2"}},
	})
	if changed {
		t.Errorf("unknown session change reported as changed; sidebar would redraw unnecessarily")
	}
	if len(m.cachedSessions) != 1 {
		t.Errorf("cachedSessions grew from unknown session; len=%d", len(m.cachedSessions))
	}
}

// TestInboxSSE_MetaChangePersistsCache verifies that a MarkRead
// arriving over SSE survives a TUI restart. Regression: before this
// fix the disk cache was only written from loadDataCmd's success path,
// so any LastReadAt bump that arrived purely via EventMetaChange was
// lost on process exit — the next launch read the pre-MarkRead snapshot
// and rendered the row as unread indefinitely even though the daemon
// SQLite row had the correct LastReadAt > UpdatedAt.
func TestInboxSSE_MetaChangePersistsCache(t *testing.T) {
	// Not Parallel: mutates the CLANK_DIR env var.
	t.Setenv("CLANK_DIR", t.TempDir())

	m := NewInboxModel(nil)
	now := time.Now()
	m.cachedSessions = []agent.SessionInfo{
		{ID: "s1", Title: "t1", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: time.Time{}},
	}

	read := now.Add(5 * time.Second)
	updated := m.cachedSessions[0]
	updated.LastReadAt = read

	changed, _ := m.applyInboxEvent(agent.Event{
		Type:      agent.EventMetaChange,
		SessionID: "s1",
		Data:      agent.MetaChangeData{Session: updated},
	})
	if !changed {
		t.Fatal("applyInboxEvent reported no change")
	}
	m.persistCacheIfChanged()

	// persistCacheIfChanged writes asynchronously; poll briefly so
	// the test doesn't flake on slow CI disks. 1s is generous —
	// json.Marshal + atomic rename for one row is microseconds.
	var saved []agent.SessionInfo
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var err error
		saved, err = loadSessionsCache()
		if err == nil && len(saved) == 1 && !saved[0].LastReadAt.IsZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(saved) != 1 {
		t.Fatalf("loadSessionsCache: got %d rows, want 1", len(saved))
	}
	if got := saved[0].LastReadAt; !got.Equal(read) {
		t.Errorf("persisted LastReadAt = %v, want %v", got, read)
	}
}
