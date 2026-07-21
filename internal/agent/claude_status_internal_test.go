package agent

import "testing"

// TestMarkModelActive_OnlyFlipsFromIdleOrError pins the atomic check-and-set
// contract in markModelActive: the flip to Busy must only ever happen from
// Idle or Error, and every other status is left untouched.
//
// Before the fix, the check (read status) and the act (setStatus(Busy)) were
// two separate lock acquisitions, so a status change landing in that gap
// (e.g. a concurrent receiveLoop tail marking the session Dead during a
// Revert) could be silently overwritten back to Busy. Collapsing both into
// one lock cycle closes that window; this test pins the resulting contract
// directly (white-box: SessionStatus isn't otherwise settable without
// driving a full transport lifecycle).
func TestMarkModelActive_OnlyFlipsFromIdleOrError(t *testing.T) {
	t.Parallel()

	statuses := []SessionStatus{StatusStarting, StatusBusy, StatusIdle, StatusError, StatusDead}
	for _, from := range statuses {
		from := from
		t.Run(string(from), func(t *testing.T) {
			t.Parallel()
			b := &ClaudeCodeBackend{status: from, events: make(chan Event, 1)}
			b.markModelActive()

			wantBusy := from == StatusIdle || from == StatusError
			got := b.Status()
			if wantBusy && got != StatusBusy {
				t.Errorf("markModelActive from %s: got %s, want %s", from, got, StatusBusy)
			}
			if !wantBusy && got != from {
				t.Errorf("markModelActive from %s must leave status untouched: got %s, want %s", from, got, from)
			}
		})
	}
}
