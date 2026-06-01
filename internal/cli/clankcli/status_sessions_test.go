package clankcli

import (
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/pkg/sessionsync"
)

func pushCTACount(s string) int { return strings.Count(s, "`clank push`") }

// TestRenderStatusReport_Sessions pins the sessions axis: hidden when
// unknown, "✓ Sessions in sync" when current, "• N session(s) not synced"
// otherwise, and a push CTA exactly once even when code is otherwise synced.
func TestRenderStatusReport_Sessions(t *testing.T) {
	t.Parallel()
	base := statusReport{
		WorktreeID: "wt", WorktreeDir: "repo", ActiveRemote: "dev", SignedIn: true,
		WorktreeFromRemote: &daemonclient.WorktreeInfo{ID: "wt"}, HasCheckpoint: true, InSync: true,
	}

	t.Run("unknown axis omits the sessions line", func(t *testing.T) {
		t.Parallel()
		got := stripANSI(renderStatusReport(base)) // SessionsKnown false
		if strings.Contains(strings.ToLower(got), "session") {
			t.Errorf("no sessions line expected:\n%s", got)
		}
		if !strings.Contains(got, "In sync with dev remote") {
			t.Errorf("want compact in-sync summary:\n%s", got)
		}
	})

	t.Run("in sync shows ✓ Sessions in sync, no CTA", func(t *testing.T) {
		t.Parallel()
		r := base
		r.SessionsKnown = true // 0 unsynced
		got := stripANSI(renderStatusReport(r))
		for _, want := range []string{"✓ Commits in sync", "✓ Sessions in sync"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q:\n%s", want, got)
			}
		}
		if pushCTACount(got) != 0 {
			t.Errorf("no CTA expected when everything synced:\n%s", got)
		}
	})

	t.Run("sessions lag while code in sync → exactly one push CTA", func(t *testing.T) {
		t.Parallel()
		r := base
		r.SessionsKnown = true
		r.UnsyncedSessions = 1
		got := stripANSI(renderStatusReport(r))
		if !strings.Contains(got, "✓ Commits in sync") || !strings.Contains(got, "1 session not synced") {
			t.Errorf("want commits-synced + singular session line:\n%s", got)
		}
		if n := pushCTACount(got); n != 1 {
			t.Errorf("want exactly one push CTA, got %d:\n%s", n, got)
		}
	})

	t.Run("plural", func(t *testing.T) {
		t.Parallel()
		r := base
		r.SessionsKnown = true
		r.UnsyncedSessions = 3
		if got := stripANSI(renderStatusReport(r)); !strings.Contains(got, "3 sessions not synced") {
			t.Errorf("want plural:\n%s", got)
		}
	})

	t.Run("rides alongside commit drift with a single CTA", func(t *testing.T) {
		t.Parallel()
		r := base
		r.InSync = false
		r.Drift = driftAhead
		r.DriftAhead = 1
		r.SessionsKnown = true
		r.UnsyncedSessions = 2
		got := stripANSI(renderStatusReport(r))
		for _, want := range []string{"Ahead by 1 commit", "2 sessions not synced"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q:\n%s", want, got)
			}
		}
		if n := pushCTACount(got); n != 1 {
			t.Errorf("want exactly one push CTA, got %d:\n%s", n, got)
		}
	})
}

// TestUnsyncedAgainst pins the diff: a session is unsynced when absent from
// the record or when its UpdatedAt has advanced; unchanged compares equal
// (strict After).
func TestUnsyncedAgainst(t *testing.T) {
	t.Parallel()
	t0, t1 := time.UnixMilli(1000), time.UnixMilli(2000)
	rec := agent.SyncedSessionRecord{Sessions: map[string]agent.SyncedSession{
		"a": {Backend: agent.BackendOpenCode, UpdatedAt: t0},
		"b": {Backend: agent.BackendOpenCode, UpdatedAt: t1},
	}}
	cur := []sessionsync.DiscoveredSession{
		{ExternalID: "a", UpdatedAt: t0},                  // unchanged
		{ExternalID: "b", UpdatedAt: t1.Add(time.Second)}, // newer → unsynced
		{ExternalID: "c", UpdatedAt: t0},                  // new → unsynced
	}
	if got := unsyncedAgainst(cur, rec); len(got) != 2 {
		t.Errorf("unsyncedAgainst = %d sessions, want 2", len(got))
	}
	if got := unsyncedAgainst([]sessionsync.DiscoveredSession{{ExternalID: "a", UpdatedAt: t0}}, rec); len(got) != 0 {
		t.Errorf("unchanged session should be 0, got %d", len(got))
	}
}

// TestUnsyncedAgainst_FingerprintBeatsMtime is the regression test for the
// read-only-`--resume` drift bug: when both sides carry a content
// fingerprint (Claude), an advanced mtime with an UNCHANGED fingerprint must
// NOT count as unsynced, and a changed fingerprint must count even if the
// mtime didn't move.
func TestUnsyncedAgainst_FingerprintBeatsMtime(t *testing.T) {
	t.Parallel()
	t0, later := time.UnixMilli(1000), time.UnixMilli(9999)
	rec := agent.SyncedSessionRecord{Sessions: map[string]agent.SyncedSession{
		"claude-a": {Backend: agent.BackendClaudeCode, UpdatedAt: t0, Fingerprint: "uuid-1"},
	}}

	// mtime bumped by a read-only resume, fingerprint identical → in sync.
	noiseBump := []sessionsync.DiscoveredSession{
		{ExternalID: "claude-a", Backend: agent.BackendClaudeCode, UpdatedAt: later, Fingerprint: "uuid-1"},
	}
	if got := unsyncedAgainst(noiseBump, rec); len(got) != 0 {
		t.Errorf("fingerprint unchanged but flagged unsynced (mtime bump leaked through): %d", len(got))
	}

	// Genuine new turn: fingerprint changed even with the same mtime.
	realChange := []sessionsync.DiscoveredSession{
		{ExternalID: "claude-a", Backend: agent.BackendClaudeCode, UpdatedAt: t0, Fingerprint: "uuid-2"},
	}
	if got := unsyncedAgainst(realChange, rec); len(got) != 1 {
		t.Errorf("fingerprint changed but not flagged unsynced: %d", len(got))
	}
}

// TestSessionLabels pins the -v detail labels: quoted title, or "session
// <id>" when untitled.
func TestSessionLabels(t *testing.T) {
	t.Parallel()
	got := sessionLabels([]sessionsync.DiscoveredSession{
		{ExternalID: "ses_1", Title: "Refactor the auth flow"},
		{ExternalID: "ses_2"},
	})
	want := []string{`"Refactor the auth flow"`, "session ses_2"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("sessionLabels = %v, want %v", got, want)
	}
}

// TestRenderStatusReport_Verbose pins that -v lists the changed session
// titles under the sessions bullet, and that without -v they're hidden.
func TestRenderStatusReport_Verbose(t *testing.T) {
	t.Parallel()
	rep := statusReport{
		WorktreeID: "wt", WorktreeDir: "repo", ActiveRemote: "dev", SignedIn: true,
		WorktreeFromRemote: &daemonclient.WorktreeInfo{ID: "wt"}, HasCheckpoint: true, InSync: true,
		SessionsKnown: true, UnsyncedSessions: 2,
		Verbose:               true,
		UnsyncedSessionLabels: []string{`"Refactor the auth flow"`, `"Debug the flaky test"`},
	}
	got := stripANSI(renderStatusReport(rep))
	for _, want := range []string{`"Refactor the auth flow"`, `"Debug the flaky test"`} {
		if !strings.Contains(got, want) {
			t.Errorf("verbose session detail missing %q:\n%s", want, got)
		}
	}

	// Same report without -v hides the titles (just the count bullet).
	rep.Verbose = false
	got = stripANSI(renderStatusReport(rep))
	if strings.Contains(got, "Refactor the auth flow") {
		t.Errorf("non-verbose output leaked session titles:\n%s", got)
	}
	if !strings.Contains(got, "2 sessions not synced") {
		t.Errorf("count bullet should still show:\n%s", got)
	}
}
