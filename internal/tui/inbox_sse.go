package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
)

// Push-based sidebar updates. The inbox subscribes to the daemon's
// global SSE stream and mutates m.cachedSessions in response to three
// row-level events:
//   - EventMetaChange: replace the row wholesale. Every persisted
//     SessionInfo mutation broadcasts one — status flips, title
//     updates, mark-read, draft, visibility, follow-up — so the
//     sidebar's sort key (UpdatedAt) and unread state (LastReadAt vs
//     UpdatedAt) stay in lockstep with the daemon's DB.
//   - EventSessionCreate: insert from the event payload (no refetch).
//   - EventSessionDelete: drop inline.
//
// The sidebar deliberately does NOT subscribe to the field-level
// EventStatusChange / EventTitleChange. Each persisted mutation emits
// those first (as a transition signal — Old → New status, new title
// string) for clients that need them, then emits EventMetaChange with
// the full post-mutation row. Listening to the field-level events
// here would patch one field but miss the bumped UpdatedAt, leaving
// the sort stale (see git history: the symptom was sessions not
// hoisting on send).
//
// The session view in sessionview.go is the other consumer of this
// SSE stream and DOES listen to EventStatusChange / EventTitleChange:
// it needs the Old→New transition to finalize streaming UI when an
// agent goes Busy→Idle, and it benefits from the pre-upsert broadcast
// latency (no DB-write gate on the "agent finished" UI). Future
// consolidation onto EventMetaChange would need to weigh that lag.
//
// On stream close (daemon restart, network), we reconnect with
// exponential backoff and resync via loadDataCmd() so any events
// missed during the gap are not lost.

// inboxSubscribeBackoffInitial is the first reconnect delay after an
// SSE drop. Grows by 2x up to inboxSubscribeBackoffMax.
const (
	inboxSubscribeBackoffInitial = 250 * time.Millisecond
	inboxSubscribeBackoffMax     = 5 * time.Second
)

type inboxSSESetupMsg struct {
	events <-chan agent.Event
	cancel context.CancelFunc
}

type inboxSSEEventMsg struct {
	event agent.Event
}

// inboxSSEClosedMsg signals the stream ended and we should reconnect.
type inboxSSEClosedMsg struct {
	delay time.Duration
}

type inboxSSEErrorMsg struct {
	err   error
	delay time.Duration
}

// subscribeInboxEvents opens the SSE stream. Used at startup and on
// reconnect.
func (m *InboxModel) subscribeInboxEvents() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(context.Background())
		events, err := client.Sessions().Subscribe(ctx)
		if err != nil {
			cancel()
			return inboxSSEErrorMsg{err: err, delay: inboxSubscribeBackoffInitial}
		}
		return inboxSSESetupMsg{events: events, cancel: cancel}
	}
}

// waitForInboxEvent blocks on the channel and emits one event message.
// On channel close, returns a ClosedMsg to drive reconnect.
func waitForInboxEvent(events <-chan agent.Event) tea.Cmd {
	return func() tea.Msg {
		evt, ok := <-events
		if !ok {
			return inboxSSEClosedMsg{delay: inboxSubscribeBackoffInitial}
		}
		return inboxSSEEventMsg{event: evt}
	}
}

// reconnectInboxEvents schedules a subscribe attempt after delay.
func (m *InboxModel) reconnectInboxEvents(delay time.Duration) tea.Cmd {
	if delay > inboxSubscribeBackoffMax {
		delay = inboxSubscribeBackoffMax
	}
	return tea.Tick(delay, func(time.Time) tea.Msg {
		return inboxRefreshSubscribeMsg{nextDelay: delay * 2}
	})
}

// inboxRefreshSubscribeMsg fires after the backoff tick; carries the
// next delay if reconnect fails again.
type inboxRefreshSubscribeMsg struct {
	nextDelay time.Duration
}

// applyInboxEvent updates m.cachedSessions in response to a single SSE
// event. Returns true if the sidebar needs a redraw. Returns a
// follow-up Cmd if a full refetch is needed (delete: row may have been
// in a filtered-out state we can't reconstruct).
func (m *InboxModel) applyInboxEvent(evt agent.Event) (changed bool, cmd tea.Cmd) {
	switch evt.Type {
	case agent.EventSessionCreate:
		// Insert the new row directly from the event payload — avoids
		// the extra List() round-trip so the sidebar shows the new
		// session the instant the daemon registers it. Dedup against
		// any racing List() that already inserted the row.
		data, ok := evt.Data.(agent.MetaChangeData)
		if !ok {
			return false, m.loadDataCmd()
		}
		for i := range m.cachedSessions {
			if m.cachedSessions[i].ID == data.Session.ID {
				m.cachedSessions[i] = data.Session
				return true, nil
			}
		}
		m.cachedSessions = append(m.cachedSessions, data.Session)
		return true, nil

	case agent.EventSessionDelete:
		// Drop the row inline; refetch as a safety net so any sibling
		// rows we missed (e.g. cascade-deleted worktree owners) reconcile.
		removed := false
		for i := range m.cachedSessions {
			if m.cachedSessions[i].ID == evt.SessionID {
				m.cachedSessions = append(m.cachedSessions[:i], m.cachedSessions[i+1:]...)
				removed = true
				break
			}
		}
		return removed, nil

	case agent.EventMetaChange:
		data, ok := evt.Data.(agent.MetaChangeData)
		if !ok {
			return false, nil
		}
		return m.replaceCachedSession(data.Session), nil
	}
	// EventStatusChange / EventTitleChange are intentionally ignored
	// here — the server emits a paired EventMetaChange with the full
	// post-mutation row (incl. fresh UpdatedAt) which we handle above.
	// See package doc for the rationale.
	return false, nil
}

// replaceCachedSession swaps the row with matching ID. Returns true on
// success. Skips if the session isn't in our cache (e.g. filtered out
// by visibility — the next list refresh will reconcile).
func (m *InboxModel) replaceCachedSession(info agent.SessionInfo) bool {
	for i := range m.cachedSessions {
		if m.cachedSessions[i].ID == info.ID {
			m.cachedSessions[i] = info
			return true
		}
	}
	return false
}

// refreshSidebarFromCache pushes the current cachedSessions into the
// sidebar and rebuilds groups when no search filter is active. Used
// by the SSE event handler so a single in-place mutation feeds both
// sidebar and inbox views without a daemon round-trip.
func (m *InboxModel) refreshSidebarFromCache() {
	m.sidebar.SetSessions(m.cachedSessions)
	m.sidebar.UpdateWorktreeOwnersFromSessions(m.cachedSessions)
	if m.searchQuery == "" {
		m.buildGroups(m.filteredSessions())
	}
}

// persistCacheIfChanged writes cachedSessions to disk when its
// signature differs from the last persisted snapshot. Called from
// both the loadDataCmd-result handler and the SSE-event handler so
// any state visible to the running TUI also survives restart. The
// write is async (goroutine) because callers run on the bubbletea
// event loop and disk I/O must not block UI updates.
func (m *InboxModel) persistCacheIfChanged() {
	sig := sessionsCacheSig(m.cachedSessions)
	if sig == m.lastSessionsCacheSig {
		return
	}
	m.lastSessionsCacheSig = sig
	snap := append([]agent.SessionInfo(nil), m.cachedSessions...)
	// TODO(coderabbit): serialize cache writes so bursty SSE can't land an older snapshot last https://github.com/Acksell/clank/pull/33#discussion_r3293211742
	go func() { _ = saveSessionsCache(snap) }()
}
