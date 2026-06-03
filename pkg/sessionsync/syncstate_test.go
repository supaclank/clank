package sessionsync

import (
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// TestUnsynced pins the diff: a session is unsynced when absent from the
// record or when its UpdatedAt has advanced; unchanged compares equal
// (strict After).
func TestUnsynced(t *testing.T) {
	t.Parallel()
	t0, t1 := time.UnixMilli(1000), time.UnixMilli(2000)
	rec := agent.SyncedSessionRecord{Sessions: map[string]agent.SyncedSession{
		"a": {Backend: agent.BackendOpenCode, UpdatedAt: t0, ContentHash: "h-a"},
		"b": {Backend: agent.BackendOpenCode, UpdatedAt: t1, ContentHash: "h-b"},
	}}
	cur := []DiscoveredSession{
		{ExternalID: "a", UpdatedAt: t0},                  // unchanged
		{ExternalID: "b", UpdatedAt: t1.Add(time.Second)}, // newer → unsynced
		{ExternalID: "c", UpdatedAt: t0},                  // new → unsynced
	}
	if got := Unsynced(cur, rec); len(got) != 2 {
		t.Errorf("Unsynced = %d sessions, want 2", len(got))
	}
	if got := Unsynced([]DiscoveredSession{{ExternalID: "a", UpdatedAt: t0}}, rec); len(got) != 0 {
		t.Errorf("unchanged session should be 0, got %d", len(got))
	}
}

// TestChanged_MissingContentHash pins the content-address guard: a recorded
// session with no ContentHash (never pushed under the content-addressed
// scheme) counts as changed, so it gets re-exported and the rebuilt
// manifest never references it by an empty hash.
func TestChanged_MissingContentHash(t *testing.T) {
	t.Parallel()
	t0 := time.UnixMilli(1000)
	prev := agent.SyncedSession{Backend: agent.BackendOpenCode, UpdatedAt: t0} // no ContentHash
	cur := DiscoveredSession{ExternalID: "a", UpdatedAt: t0}                   // same mtime
	if !Changed(cur, prev) {
		t.Error("missing ContentHash must count as changed")
	}
}

// TestUnsynced_FingerprintBeatsMtime is the regression test for the
// read-only-`--resume` drift bug: when both sides carry a content
// fingerprint (Claude), an advanced mtime with an UNCHANGED fingerprint must
// NOT count as unsynced, and a changed fingerprint must count even if the
// mtime didn't move.
func TestUnsynced_FingerprintBeatsMtime(t *testing.T) {
	t.Parallel()
	t0, later := time.UnixMilli(1000), time.UnixMilli(9999)
	rec := agent.SyncedSessionRecord{Sessions: map[string]agent.SyncedSession{
		"claude-a": {Backend: agent.BackendClaudeCode, UpdatedAt: t0, Fingerprint: "uuid-1", ContentHash: "h-1"},
	}}

	// mtime bumped by a read-only resume, fingerprint identical → in sync.
	noiseBump := []DiscoveredSession{
		{ExternalID: "claude-a", Backend: agent.BackendClaudeCode, UpdatedAt: later, Fingerprint: "uuid-1"},
	}
	if got := Unsynced(noiseBump, rec); len(got) != 0 {
		t.Errorf("fingerprint unchanged but flagged unsynced (mtime bump leaked through): %d", len(got))
	}

	// Genuine new turn: fingerprint changed even with the same mtime.
	realChange := []DiscoveredSession{
		{ExternalID: "claude-a", Backend: agent.BackendClaudeCode, UpdatedAt: t0, Fingerprint: "uuid-2"},
	}
	if got := Unsynced(realChange, rec); len(got) != 1 {
		t.Errorf("fingerprint changed but not flagged unsynced: %d", len(got))
	}
}
