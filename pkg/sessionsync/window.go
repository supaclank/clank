package sessionsync

import (
	"time"

	"github.com/acksell/clank/internal/agent"
)

// SessionPushWindow bounds which NEVER-synced sessions a push carries: only
// those updated within the window. Sessions already in the synced record are
// always carried regardless of age (an established worktree keeps syncing its
// full history). Keeps a first push of a long-lived worktree from shipping
// years of stale sessions.
const SessionPushWindow = 14 * 24 * time.Hour

// PushCutoff is the oldest UpdatedAt a never-synced session may carry to still
// be pushed: now minus SessionPushWindow. Feed the result to WithinPushWindow.
func PushCutoff() time.Time {
	return time.Now().Add(-SessionPushWindow)
}

// WithinPushWindow keeps the sessions a push should carry: those updated after
// cutoff, plus any already in the synced record (previously synced — kept
// regardless of age, so the window never drops something already up there). A
// zero cutoff keeps everything (no window). Pure; the cutoff is passed in so
// callers and tests control "now".
func WithinPushWindow(all []DiscoveredSession, rec agent.SyncedSessionRecord, cutoff time.Time) []DiscoveredSession {
	if cutoff.IsZero() {
		return all
	}
	out := make([]DiscoveredSession, 0, len(all))
	for _, s := range all {
		if _, synced := rec.Sessions[s.ExternalID]; synced || s.UpdatedAt.After(cutoff) {
			out = append(out, s)
		}
	}
	return out
}
